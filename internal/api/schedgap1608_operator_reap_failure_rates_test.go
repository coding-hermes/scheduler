package api

// SCHED-GAP-1608 — the /api/v1/status failure-rate surface (and the
// auto_disable_armed verdict it drives) must exclude operator-induced drain
// reaps — failed/timeout rows whose orphan_reason is drain_timeout or
// operator_restart, or whose only signature is the legacy "aborted by
// graceful shutdown" error text — from BOTH counters, exactly as
// CheckFailureRateAutoDisable now does through the shared tuple predicate
// (internal/scheduler/orphan_exclusion.go). A genuine project-attributable
// failure keeps counting.
//
// Measured evidence for this row: a busy fleet held 10-12 live heartbeating
// ticks; a controlled restart eventually reaped 40 ticks as
// failed(drain_timeout) — none of which ever reached their projects.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

const (
	gap1608AbortText = "aborted by graceful shutdown — drain timed out with tick in flight"
	gap1608RealErr   = "exit status 2: judge rejected the criteria evidence"
)

// gap1608InsertTick seeds one terminal tick row with explicit orphan stamp.
func gap1608InsertTick(t *testing.T, db *sql.DB, id, project, status, orphan, errText string, completedAt time.Time) {
	t.Helper()
	ts := completedAt.Format(time.RFC3339)
	var err error
	if errText == "" && orphan == "" {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, project, status, ts, ts, ts)
	} else if errText == "" {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, orphaned_at, orphan_reason, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, project, status, ts, orphan, ts, ts, ts)
	} else {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, error, orphaned_at, orphan_reason, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, project, status, errText, ts, orphan, ts, ts, ts)
	}
	if err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// gap1608RunStatus drives the REAL /api/v1/status surface over db (the same
// wiring TestSCHEDGAP173 uses) and returns the projects_failure_rates map.
func gap1608RunStatus(t *testing.T, db *sql.DB) map[string]interface{} {
	t.Helper()
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	srv := NewServer(db, loop)
	srv.SetResolvedConfig(gap1608ResolvedConfig())
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
	rates, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}
	return rates
}

// gap1608ResolvedConfig is the auto-disable policy both the surface and the
// enforcer run under in these tests — the same numbers, from one place, the
// way the daemon wires them from one resolved config.
func gap1608ResolvedConfig() ResolvedConfig {
	return ResolvedConfig{
		AutoDisableFailureRate: 0.5,
		AutoDisableWindow:      100,
		AutoDisableMinTicks:    5,
		FailureWindow:          100,
	}
}

// TestSCHEDGAP1608_StatusFailureRatesExcludeOperatorReaps is the API-side
// regression: the REAL HTTP surface must report reaps as excluded (failed 0,
// rate 0, not armed) for a lane whose window is all operator reaps, must
// mirror the same verdict for the legacy text-only rows, and must keep a
// genuine failure counted on the same shape of data.
func TestSCHEDGAP1608_StatusFailureRatesExcludeOperatorReaps(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	const (
		drainLane  = "reap-drain" // 40 failed drain_timeout reaps + 2 completions
		opLane     = "reap-op"    // 40 failed operator_restart reaps + 2 completions
		textLane   = "reap-text"  // 40 legacy abort-text-only failures + 2 completions
		genuineOne = "onefault"   // 40 reaps + 1 GENUINE failure + 2 completions
		genuineAll = "allgenuine" // 40 genuine failures + 2 completions
	)
	for _, p := range []string{drainLane, opLane, textLane, genuineOne, genuineAll} {
		mustCreateHelperTestProject(t, db, p)
	}

	seq := 0
	insert := func(project, status, orphan, errText string) {
		seq++
		gap1608InsertTick(t, db, fmt.Sprintf("%s-%03d", project, seq), project, status, orphan, errText,
			time.Now().Add(-time.Duration(seq)*time.Second))
	}
	for i := 0; i < 40; i++ {
		// Structural reap rows are TEXTLESS: only the orphan stamp (or the
		// NEW exclusion path) can exclude them — the regression cannot
		// accidentally pass via the legacy abort-text marker in
		// failureclass.go (non-vacuity).
		insert(drainLane, "failed", "drain_timeout", "")
		insert(opLane, "failed", "operator_restart", "")
		insert(textLane, "failed", "", gap1608AbortText)
		insert(genuineOne, "failed", "drain_timeout", "")
		insert(genuineAll, "failed", "", gap1608RealErr)
	}
	for _, p := range []string{drainLane, opLane, textLane, genuineOne, genuineAll} {
		insert(p, "completed", "", "")
		insert(p, "completed", "", "")
	}
	// genuineOne's single genuine failure, more recent than the reaps so it
	// sits inside the per-project window ordering deterministically:
	insert(genuineOne, "failed", "", gap1608RealErr)

	rates := gap1608RunStatus(t, db)

	for name, want := range map[string]struct {
		failed, total float64
		armed         bool
	}{
		drainLane:  {0, 2, false},
		opLane:     {0, 2, false},
		textLane:   {0, 2, false},
		genuineOne: {1, 3, false}, // the genuine failure survived, the 40 reaps did not
		genuineAll: {40, 42, true},
	} {
		entry, ok := rates[name].(map[string]interface{})
		if !ok {
			t.Fatalf("%s missing from projects_failure_rates: %v", name, rates)
		}
		failed, _ := entry["failed"].(float64)
		total, _ := entry["total"].(float64)
		armed, _ := entry["auto_disable_armed"].(bool)
		if failed != want.failed || total != want.total {
			t.Errorf("%s: failed=%v total=%v, want %v/%v — the reap exclusion or genuine accounting broke on the status surface",
				name, failed, total, want.failed, want.total)
		}
		if armed != want.armed {
			t.Errorf("%s: auto_disable_armed=%v, want %v", name, armed, want.armed)
		}
	}
}

// TestSCHEDGAP1608_StatusMatchesEnforcerOnReapFixture pins surface↔enforcer
// PARITY on reap-heavy data: for the same DB, the enforcer's verdict (park or
// not) must agree with the surface's auto_disable_armed for every lane in the
// fixture — no surface may report armed=true while the enforcer leaves the
// lane running (the measured mass-disable lie), and vice versa.
func TestSCHEDGAP1608_StatusMatchesEnforcerOnReapFixture(t *testing.T) {
	db := gap1608EnforcerFixture(t)
	defer db.Close()

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	srv := NewServer(db, loop)
	srv.SetResolvedConfig(gap1608ResolvedConfig())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rates := body["projects_failure_rates"].(map[string]interface{})

	esc := scheduler.NewAlertEscalatorWithPolicy(db, scheduler.NewEventLogger(db), 0.5, 100, 5)
	if err := esc.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("enforcer: %v", err)
	}
	for name, entry := range rates {
		m, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("%s wrong shape: %T", name, entry)
		}
		surfaceArmed, _ := m["auto_disable_armed"].(bool)
		var enabled int
		if err := db.QueryRow(`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&enabled); err != nil {
			t.Fatalf("query %s: %v", name, err)
		}
		enforcerDisabled := enabled == 0
		if surfaceArmed != enforcerDisabled {
			t.Errorf("SCHED-GAP-1608 PARITY DIVERGENCE on %s: surface armed=%v but enforcer disabled=%v",
				name, surfaceArmed, enforcerDisabled)
		}
	}
}

// gap1608EnforcerFixture builds the reap-heavy + genuine fixture both the
// surface and the enforcer then evaluate.
func gap1608EnforcerFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	for _, p := range []string{"p-reaps", "p-genuine"} {
		mustCreateHelperTestProject(t, db, p)
	}
	seq := 0
	insert := func(project, status, orphan, errText string) {
		seq++
		gap1608InsertTick(t, db, fmt.Sprintf("%s-%03d", project, seq), project, status, orphan, errText,
			time.Now().Add(-time.Duration(seq)*time.Second))
	}
	for i := 0; i < 40; i++ {
		insert("p-reaps", "failed", "drain_timeout", "") // textless structural row
		insert("p-genuine", "failed", "", gap1608RealErr)
	}
	return db
}
