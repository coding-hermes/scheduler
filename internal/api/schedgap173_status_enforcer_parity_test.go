package api

// SCHED-GAP-173 — the /api/v1/status auto_disable_armed surface must report
// what the AUTO-DISABLE ENFORCER would actually do.
//
// The enforcer (internal/scheduler CheckFailureRateAutoDisable) excludes
// harness-class failures — a tick refused by a draining gateway never reached
// the project, so it says nothing about the project's health — while the
// status surface used to count every failed/timeout row. Measured on the live
// fleet 2026-09-19: heading read failure_rate=0.91 / auto_disable_armed=true
// while 98 of its last 100 ticks were "gateway unreachable ... HTTP 503"
// refusals, i.e. the enforcer's own verdict on that window was ~0.02. A
// perfectly healthy lane was permanently advertised as "about to be parked",
// and PM/QA automation + the SCHED-GAP-170 per-lane-decay read inherited the
// lie.
//
// These tests pin the parity three ways:
//  1. the real HTTP surface (GET /api/v1/status) against the REAL enforcer
//     run on the same DB and the same policy numbers (the load-bearing one);
//  2. the exact counts/rate/armed for a fixture mixing harness-class and
//     project-attributable failures (including the pre-fix values, so the
//     assertions fail loudly if naive counting comes back);
//  3. a structural guard that the marker list exists in exactly one place
//     (internal/scheduler/failureclass.go) and that the surface calls it
//     instead of re-declaring a private classifier.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

const (
	// gap173HarnessErr is the live gateway-drain refusal text recorded on the
	// fleet host (2026-09-19) — 98 of heading's last 100 ticks carried it.
	gap173HarnessErr = `gateway unreachable and exec fallback disabled: gateway POST: HTTP 503: invalid_request_error: Gateway is draining existing work; retry shortly.`

	// gap173ProjectErr is a project-attributable failure: the tick reached the
	// project and the project's work failed. It must always count.
	gap173ProjectErr = `exit status 2: judge rejected the criteria evidence`
)

// The auto-disable policy every assertion in this file uses. The same numbers
// feed the surface (ResolvedConfig) and the enforcer (NewAlertEscalatorWithPolicy)
// — exactly as the daemon wires them from one resolved config.
const (
	gap173Threshold = 0.5
	gap173Window    = 100
	gap173MinTicks  = 5
)

// gap173Expect is what the status surface must report for a fixture project
// after the fix, plus the pre-fix ("naive") armed verdict for the same data —
// the lie this task removes. naiveArmed doubles as the RED witness: if the
// harness-class rows start counting again, armed flips back to naiveArmed and
// the assertions below name the divergence.
type gap173Expect struct {
	failed     int
	total      int
	rate       float64
	armed      bool
	naiveArmed bool
}

// gap173Projects is the fixture roster. Row shapes are built by gap173FixtureDB.
var gap173Projects = map[string]gap173Expect{
	// The heading shape: 98 harness-class failures + 2 real completions.
	// Pre-fix 98/100 = 0.98 → armed; post-fix 0/2 = 0.0 → not armed.
	"harnessnoise": {failed: 0, total: 2, rate: 0, armed: false, naiveArmed: true},
	// Mostly noise with one real fault: 1/40 = 0.025 — the "~0.02" enforcer
	// verdict this task measured, far below the 0.5 threshold.
	"mostlynoise": {failed: 1, total: 40, rate: 0.025, armed: false, naiveArmed: true},
	// The breaker must still fire on project-side faults: 5/10 = 0.5 >= 0.5.
	"realbad": {failed: 5, total: 10, rate: 0.5, armed: true, naiveArmed: true},
	// A window that is entirely harness-class: the entry stays (total 0,
	// rate 0, not armed) instead of vanishing — a harness-blocked lane must
	// remain visible, and "absent" would also be indistinguishable from a
	// lane with no ticks at all.
	"allharness": {failed: 0, total: 0, rate: 0, armed: false, naiveArmed: true},
	// A clean lane, unchanged by the fix.
	"cleanlane": {failed: 0, total: 20, rate: 0, armed: false, naiveArmed: false},
	// The enforcer's predicate is status == "failed" AND harness-class, so a
	// TIMEOUT carrying harness text is still counted. The surface must mirror
	// that exactly — a sloppier "skip any harness-class row" implementation
	// would report armed=false while the enforcer parked the lane.
	"timeoutnoise": {failed: 10, total: 10, rate: 1, armed: true, naiveArmed: true},
}

// gap173InsertTick inserts one completed tick row, with or without an error
// string (the existing package helpers do not carry ticks.error).
func gap173InsertTick(t *testing.T, db *sql.DB, id, project, status, errText string, completedAt time.Time) {
	t.Helper()
	ts := completedAt.Format(time.RFC3339)
	var err error
	if errText == "" {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, project, status, ts, ts, ts)
	} else {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, error, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, project, status, errText, ts, ts, ts)
	}
	if err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// gap173FixtureDB creates an in-memory DB holding every project in
// gap173Projects with the exact row mix described there.
func gap173FixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	now := time.Now()

	// seq keeps spawned_at strictly increasing so the windowed
	// ORDER BY spawned_at DESC LIMIT picks exactly the rows inserted here.
	seq := 0
	insert := func(project, status, errText string) {
		seq++
		gap173InsertTick(t, db, fmt.Sprintf("%s-%03d", project, seq), project, status, errText,
			now.Add(-time.Duration(seq)*time.Second))
	}

	rows := []struct {
		project string
		status  string
		errText string
		count   int
	}{
		{"harnessnoise", "failed", gap173HarnessErr, 98},
		{"harnessnoise", "completed", "", 2},

		{"mostlynoise", "failed", gap173HarnessErr, 60},
		{"mostlynoise", "completed", "", 39},
		{"mostlynoise", "failed", gap173ProjectErr, 1},

		{"realbad", "failed", gap173HarnessErr, 90},
		{"realbad", "failed", gap173ProjectErr, 5},
		{"realbad", "completed", "", 5},

		{"allharness", "failed", gap173HarnessErr, 100},

		{"cleanlane", "completed", "", 20},

		{"timeoutnoise", "timeout", gap173HarnessErr, 10},
	}
	created := map[string]bool{}
	for _, r := range rows {
		if created[r.project] {
			continue
		}
		created[r.project] = true
		mustCreateHelperTestProject(t, db, r.project)
	}
	for _, r := range rows {
		for i := 0; i < r.count; i++ {
			insert(r.project, r.status, r.errText)
		}
	}
	return db
}

// gap173EnforcerRun runs the REAL enforcer over db and returns, per project,
// whether it DISABLED the project (enabled=1 → 0) during the run.
func gap173EnforcerRun(t *testing.T, db *sql.DB, failureRate float64, window, minTicks int) map[string]bool {
	t.Helper()
	esc := scheduler.NewAlertEscalatorWithPolicy(db, scheduler.NewEventLogger(db), failureRate, window, minTicks)
	if err := esc.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}
	rows, err := db.Query(`SELECT name, enabled FROM projects`)
	if err != nil {
		t.Fatalf("query projects: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var enabled int
		if err := rows.Scan(&name, &enabled); err != nil {
			t.Fatalf("scan project: %v", err)
		}
		out[name] = enabled == 0
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate projects: %v", err)
	}
	return out
}

// gap173Validate checks the enforcer actually saw the whole fixture — a
// silently empty projects table would make every parity assertion vacuous.
func gap173AssertFixturePresent(t *testing.T, disabled map[string]bool) {
	t.Helper()
	for name := range gap173Projects {
		if _, ok := disabled[name]; !ok {
			t.Fatalf("fixture project %q missing from the projects table — the parity assertions would be vacuous", name)
		}
	}
}

var gap173ReasonRe = regexp.MustCompile(`failure rate ([0-9.]+)% \((\d+)/(\d+) ticks, window (\d+), threshold ([0-9.]+)\)`)

// gap173DisabledReasonCounts parses the counts the ENFORCER itself wrote into
// projects.disabled_reason when it parked a lane — an enforcer-side number
// independent of the surface's own arithmetic.
func gap173DisabledReasonCounts(t *testing.T, db *sql.DB, name string) (failed, total int) {
	t.Helper()
	var reason string
	if err := db.QueryRow(`SELECT COALESCE(disabled_reason, '') FROM projects WHERE name = ?`, name).Scan(&reason); err != nil {
		t.Fatalf("read disabled_reason for %s: %v", name, err)
	}
	m := gap173ReasonRe.FindStringSubmatch(reason)
	if m == nil {
		t.Fatalf("enforcer disabled %s but its disabled_reason does not carry counts: %q", name, reason)
	}
	failed, err := strconv.Atoi(m[2])
	if err != nil {
		t.Fatalf("parse disabled_reason failed-count for %s (%q): %v", name, reason, err)
	}
	total, err = strconv.Atoi(m[3])
	if err != nil {
		t.Fatalf("parse disabled_reason total-count for %s (%q): %v", name, reason, err)
	}
	return failed, total
}

// TestSCHEDGAP173_StatusEndpointArmedMatchesEnforcer is the canonical parity
// test: the real GET /api/v1/status payload is compared against the real
// CheckFailureRateAutoDisable verdict on the same DB and the same policy, and
// the enforcer's own written counts are compared against the surface's.
func TestSCHEDGAP173_StatusEndpointArmedMatchesEnforcer(t *testing.T) {
	db := gap173FixtureDB(t)
	defer db.Close()

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	srv := NewServer(db, loop)
	srv.SetFailureWindow(gap173Window)
	srv.SetResolvedConfig(ResolvedConfig{
		AutoDisableFailureRate: gap173Threshold,
		AutoDisableWindow:      gap173Window,
		AutoDisableMinTicks:    gap173MinTicks,
		FailureWindow:          gap173Window,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /api/v1/status: %v", err)
	}
	raw, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}

	// The independent verdict: run the enforcer itself on the same window.
	disabled := gap173EnforcerRun(t, db, gap173Threshold, gap173Window, gap173MinTicks)
	gap173AssertFixturePresent(t, disabled)

	type surfaceCounts struct {
		failed, total int
		rate          float64
		armed         bool
	}
	surface := map[string]surfaceCounts{}
	for name, entry := range raw {
		m, ok := entry.(map[string]interface{})
		if !ok {
			t.Errorf("projects_failure_rates[%s] is %T, want object", name, entry)
			continue
		}
		f, okF := m["failed"].(float64)
		tot, okT := m["total"].(float64)
		r, okR := m["failure_rate"].(float64)
		armed, okA := m["auto_disable_armed"].(bool)
		if !okF || !okT || !okR || !okA {
			t.Errorf("projects_failure_rates[%s] has unexpected JSON shape: %v", name, m)
			continue
		}
		surface[name] = surfaceCounts{failed: int(f), total: int(tot), rate: r, armed: armed}
	}

	for name, want := range gap173Projects {
		got, ok := surface[name]
		if !ok {
			t.Errorf("SCHED-GAP-173 DIVERGENCE: %s is missing from /api/v1/status projects_failure_rates, but the enforcer still evaluates it (verdict: disabled=%v)",
				name, disabled[name])
			continue
		}
		if got.failed != want.failed || got.total != want.total || got.rate != want.rate {
			t.Errorf("SCHED-GAP-173 RATE DIVERGENCE on %s: status surface reports failed=%d total=%d rate=%v, want failed=%d total=%d rate=%v — harness-class failures must be excluded from BOTH counters, exactly as CheckFailureRateAutoDisable excludes them",
				name, got.failed, got.total, got.rate, want.failed, want.total, want.rate)
		}
		if got.armed != want.armed {
			t.Errorf("SCHED-GAP-173 ARMED DIVERGENCE on %s: auto_disable_armed=%v, want %v (counting every failed/timeout row would report %v) with failed=%d total=%d rate=%v under threshold=%.2f min_ticks=%d",
				name, got.armed, want.armed, want.naiveArmed, got.failed, got.total, got.rate, gap173Threshold, gap173MinTicks)
		}
		// The load-bearing invariant: armed == what the enforcer would do.
		if got.armed != disabled[name] {
			enforcerVerdict := "left the project ENABLED"
			if disabled[name] {
				enforcerVerdict = "DISABLED the project"
			}
			t.Errorf("SCHED-GAP-173 PARITY DIVERGENCE on %s: status surface auto_disable_armed=%v (failed=%d total=%d rate=%v) but the enforcer %s on the same window — the status surface must report what CheckFailureRateAutoDisable would actually do",
				name, got.armed, got.failed, got.total, got.rate, enforcerVerdict)
		}
	}

	// For every lane the enforcer actually parked, its OWN counts (written
	// into disabled_reason) must equal the surface's.
	parked := 0
	for name, wasDisabled := range disabled {
		if !wasDisabled {
			continue
		}
		parked++
		want, known := gap173Projects[name]
		if !known {
			t.Errorf("enforcer disabled an unexpected project %q (not in the fixture roster)", name)
			continue
		}
		if !want.armed {
			t.Errorf("enforcer disabled %s but the fixture expects armed=false — the enforcer counted rows the surface excluded (or vice versa)", name)
		}
		failed, total := gap173DisabledReasonCounts(t, db, name)
		got := surface[name]
		if failed != got.failed || total != got.total {
			t.Errorf("SCHED-GAP-173 COUNT DIVERGENCE on %s: enforcer wrote %d/%d ticks (disabled_reason) but the status surface reports failed=%d total=%d",
				name, failed, total, got.failed, got.total)
		}
	}
	if parked == 0 {
		t.Fatalf("enforcer disabled nothing — the fixture is not exercising the breaker (policy threshold=%.2f min_ticks=%d)", gap173Threshold, gap173MinTicks)
	}
}

// TestSCHEDGAP173_ClassifierIsSingleAuthority fails the moment the harness
// marker list is duplicated into a second surface (the exact regression this
// task removes) or the status surface stops calling the exported classifier.
// Mirrors the house pattern of TestADVR10_NoSecondHardcodedWeeklyCeiling.
func TestSCHEDGAP173_ClassifierIsSingleAuthority(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // .../internal/api/x_test.go -> repo root

	markers := []string{
		"gateway unreachable",
		"gateway is draining",
		"gateway_auth_error",
		"invalid gateway api key",
		"connection refused",
		"exec fallback disabled",
		"aborted by graceful shutdown",
	}
	shared := filepath.Join("internal", "scheduler", "failureclass.go")

	// (a) The shared file must carry the complete marker set.
	sharedPath := filepath.Join(repoRoot, shared)
	sharedSrc, err := os.ReadFile(sharedPath)
	if err != nil {
		t.Fatalf("read %s: %v", shared, err)
	}
	for _, m := range markers {
		// SCHED-GAP-1608: the abort wording may live as the named constant
		// OrphanAbortMarker (same file) instead of an inline list literal —
		// the single-authority requirement is "declared HERE, once", which
		// the const satisfies.
		if !strings.Contains(string(sharedSrc), `"`+m+`",`) &&
			!strings.Contains(string(sharedSrc), `"`+m+`"`) {
			t.Errorf("%s does not declare marker %q — the shared classifier list is incomplete", shared, m)
		}
	}

	// (b) The status surface must CALL it, not re-declare one, and the old
	//     private copy must be gone from alert_escalation.go.
	surfacePath := filepath.Join(repoRoot, "internal", "api", "server_helpers.go")
	surfaceSrc, err := os.ReadFile(surfacePath)
	if err != nil {
		t.Fatalf("read internal/api/server_helpers.go: %v", err)
	}
	if !strings.Contains(string(surfaceSrc), "scheduler.HarnessFailure(") &&
		!strings.Contains(string(surfaceSrc), "scheduler.FailureIsLaneAttributableT(") {
		t.Error("internal/api/server_helpers.go does not call scheduler.HarnessFailure nor scheduler.FailureIsLaneAttributableT — the status surface must classify with the shared classifier (SCHED-GAP-173; SCHED-GAP-1608 moved the call to the tuple predicate in internal/scheduler/orphan_exclusion.go, which itself calls HarnessFailure)")
	}
	if strings.Contains(string(surfaceSrc), "func harnessFailure(") {
		t.Error("internal/api/server_helpers.go declares its own harnessFailure — the classifier must have exactly one implementation")
	}
	for _, m := range markers {
		if strings.Contains(string(surfaceSrc), `"`+m+`",`) {
			t.Errorf("internal/api/server_helpers.go declares harness marker %q — the marker list belongs to internal/scheduler/failureclass.go alone", m)
		}
	}
	escPath := filepath.Join(repoRoot, "internal", "scheduler", "alert_escalation.go")
	escSrc, err := os.ReadFile(escPath)
	if err != nil {
		t.Fatalf("read internal/scheduler/alert_escalation.go: %v", err)
	}
	for _, m := range markers {
		if strings.Contains(string(escSrc), `"`+m+`",`) {
			t.Errorf("internal/scheduler/alert_escalation.go still declares harness marker %q — the list moved to failureclass.go", m)
		}
	}

	// (c) No third marker list anywhere under internal/ or cmd/.
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(repoRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			hits := 0
			for _, m := range markers {
				if strings.Contains(string(b), `"`+m+`",`) {
					hits++
				}
			}
			if hits >= 2 {
				rel, _ := filepath.Rel(repoRoot, path)
				if rel != shared {
					offenders = append(offenders, fmt.Sprintf("%s (%d markers)", rel, hits))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("duplicate harness-failure marker list(s) found: %s — the only authority is %s (SCHED-GAP-173)",
			strings.Join(offenders, ", "), shared)
	}

	// (d) The exported classifier behaves as both surfaces require.
	if !scheduler.HarnessFailure(gap173HarnessErr) {
		t.Errorf("scheduler.HarnessFailure(%q) = false, want true", gap173HarnessErr)
	}
	if scheduler.HarnessFailure(gap173ProjectErr) {
		t.Errorf("scheduler.HarnessFailure(%q) = true, want false (project-attributable failures must count)", gap173ProjectErr)
	}
}
