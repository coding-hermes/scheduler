package scheduler

// SCHED-GAP-128 — regression coverage for the per-(project, finding-class)
// shared-infra alert throttle.
//
// The invariant the row states, and what these tests pin:
//
//  1. Consecutive IDENTICAL infra alerts (same project + same finding class)
//     are spaced at least DefaultInfraAlertThrottleWindow apart in the events
//     log — the table GET /api/v1/events reads.
//  2. A DIFFERENT class about the same project, or the same class about a
//     different project, is NOT suppressed: the throttle unit is the tuple.
//  3. The throttle survives the production shape — a FRESH escalator on every
//     health pass (tick_process.go) — because the durable half of its memory
//     is the events table, not the in-process map.
//  4. The window is configurable (env + setter) and fails closed to the 1h
//     default on a nonsense value.
//  5. The scheduler's own unthrottled infra emitter, CheckDuplicateWorkdirs,
//     now rides the same path and keeps emitting the finding class + project
//     on the payload, so an operator can filter the tuple.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// infraFixedClock installs a fixed clock on ae and returns the context an
// emission must carry: the escalator compares against ae's clock while
// database.LogEvent stamps created_at from the CONTEXT clock, so both halves
// of the throttle must read the same instant or the test would be measuring
// the harness instead of the code.
func infraFixedClock(ae *AlertEscalator, at time.Time) context.Context {
	c := clock.NewFixed(at)
	ae.SetClock(c)
	return clock.WithClock(context.Background(), c)
}

// infraTestProjects inserts an enabled lane with its workdir. The real schema
// (database.InitDB, used by newEmitterTestDB) requires repo_url/workdir and
// both timestamps.
func infraTestProject(t *testing.T, db *sql.DB, name, workdir string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, cooldown_s, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		name, "https://example.invalid/"+name, workdir, 900, now, now)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

// infraEventRows returns every event in the operator's listing order
// (newest first) — the same read path GET /api/v1/events uses.
func infraEventRows(t *testing.T, db *sql.DB) []database.Event {
	t.Helper()
	rows, err := database.ListEvents(context.Background(), db, "", "", 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return rows
}

// infraEventDetails decodes one event's details blob.
func infraEventDetails(t *testing.T, ev database.Event) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(ev.Details), &out); err != nil {
		t.Fatalf("decode details %q: %v", ev.Details, err)
	}
	return out
}

// TestSCHEDGAP128_InfraAlertWindow_DefaultsAndOverride pins the default (1h)
// and the two override paths: the env var wins when it parses, and a
// nonsense/non-positive value falls back to the default rather than disabling
// the throttle.
func TestSCHEDGAP128_InfraAlertWindow_DefaultsAndOverride(t *testing.T) {
	if DefaultInfraAlertThrottleWindow != time.Hour {
		t.Fatalf("DefaultInfraAlertThrottleWindow = %v, want 1h (SCHED-GAP-128 acceptance 1)",
			DefaultInfraAlertThrottleWindow)
	}

	db := newEmitterTestDB(t)
	events := NewEventLogger(db)

	t.Setenv(InfraAlertThrottleEnv, "")
	if got := NewAlertEscalator(db, events, autoDisablePolicy{}).infraAlertWindowOrDefault(); got != time.Hour {
		t.Errorf("default window = %v, want 1h", got)
	}

	t.Setenv(InfraAlertThrottleEnv, "90m")
	if got := NewAlertEscalator(db, events, autoDisablePolicy{}).infraAlertWindowOrDefault(); got != 90*time.Minute {
		t.Errorf("env-configured window = %v, want 90m", got)
	}

	for _, bad := range []string{"banana", "0s", "-5m"} {
		t.Setenv(InfraAlertThrottleEnv, bad)
		if got := NewAlertEscalator(db, events, autoDisablePolicy{}).infraAlertWindowOrDefault(); got != time.Hour {
			t.Errorf("window for %q = %v, want the 1h default (fail closed)", bad, got)
		}
	}

	// The setter overrides the env-resolved value, and 0 restores the default.
	ae := NewAlertEscalator(db, events, autoDisablePolicy{})
	ae.SetInfraAlertThrottleWindow(15 * time.Minute)
	if got := ae.infraAlertWindowOrDefault(); got != 15*time.Minute {
		t.Errorf("SetInfraAlertThrottleWindow(15m) = %v", got)
	}
	ae.SetInfraAlertThrottleWindow(0)
	if got := ae.infraAlertWindowOrDefault(); got != time.Hour {
		t.Errorf("SetInfraAlertThrottleWindow(0) = %v, want 1h", got)
	}
}

// TestSCHEDGAP128_EmitInfraFinding_StampsTupleAndSuppressesSamePass proves the
// payload carries the throttle tuple (project + finding_class) on the visible
// row, that the caller's details map is not mutated, and that a second
// identical finding inside ONE health pass is already suppressed by the
// in-process map.
func TestSCHEDGAP128_EmitInfraFinding_StampsTupleAndSuppressesSamePass(t *testing.T) {
	db := newEmitterTestDB(t)
	events := NewEventLogger(db)
	ae := NewAlertEscalator(db, events, autoDisablePolicy{})
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := infraFixedClock(ae, at)

	caller := map[string]any{"host": "bunker-1"}
	if !ae.EmitInfraFinding(ctx, SeverityHigh, "", "bunker-port-pool", InfraAlertPortPool,
		"port pool exhausted on bunker-1", caller) {
		t.Fatal("first finding must emit")
	}
	if _, mutated := caller["project"]; mutated {
		t.Error("EmitInfraFinding mutated the caller's details map")
	}
	if _, mutated := caller["finding_class"]; mutated {
		t.Error("EmitInfraFinding mutated the caller's details map")
	}

	// Same tuple, same pass: the map half of the throttle suppresses it.
	if ae.EmitInfraFinding(ctx, SeverityHigh, "", "bunker-port-pool", InfraAlertPortPool,
		"port pool exhausted on bunker-1", nil) {
		t.Error("repeat inside one health pass must be suppressed")
	}

	rows := infraEventRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("expected 1 event after the suppressed repeat, got %d", len(rows))
	}
	if rows[0].Component != InfraAlertComponent {
		t.Errorf("component = %q, want %q (empty component default)", rows[0].Component, InfraAlertComponent)
	}
	details := infraEventDetails(t, rows[0])
	if details["project"] != "bunker-port-pool" {
		t.Errorf("details.project = %v, want bunker-port-pool", details["project"])
	}
	if details["finding_class"] != string(InfraAlertPortPool) {
		t.Errorf("details.finding_class = %v, want %q", details["finding_class"], InfraAlertPortPool)
	}
	if details["host"] != "bunker-1" {
		t.Errorf("details.host = %v, want the caller's detail preserved", details["host"])
	}
}

// TestSCHEDGAP128_InfraAlertSpacing_OneHourAcrossPasses is ACCEPTANCE 1: with a
// FRESH escalator on every call (exactly how tick_process.go runs the
// escalator), consecutive identical infra alerts land at least one hour apart
// in the events log the API serves.
func TestSCHEDGAP128_InfraAlertSpacing_OneHourAcrossPasses(t *testing.T) {
	db := newEmitterTestDB(t)
	events := NewEventLogger(db)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	emit := func(at time.Time) bool {
		ae := NewAlertEscalator(db, events, autoDisablePolicy{}) // one fresh escalator per health pass
		return ae.EmitInfraFinding(infraFixedClock(ae, at), SeverityMedium, "", "bunker-host",
			InfraAlertStaleBinary, "stale binary: schedulerd predates the merged fix", nil)
	}

	if !emit(base) {
		t.Fatal("first finding must emit")
	}
	if emit(base.Add(30 * time.Minute)) {
		t.Error("a repeat 30m later must be suppressed")
	}
	if emit(base.Add(time.Hour - time.Second)) {
		t.Error("a repeat 59m59s later must be suppressed")
	}
	if !emit(base.Add(time.Hour)) {
		t.Error("a repeat at the window boundary (1h) must emit — the window is 'at least', not 'more than'")
	}

	rows := infraEventRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("expected 2 events (one per hour), got %d", len(rows))
	}
	// ListEvents is newest-first.
	newest, err := time.Parse(time.RFC3339, rows[0].CreatedAt)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", rows[0].CreatedAt, err)
	}
	oldest, err := time.Parse(time.RFC3339, rows[1].CreatedAt)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", rows[1].CreatedAt, err)
	}
	if spacing := newest.Sub(oldest); spacing < time.Hour {
		t.Errorf("identical infra alerts spaced %v apart, want >= 1h", spacing)
	}
}

// TestSCHEDGAP128_InfraAlertKey_ProjectAndClassIndependent proves the throttle
// unit is the TUPLE: a different class about the same project and the same
// class about a different project both emit, and only the exact tuple repeat
// is suppressed — across fresh escalator instances, so the durable lookup is
// what is being exercised.
func TestSCHEDGAP128_InfraAlertKey_ProjectAndClassIndependent(t *testing.T) {
	db := newEmitterTestDB(t)
	events := NewEventLogger(db)
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	emit := func(project string, class InfraAlertClass) bool {
		ae := NewAlertEscalator(db, events, autoDisablePolicy{})
		return ae.EmitInfraFinding(infraFixedClock(ae, at), SeverityHigh, "", project, class,
			"infra finding for "+project+"/"+string(class), nil)
	}

	for _, tc := range []struct {
		project string
		class   InfraAlertClass
		want    bool
	}{
		{"lane-a", InfraAlertPortPool, true},      // first tuple
		{"lane-a", InfraAlertDroppedConfig, true}, // same project, different class
		{"lane-b", InfraAlertPortPool, true},      // same class, different project
		{"lane-a", InfraAlertPortPool, false},     // exact tuple repeat
		{"lane-a", InfraAlertDroppedConfig, false},
		{"lane-b", InfraAlertPortPool, false},
	} {
		if got := emit(tc.project, tc.class); got != tc.want {
			t.Errorf("emit(%s, %s) = %v, want %v", tc.project, tc.class, got, tc.want)
		}
	}
	if got := len(infraEventRows(t, db)); got != 3 {
		t.Errorf("expected 3 distinct-tuple events, got %d", got)
	}
}

// TestSCHEDGAP128_InfraAlertWindow_Configurable proves the window actually
// gates on its VALUE: with a 90m override a repeat at 60m is suppressed and a
// repeat at 95m is not.
func TestSCHEDGAP128_InfraAlertWindow_Configurable(t *testing.T) {
	t.Setenv(InfraAlertThrottleEnv, "90m")
	db := newEmitterTestDB(t)
	events := NewEventLogger(db)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	emit := func(at time.Time) bool {
		ae := NewAlertEscalator(db, events, autoDisablePolicy{})
		return ae.EmitInfraFinding(infraFixedClock(ae, at), SeverityMedium, "", "host-x",
			InfraAlertDroppedConfig, "dropped config: dispatch-targets entry vanished", nil)
	}

	if !emit(base) {
		t.Fatal("first finding must emit")
	}
	if emit(base.Add(60 * time.Minute)) {
		t.Error("a 60m repeat must be suppressed under a 90m window")
	}
	if !emit(base.Add(95 * time.Minute)) {
		t.Error("a 95m repeat must emit under a 90m window")
	}
	if got := len(infraEventRows(t, db)); got != 2 {
		t.Errorf("expected 2 events under the 90m window, got %d", got)
	}
}

// TestSCHEDGAP128_CheckDuplicateWorkdirs_Throttled pins the real in-repo
// producer: an unchanged duplicate workdir group used to emit HIGH on EVERY
// health pass, and now emits at most once per window while carrying the tuple
// on the payload.
func TestSCHEDGAP128_CheckDuplicateWorkdirs_Throttled(t *testing.T) {
	db := newEmitterTestDB(t)
	events := NewEventLogger(db)
	infraTestProject(t, db, "HEADING", "/home/kara/heading")
	infraTestProject(t, db, "heading", "/home/kara/heading")
	infraTestProject(t, db, "solo", "/home/kara/solo")

	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	run := func(at time.Time) {
		ae := NewAlertEscalator(db, events, autoDisablePolicy{})
		if err := ae.CheckDuplicateWorkdirs(infraFixedClock(ae, at)); err != nil {
			t.Fatalf("CheckDuplicateWorkdirs: %v", err)
		}
	}

	run(base)
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Fatalf("after first pass: expected 1 HIGH event, got %d", n)
	}
	run(base.Add(10 * time.Minute)) // unchanged duplicate, next health pass
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("after a second pass 10m later: expected the finding suppressed, got %d HIGH events", n)
	}
	run(base.Add(61 * time.Minute)) // past the window: the finding is re-reported
	if n := countEventsBySeverity(t, db, "HIGH"); n != 2 {
		t.Errorf("after the window elapsed: expected the finding re-emitted, got %d HIGH events", n)
	}

	rows := infraEventRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rows))
	}
	details := infraEventDetails(t, rows[0])
	if details["finding_class"] != string(InfraAlertDuplicateWorkdir) {
		t.Errorf("details.finding_class = %v, want %q", details["finding_class"], InfraAlertDuplicateWorkdir)
	}
	if details["project"] != "HEADING" {
		t.Errorf("details.project = %v, want the group's canonical first member HEADING", details["project"])
	}
	if got, ok := details["projects"].([]any); !ok || len(got) != 2 {
		t.Errorf("details.projects = %v, want the 2-member roster", details["projects"])
	}
	if rows[0].Message == "" || !strings.Contains(rows[0].Message, "duplicate workdir") {
		t.Errorf("message = %q, want the duplicate-workdir wording preserved", rows[0].Message)
	}
}
