package api

// SCHED-GAP-1745: the cadence surface must carry GOVERNOR context so a client
// can never mistake a cooldown-derived deficit for pending delivery on a
// tasks-admission lane. A tasks-mode lane is board-driven (admission_mode.go
// tasksAdmissionDue): when its board holds dispatchable work the cooldown pin
// is WAIVED, so `86400 / cooldown_pin_s` is not its delivery pace and
// `target − achieved` is not a shortfall — the board decides. Cooldown lanes
// (and tasks lanes that are paced by the pin because they do not own their
// board) keep the existing semantics byte-for-byte: a real derived target and
// a real deficit.
//
// The contract this file pins, additive and explicit:
//   - every row carries `admission_governor`: "tasks_board" | "cooldown_pin"
//   - a tasks_board row reports target_source "tasks_board" with a NULL
//     target and NO `target_deficit_per_day` key — even when a durable pin
//     exists that would otherwise have produced one
//   - a cooldown-paced row keeps its target, deficit and the existing
//     `cooldown_pin` target_source untouched

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

func TestAPI_Cadence_TasksAdmissionGovernorContext(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(cadenceNow))

	ctx := clock.WithClock(context.Background(), clock.NewFixed(cadenceNow))

	// A tasks-mode namespace: member lanes inherit board-driven admission
	// unless the project overrides the mode itself.
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID:            "cad-gov-tasks",
		Weight:        50,
		MaxConcurrent: 2,
		Enabled:       true,
		AdmissionMode: database.AdmissionModeTasks,
		Description:   "SCHED-GAP-1745 tasks-mode namespace",
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	pin := 21600 // 6h pin → would derive 86400/21600 = 4 runs/day
	tasksMode := database.AdmissionModeTasks
	sharedBoard := database.BoardOwnershipShared
	nsID := "cad-gov-tasks"
	// An OWNED empty board for the board-driven lane: the same walk
	// tasksAdmissionDue runs must resolve the board inside the workdir
	// (fail-closed otherwise) — an owned board with zero open rows means
	// the lane is board-driven and simply has nothing admissible now.
	boardWd := t.TempDir()
	boardPath := filepath.Join(boardWd, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(boardPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(boardPath, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile(board): %v", err)
	}
	lanes := []*database.Project{
		{
			// Board-driven: explicit tasks-mode override, an OWNED (empty)
			// board, and a durable pin that must NOT be presented as its
			// cadence target.
			Name: "e-tasks-board", RepoURL: "local://e-tasks-board",
			Workdir: boardWd, Weight: 10, Priority: 5, CooldownS: pin,
			AdmissionMode: tasksMode, DecayRate: 1, Enabled: true,
		},
		{
			// Namespace-inherits tasks mode but the lane reads a SHARED board
			// (SCHED-GAP-141) → the waiver is refused and the pin paces it:
			// time-based, so the pin-derived target stays real.
			Name: "f-tasks-shared", RepoURL: "local://f-tasks-shared",
			Workdir: t.TempDir(), Weight: 10, Priority: 5, CooldownS: pin,
			NamespaceID: &nsID, BoardOwnership: sharedBoard,
			DecayRate: 1, Enabled: true,
		},
		{
			// The classic cooldown lane: unchanged contract end to end.
			Name: "g-cooldown", RepoURL: "local://g-cooldown",
			Workdir: t.TempDir(), Weight: 10, Priority: 5, CooldownS: pin,
			AdmissionMode: database.AdmissionModeCooldown,
			DecayRate:     1, Enabled: true,
		},
	}
	for _, p := range lanes {
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.Name, err)
		}
	}
	// The durable pin is an UPDATE-only attribute (see
	// TestAPI_Cadence_ConfiguredVsAchieved): set it the way the daemon does.
	for _, name := range []string{"e-tasks-board", "f-tasks-shared", "g-cooldown"} {
		if err := database.UpdateProject(ctx, db, name, database.ProjectUpdates{CooldownPinS: &pin}); err != nil {
			t.Fatalf("UpdateProject(pin %s): %v", name, err)
		}
	}
	// Two spawned runs for the cooldown lane inside the window → 2/7 per day,
	// so its deficit arithmetic is proven on a live number, not a zero.
	for i, id := range []string{"cad-gov-1", "cad-gov-2"} {
		tick := &database.Tick{ID: id, ProjectName: "g-cooldown", Status: database.StatusQueued}
		if err := database.CreateTick(ctx, db, tick); err != nil {
			t.Fatalf("CreateTick(%s): %v", id, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE ticks SET spawned_at = ?, status = 'running' WHERE id = ?`,
			cadenceNow.Add(-time.Duration(i+1)*time.Hour).Format(time.RFC3339), id); err != nil {
			t.Fatalf("stamp spawned_at(%s): %v", id, err)
		}
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cadence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/cadence = %d (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Cadence []map[string]interface{} `json:"cadence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	items := make(map[string]map[string]interface{}, len(body.Cadence))
	for _, it := range body.Cadence {
		name, _ := it["name"].(string)
		items[name] = it
	}
	for _, want := range []string{"e-tasks-board", "f-tasks-shared", "g-cooldown"} {
		if _, ok := items[want]; !ok {
			t.Fatalf("lane %q missing from /api/v1/cadence (%v)", want, items)
		}
	}

	num := func(m map[string]interface{}, key string) float64 {
		t.Helper()
		v, ok := m[key].(float64)
		if !ok {
			t.Fatalf("%s = %#v, want a number (item: %v)", key, m[key], m)
		}
		return v
	}
	governor := func(m map[string]interface{}) interface{} {
		t.Helper()
		g, ok := m["admission_governor"]
		if !ok {
			t.Fatalf("admission_governor missing from item %v — every cadence row must name its governor (SCHED-GAP-1745)", m)
		}
		return g
	}

	// e-tasks-board: board-driven. The governor context is the contract; the
	// pin it also carries must not surface as a target or a deficit.
	e := items["e-tasks-board"]
	if got := governor(e); got != "tasks_board" {
		t.Errorf("e-tasks-board admission_governor = %v, want tasks_board", got)
	}
	if got := e["target_source"]; got != "tasks_board" {
		t.Errorf("e-tasks-board target_source = %v, want tasks_board", got)
	}
	if got := e["target_runs_per_day"]; got != nil {
		t.Errorf("e-tasks-board target_runs_per_day = %v, want null (a board-driven lane has no pin-derived target)", got)
	}
	if got := num(e, "target_deficit_per_day"); got != 0 {
		t.Errorf("e-tasks-board target_deficit_per_day = %v, want 0 (a no-opinion zero, same as source=none — the pin-derived deficit is not served)", got)
	}

	// f-tasks-shared: paced by the pin (shared board ⇒ waiver refused), so
	// the pre-1745 numbers survive exactly.
	f := items["f-tasks-shared"]
	if got := governor(f); got != "cooldown_pin" {
		t.Errorf("f-tasks-shared admission_governor = %v, want cooldown_pin", got)
	}
	if got := f["target_source"]; got != "cooldown_pin" {
		t.Errorf("f-tasks-shared target_source = %v, want cooldown_pin", got)
	}
	if got := num(f, "target_runs_per_day"); math.Abs(got-4) > 1e-9 {
		t.Errorf("f-tasks-shared target_runs_per_day = %v, want 4", got)
	}
	if got := num(f, "target_deficit_per_day"); math.Abs(got-4) > 1e-9 {
		t.Errorf("f-tasks-shared target_deficit_per_day = %v, want 4", got)
	}

	// g-cooldown: the cooldown contract is byte-for-byte unchanged.
	g := items["g-cooldown"]
	if got := governor(g); got != "cooldown_pin" {
		t.Errorf("g-cooldown admission_governor = %v, want cooldown_pin", got)
	}
	if got := g["target_source"]; got != "cooldown_pin" {
		t.Errorf("g-cooldown target_source = %v, want cooldown_pin", got)
	}
	if got := num(g, "achieved_runs_per_day"); math.Abs(got-2.0/7.0) > 1e-9 {
		t.Errorf("g-cooldown achieved_runs_per_day = %v, want 2/7", got)
	}
	if got := num(g, "target_deficit_per_day"); math.Abs(got-(4-2.0/7.0)) > 1e-9 {
		t.Errorf("g-cooldown target_deficit_per_day = %v, want 4-2/7", got)
	}
}

/*
ch:trace row=SCHED-GAP-1745 spec=docs/api.md#cadence wave=none:serial-worker test=internal/api/server_cadence_test.go doc=docs/api.md prompt=none:foreman-dispatch-brief evidence=none:pending-commit witness=none:worker-local verdict=none:foreman-judge commit=none:pending-implementation memory=none:not-written
STATUS: complete
TIER: T3 — invoked: go test ./internal/api (PASS: TestAPI_Cadence_ConfiguredVsAchieved, TestAPI_Cadence_TasksAdmissionGovernorContext), go test ./internal/database (PASS: TestEffectiveCadenceTargetNoOpinionWithoutOverrideOrPin, TestCadenceTargetRoundTripAndAchievedRate, TestLoadCadenceRates_WindowFollowsClockSeam), go test ./internal/mcp (PASS: TestMCP_Cadence_GovernorParity + cadence registry arms), go test ./internal/scheduler (PASS: cadence/cooldown suites); go build ./... 0 errors; go vet ./... clean; gofmt -l on changed files: empty. Acceptance grep `git grep -n tasksAdmissionDue -- internal` hits internal/scheduler/admission_mode.go (tasksAdmissionDue, the board-driven admission branch).
EVIDENCE: commit:none:pending-commit (implementation + tests + docs + this marker in one commit on wt/SCHED-GAP-1745; SHA bound by foreman trace follow-up)
*/
