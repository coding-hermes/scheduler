package dashboard

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	scheduler "github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1603: the Next Tick column must reflect the lane's EFFECTIVE
// admission mode (project override → namespace default → cooldown, the
// same resolution rule the scheduler applies in admission_mode.go). A
// tasks-mode lane never renders a cooldown countdown — it renders
// board-driven state ("due — N board rows open" / "idle — board drained" /
// "tasks admission" when the board cannot be read); a cooldown lane keeps
// the countdown.

// writeTasksMd writes a markdown board at <workdir>/.coding-hermes/tasks.md —
// the SAME file readBoardProgress reads for the Progress column.
func writeTasksMd(t *testing.T, workdir, board string) {
	t.Helper()
	dir := filepath.Join(workdir, ".coding-hermes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"), []byte(board), 0o644); err != nil {
		t.Fatalf("WriteFile tasks.md: %v", err)
	}
}

// writeJSONLBoard writes a JSONL board at
// <workdir>/.coding-hermes/board/tasks.jsonl — the board the scheduler's
// admission path reads (used by the parity test to drive the real packer).
func writeJSONLBoard(t *testing.T, workdir string, rows ...string) {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile tasks.jsonl: %v", err)
	}
}

// modeLane inserts one project row for the render tests: name, admission
// mode override ("" = inherit), namespace id ("" = none), workdir, cooldown.
// last_tick_completed is pinned 20 minutes in the past so a (wrong)
// countdown path would render a visible "in …" and a past-due cooldown lane
// renders "due now".
func modeLane(t *testing.T, db *sql.DB, name, projMode, nsID, workdir string, cooldownS int) {
	t.Helper()
	p := &database.Project{
		Name:      name,
		RepoURL:   "local://" + name,
		Workdir:   workdir,
		Weight:    10,
		Priority:  5,
		CooldownS: cooldownS,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}
	if projMode != "" {
		p.AdmissionMode = projMode
	}
	if nsID != "" {
		ns := nsID
		p.NamespaceID = &ns
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
	past := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	if _, err := db.ExecContext(context.Background(),
		`UPDATE projects SET last_tick_completed = ? WHERE name = ?`, past, name); err != nil {
		t.Fatalf("pin last_tick_completed for %s: %v", name, err)
	}
}

// mustNamespace inserts one namespace row with the given default admission mode.
func mustNamespace(t *testing.T, db *sql.DB, id, mode string) {
	t.Helper()
	if err := database.CreateNamespace(context.Background(), db, &database.Namespace{
		ID: id, Weight: 100, Reserved: 1, HardCap: 100, Enabled: true,
		MaxConcurrent: 4, AdmissionMode: mode,
	}); err != nil {
		t.Fatalf("CreateNamespace %s: %v", id, err)
	}
}

// fleetRowByName runs collect() and returns the one row for the project.
func fleetRowByName(t *testing.T, g *Generator, name string) FleetRow {
	t.Helper()
	data := g.collect(context.Background())
	for _, r := range data.Projects {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("project %q not found in collect() output", name)
	return FleetRow{}
}

// isCountdown reports whether the rendered Next Tick value is a cooldown
// countdown ("in Nm Ns") — the string class that must NEVER appear for a
// tasks-admission lane.
func isCountdown(s string) bool {
	return strings.HasPrefix(s, "in ") && strings.HasSuffix(s, "s")
}

// T1: a tasks-admission lane with open board work renders NO countdown —
// it renders the board-driven due state naming the open row count.
func TestNextTick_TasksMode_OpenBoardRendersNoCountdown(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	wd := t.TempDir()
	writeTasksMd(t, wd, `# Board

## Active

| ID | Task |
|----|------|
| GAP-1 | one |
| GAP-2 | two |

## Completed

| ID | Task | Commit |
|----|------|--------|
| GAP-3 | three | abc123 |

## [ ] NEVER-DONE — audit

Never counted.
`)
	mustNamespace(t, db, "foreman-ns", database.AdmissionModeTasks)
	modeLane(t, db, "tasks-open", "", "foreman-ns", wd, 21600)

	g := NewGenerator(db, nil)
	row := fleetRowByName(t, g, "tasks-open")
	if isCountdown(row.NextTickIn) {
		t.Fatalf("T1 FAIL: tasks lane with open board rendered a cooldown countdown %q", row.NextTickIn)
	}
	if row.NextTickIn != "due — 2 board rows open" {
		t.Fatalf("T1 FAIL: tasks lane with open work = %q, want %q", row.NextTickIn, "due — 2 board rows open")
	}
}

// T2: a tasks-admission lane with a drained board renders the idle state.
func TestNextTick_TasksMode_DrainedBoardRendersIdle(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	wd := t.TempDir()
	writeTasksMd(t, wd, `# Board

## Completed

| ID | Task | Commit |
|----|------|--------|
| GAP-1 | one | abc123 |
| GAP-2 | two | def456 |
`)
	modeLane(t, db, "tasks-drained", database.AdmissionModeTasks, "", wd, 21600)

	g := NewGenerator(db, nil)
	row := fleetRowByName(t, g, "tasks-drained")
	if isCountdown(row.NextTickIn) {
		t.Fatalf("T2 FAIL: tasks lane with drained board rendered a countdown %q", row.NextTickIn)
	}
	if row.NextTickIn != "idle — board drained" {
		t.Fatalf("T2 FAIL: drained tasks lane = %q, want %q", row.NextTickIn, "idle — board drained")
	}
}

// T3: a cooldown-admission lane keeps today's countdown behavior unchanged —
// open board work must NOT leak the tasks-mode label into its cell.
func TestNextTick_CooldownMode_KeepsCountdown(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	wd := t.TempDir()
	writeTasksMd(t, wd, `# Board

## Active

| ID | Task |
|----|------|
| GAP-1 | pending work |

## Completed

| ID | Task | Commit |
|----|------|--------|
| GAP-2 | done | abc123 |
`)
	modeLane(t, db, "cool-pin", database.AdmissionModeCooldown, "", wd, 900)

	g := NewGenerator(db, nil)
	row := fleetRowByName(t, g, "cool-pin")
	// last tick completed 20m ago against a 900s (15m) cooldown → past due.
	if row.NextTickIn != "due now" {
		t.Fatalf("T3 FAIL: cooldown lane = %q, want %q (countdown behavior must be unchanged)", row.NextTickIn, "due now")
	}
}

// T4: project detail page — a tasks lane's Next Tick card renders the
// board-driven state, not a countdown; a cooldown lane keeps the countdown.
func TestNextTick_ProjectDetailByMode(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// One board per lane — CreateProject enforces case-insensitive workdir
	// uniqueness (two enabled lanes may never share a board directory).
	tasksWd := t.TempDir()
	writeTasksMd(t, tasksWd, `# Board

## Active

| ID | Task |
|----|------|
| GAP-1 | one |

## Completed

| ID | Task | Commit |
|----|------|--------|
| GAP-2 | two | abc123 |
`)
	coolWd := t.TempDir()
	writeTasksMd(t, coolWd, `# Board

## Active

| ID | Task |
|----|------|
| GAP-9 | cooldown lane board work |

## Completed

| ID | Task | Commit |
|----|------|--------|
| GAP-8 | done | def456 |
`)
	mustNamespace(t, db, "foreman-ns", database.AdmissionModeTasks)
	modeLane(t, db, "detail-tasks", "", "foreman-ns", tasksWd, 21600)
	modeLane(t, db, "detail-cool", database.AdmissionModeCooldown, "", coolWd, 900)

	g := NewGenerator(db, nil)
	var buf strings.Builder
	if err := g.GenerateProjectDetail(&buf, "detail-tasks"); err != nil {
		t.Fatalf("GenerateProjectDetail(detail-tasks): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "due — 1 board rows open") {
		t.Errorf("T4 FAIL: tasks lane detail page missing board-driven state; snippet: %s", snippetFrom(out, "Next Tick"))
	}
	if strings.Contains(out, "in 5h") {
		t.Errorf("T4 FAIL: tasks lane detail page renders a 6h countdown: %q", snippetFrom(out, "in 5h"))
	}

	buf.Reset()
	if err := g.GenerateProjectDetail(&buf, "detail-cool"); err != nil {
		t.Fatalf("GenerateProjectDetail(detail-cool): %v", err)
	}
	out = buf.String()
	if !strings.Contains(out, "due now") {
		t.Errorf("T4 FAIL: cooldown lane detail page missing %q", "due now")
	}
}

// snippetFrom returns a short window around the first occurrence of marker.
func snippetFrom(haystack, marker string) string {
	i := strings.Index(haystack, marker)
	if i < 0 {
		return marker + " not found"
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	end := i + 160
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}

// T5: resolution-rule parity — the dashboard's replicated precedence
// (project → namespace → cooldown) must produce the SAME effective mode as
// the scheduler's real resolver for a table of cases. The scheduler's
// admissionModeFor is unexported (and scheduler production code is
// off-limits here), so parity is pinned BEHAVIORALLY through the exported
// packer surface: a lane inside its cooldown pin (4h pin, last tick 1h ago)
// with non-perpetual pending work is admitted iff its effective mode is
// tasks. The replica and the packer must agree on every case.
func TestAdmissionModeReplica_MatchesSchedulerResolver(t *testing.T) {
	cases := []struct {
		name     string
		projMode string
		nsID     string // namespace the project references ("" = none)
		nsMode   string // default of the LISTED namespace
		nsListed bool   // the referenced namespace row actually exists
		wantMode string
	}{
		{"namespace tasks, project inherits", "", "ns-a", database.AdmissionModeTasks, true, database.AdmissionModeTasks},
		{"namespace tasks, project pins cooldown", database.AdmissionModeCooldown, "ns-a", database.AdmissionModeTasks, true, database.AdmissionModeCooldown},
		{"namespace cooldown, project overrides tasks", database.AdmissionModeTasks, "ns-b", database.AdmissionModeCooldown, true, database.AdmissionModeTasks},
		{"namespace cooldown, project inherits", "", "ns-b", database.AdmissionModeCooldown, true, database.AdmissionModeCooldown},
		{"no namespace, project override tasks", database.AdmissionModeTasks, "", "", false, database.AdmissionModeTasks},
		{"no namespace, no override", "", "", "", false, database.AdmissionModeCooldown},
		{"project references a namespace that is not loaded", "", "ghost-ns", database.AdmissionModeTasks, false, database.AdmissionModeCooldown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Replica side: exactly the inputs collect() assembles.
			nsModes := map[string]string{}
			var namespaces []database.Namespace
			if tc.nsListed {
				nsModes[tc.nsID] = tc.nsMode
				namespaces = []database.Namespace{{
					ID: tc.nsID, Weight: 100, Reserved: 1, HardCap: 100, Enabled: true,
					MaxConcurrent: 4, AdmissionMode: tc.nsMode,
				}}
			}
			got := effectiveAdmissionModeFor(tc.projMode, tc.nsID, nsModes)
			if got != tc.wantMode {
				t.Fatalf("replica: effectiveAdmissionModeFor(%q, %q, %v) = %q, want %q",
					tc.projMode, tc.nsID, nsModes, got, tc.wantMode)
			}

			// Real side: observable via the exported packer — a lane with
			// pending work inside its cooldown pin is admitted iff its
			// effective mode is tasks (admission_mode.go waiver).
			wd := t.TempDir()
			writeJSONLBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)
			p := database.Project{
				Name: "parity", RepoURL: "local://parity", Workdir: wd,
				Weight: 10, Priority: 5, CooldownS: 14400, DecayRate: 1.0,
				Enabled: true, AdmissionMode: tc.projMode,
				CreatedAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if tc.nsID != "" {
				ns := tc.nsID
				p.NamespaceID = &ns
			}
			mp := scheduler.NewMultiPoolPacker(400, 8, nil)
			res := mp.Pack([]database.Project{p}, namespaces,
				scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10),
				map[string]time.Time{"parity": time.Now().UTC().Add(-time.Hour)}, nil, time.Now().UTC())
			selected := false
			for _, s := range res.Projects {
				if s.Name == "parity" {
					selected = true
				}
			}
			if selected != (got == database.AdmissionModeTasks) {
				t.Fatalf("parity with scheduler: replica says %q, packer selected=%v (want selected iff mode==tasks)",
					got, selected)
			}
		})
	}
}

// T6: a tasks lane whose workdir is unavailable/empty renders "tasks
// admission" rather than a countdown.
func TestNextTick_TasksMode_UnreadableBoardRendersAdmissionLabel(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// workdir exists but has no board file at all.
	modeLane(t, db, "tasks-noboard", database.AdmissionModeTasks, "", t.TempDir(), 21600)

	g := NewGenerator(db, nil)
	row := fleetRowByName(t, g, "tasks-noboard")
	if isCountdown(row.NextTickIn) {
		t.Fatalf("T6 FAIL: tasks lane with no board rendered a countdown %q", row.NextTickIn)
	}
	if row.NextTickIn != "tasks admission" {
		t.Fatalf("T6 FAIL: tasks lane with no board = %q, want %q", row.NextTickIn, "tasks admission")
	}
}

// T7: sorting — tasks lanes sort sensibly within the "next" column:
// "due — …" ranks with "due now", "idle — …" sinks last, and a countdown
// lane stays between them.
func TestNextTickSort_ModesRankSensibly(t *testing.T) {
	rows := []FleetRow{
		{Name: "cool-waiting", NextTickIn: "in 3h 12m 40s"},
		{Name: "tasks-idle", NextTickIn: "idle — board drained"},
		{Name: "cool-running", NextTickIn: "running"},
		{Name: "tasks-due", NextTickIn: "due — 4 board rows open"},
		{Name: "cool-due", NextTickIn: "due now"},
		{Name: "tasks-unknown", NextTickIn: "tasks admission"},
	}
	less := fleetProjectSortable("next")
	sorted := append([]FleetRow(nil), rows...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && less(sorted[j], sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	gotOrder := make([]string, len(sorted))
	for i, r := range sorted {
		gotOrder[i] = r.Name
	}
	wantOrder := []string{"cool-running", "cool-due", "tasks-due", "cool-waiting", "tasks-idle", "tasks-unknown"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("T7 FAIL: order = %v, want %v", gotOrder, wantOrder)
		}
	}
}
