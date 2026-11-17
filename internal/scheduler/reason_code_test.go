package scheduler

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-16947 acceptance — the reason code must be DERIVED per mechanism,
// PERSISTED on the row, and the liveness rule must be unforgiving.
//
// Every persisted assertion below reads the ticks columns back, never a helper's
// return value: the columns are the deliverable (a classifier that computes a
// code and writes it nowhere is the defect this row removes).

// sched16947Complete enqueues a tick, optionally stamps its no-op verdict, runs
// the real finalization path, and reads the persisted verdict back.
func sched16947Complete(t *testing.T, db *sql.DB, project, tickID string, noopFlag int, o TickOutcome) (status, outcome, reason, unavailable string) {
	t.Helper()
	lt := NewLifecycleTracker(db)
	if err := lt.Enqueue(project, tickID); err != nil {
		t.Fatalf("enqueue %s: %v", tickID, err)
	}
	if noopFlag != 0 {
		if _, err := db.Exec(`UPDATE ticks SET noop_flag = ? WHERE id = ?`, noopFlag, tickID); err != nil {
			t.Fatalf("stamp noop_flag on %s: %v", tickID, err)
		}
	}
	o.TickID = tickID
	o.Project = project
	if o.Started.IsZero() {
		o.Started = time.Now().Add(-time.Minute)
	}
	if o.Finished.IsZero() {
		o.Finished = time.Now()
	}
	if err := lt.Complete(o); err != nil {
		t.Fatalf("complete %s: %v", tickID, err)
	}
	if err := db.QueryRow(
		`SELECT status, COALESCE(outcome,''), reason_code, tokens_out_unavailable FROM ticks WHERE id = ?`, tickID,
	).Scan(&status, &outcome, &reason, &unavailable); err != nil {
		t.Fatalf("read verdict for %s: %v", tickID, err)
	}
	return status, outcome, reason, unavailable
}

// TestSCHEDGAP16947_MechanismMapping pins the five measured mechanisms onto the
// closed vocabulary — the mapping itself, pure, one row per mechanism.
func TestSCHEDGAP16947_MechanismMapping(t *testing.T) {
	cases := []struct {
		name    string
		outcome TickOutcome
		noop    int
		want    string
		why     string
	}{
		{
			name:    "a: model declared no-op",
			outcome: TickOutcome{Status: TickCompleted, TokensIn: 40_000, TokensOut: 200},
			noop:    1,
			want:    database.TickReasonCodeModelNoop,
			why:     "noop_flag=1 is the guard's own declaration (pre==post on a lane that may not no-op)",
		},
		{
			name:    "b: ran, spent, landed nothing",
			outcome: TickOutcome{Status: TickCompleted, TokensIn: 900_000, TokensOut: 4_000, CostSource: CostSourceGateway},
			want:    database.TickReasonCodeNoArtifact,
			why:     "a measured session with no artifact is the plain dry run",
		},
		{
			name: "c: session produced no output at all",
			outcome: TickOutcome{Status: TickCompleted, CostSource: CostSourceGateway,
				TokensOutUnavailable: database.TokensOutUnavailableGatewayNoUsage},
			want: database.TickReasonCodeMalformedToolCall,
			why:  "no usage object in the terminal envelope: nothing was measured, so silence is the honest reading",
		},
		{
			name: "c: measured zero output",
			outcome: TickOutcome{Status: TickCompleted, TokensIn: 30_000, CostSource: CostSourceGateway,
				TokensOutUnavailable: ""},
			want: database.TickReasonCodeMalformedToolCall,
			why:  "a MEASURED zero tokens_out is output that never became a tool call",
		},
		{
			name:    "c: overran its own wall",
			outcome: TickOutcome{Status: TickCompleted, Started: time.Unix(0, 0), Finished: time.Unix(2400, 0), Deadline: 1800 * time.Second},
			want:    database.TickReasonCodeDeadlineExceeded,
			why:     "the 2,380s-against-1,800s shape: liveness outranks the completed verdict",
		},
		{
			name:    "d: never opened a session by design",
			outcome: TickOutcome{Status: TickDeferred, Error: "gateway transient"},
			want:    database.TickReasonCodeNotSpawned,
			why:     "a deferral charges nobody: no session opened",
		},
		{
			name:    "e: execution surface unavailable",
			outcome: TickOutcome{Status: TickFailed, Error: "gateway is draining (503): refusing new work"},
			want:    database.TickReasonCodeLaneUnavailable,
			why:     "the transport classifier's drain class is the lane's surface, not the lane's fault",
		},
		{
			name:    "e: owner-gone reap",
			outcome: TickOutcome{Status: TickTimeout, TelemetryPartial: true, TelemetryPartialReason: TelemetryPartialStaleReap},
			want:    database.TickReasonCodeLaneUnavailable,
			why:     "a stale-reaped row means the tick's owner vanished mid-run",
		},
		{
			name:    "operator restart",
			outcome: TickOutcome{Status: TickAborted, OperatorRestart: true, AbortReason: OrphanReasonOperatorRestart},
			want:    database.TickReasonCodeLaneUnavailable,
			why:     "an operator restart took the surface away from an in-flight tick",
		},
		{
			name:    "guard abort",
			outcome: TickOutcome{Status: TickFailed, GuardAbort: true},
			want:    database.TickReasonCodeNoArtifact,
			why:     "the builder no-artifact guard's verdict is literally 'no artifact'",
		},
		{
			name:    "no_work stays distinct from a silent session",
			outcome: TickOutcome{Status: TickCompleted, NoTools: true, TokensIn: 12_000, TokensOut: 90, CostSource: CostSourceGateway},
			want:    database.TickReasonCodeNoWork,
			why:     "a MEASURED zero-tool transcript is the strongest nothing-to-do proof",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTickReasonCode(tc.outcome, terminalOutcome(tc.outcome), failureReasonClass(tc.outcome.Error), tc.noop)
			if got != tc.want {
				t.Fatalf("resolveTickReasonCode = %q, want %q (%s)", got, tc.want, tc.why)
			}
			if !database.TickReasonCodeIsValid(got) {
				t.Fatalf("derived code %q is outside the closed vocabulary", got)
			}
		})
	}
}

// TestSCHEDGAP16947_CommittedOwesNoReason pins the honest-empty convention: a
// productive tick is not annotated with a reason it does not have.
func TestSCHEDGAP16947_CommittedOwesNoReason(t *testing.T) {
	o := TickOutcome{Status: TickCompleted, Commits: 3, FilesChanged: 9}
	if got := resolveTickReasonCode(o, terminalOutcome(o), "", 0); got != "" {
		t.Fatalf("committed tick derived reason %q, want \"\"", got)
	}
}

// TestSCHEDGAP16947_UnknownCarriesRawSignal is the vocabulary's escape-hatch
// contract: an unmapped cause is recorded WITH the signal that made it unknown.
func TestSCHEDGAP16947_UnknownCarriesRawSignal(t *testing.T) {
	o := TickOutcome{Status: TickFailed, Error: "board read failed: unexpected EOF\nsecond line"}
	got := resolveTickReasonCode(o, terminalOutcome(o), failureReasonClass(o.Error), 0)
	if !strings.HasPrefix(got, database.TickReasonCodeUnknownPrefix) {
		t.Fatalf("unmapped failure derived %q, want an unknown:<raw> code", got)
	}
	if !strings.Contains(got, "unexpected EOF") {
		t.Errorf("unknown code %q dropped the raw signal", got)
	}
	if strings.Contains(got, "second line") {
		t.Errorf("unknown code %q carried more than the first error line", got)
	}
	if !database.TickReasonCodeIsValid(got) {
		t.Errorf("unknown code %q fails the vocabulary check", got)
	}

	// A timeout with no classified cause carries the timeout shape as the signal.
	to := TickOutcome{Status: TickTimeout, Error: "session ended with no terminal event"}
	got = resolveTickReasonCode(to, terminalOutcome(to), failureReasonClass(to.Error), 0)
	if !strings.Contains(got, "timeout: ") {
		t.Errorf("unclassified timeout derived %q, want an unknown:timeout:… code", got)
	}
}

// TestSCHEDGAP16947_LivenessRule pins the predicate's edges and the FINALIZATION
// consequence: a completed verdict past its own wall is stored timeout, never
// completed.
func TestSCHEDGAP16947_LivenessRule(t *testing.T) {
	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		o    TickOutcome
		want bool
	}{
		{"exactly at the wall counts (a ctx deadline is done AT its instant)",
			TickOutcome{Started: base, Finished: base.Add(30 * time.Minute), Deadline: 30 * time.Minute}, true},
		{"far past the wall", TickOutcome{Started: base, Finished: base.Add(2380 * time.Second), Deadline: 1800 * time.Second}, true},
		{"inside the wall", TickOutcome{Started: base, Finished: base.Add(20 * time.Minute), Deadline: 30 * time.Minute}, false},
		{"no known wall stays inert", TickOutcome{Started: base, Finished: base.Add(10 * time.Hour), Deadline: 0}, false},
		{"zero timestamps are unmeasured", TickOutcome{Deadline: 30 * time.Minute}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tickOverranDeadline(tc.o); got != tc.want {
				t.Fatalf("tickOverranDeadline = %v, want %v", got, tc.want)
			}
		})
	}

	// Persisted: the overrun cannot survive as 'completed'.
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "liveness-lane")
	started := time.Now().Add(-40 * time.Minute)
	status, outcome, reason, _ := sched16947Complete(t, db, "liveness-lane", "liveness-t1", 0, TickOutcome{
		Status:   TickCompleted,
		Started:  started,
		Finished: time.Now(),
		Deadline: 30 * time.Minute,
	})
	if status != string(TickTimeout) {
		t.Fatalf("persisted status = %q, want %q — an overrun verdict must never stay completed", status, TickTimeout)
	}
	if outcome != "timeout" {
		t.Fatalf("persisted outcome = %q, want timeout", outcome)
	}
	if reason != database.TickReasonCodeDeadlineExceeded {
		t.Fatalf("persisted reason_code = %q, want %q", reason, database.TickReasonCodeDeadlineExceeded)
	}

	// A tick INSIDE its wall keeps its completed verdict and its own reason.
	db2 := newTestDB(t)
	mustCreateProjectINFRA012(t, db2, "liveness-lane-2")
	status, outcome, reason, _ = sched16947Complete(t, db2, "liveness-lane-2", "liveness-t2", 0, TickOutcome{
		Status:   TickCompleted,
		Started:  time.Now().Add(-10 * time.Minute),
		Finished: time.Now(),
		Deadline: 30 * time.Minute,
		TokensIn: 500_000, TokensOut: 1200, CostSource: CostSourceGateway,
	})
	if status != string(TickCompleted) || outcome != "dry_run" {
		t.Fatalf("in-wall tick persisted %s/%s, want completed/dry_run", status, outcome)
	}
	if reason != database.TickReasonCodeNoArtifact {
		t.Fatalf("in-wall dry tick reason_code = %q, want %q", reason, database.TickReasonCodeNoArtifact)
	}
}

// TestSCHEDGAP16947_PersistedMechanisms proves each mechanism's code reaches the
// COLUMN through the real finalization path — the deliverable, not a helper.
func TestSCHEDGAP16947_PersistedMechanisms(t *testing.T) {
	cases := []struct {
		name        string
		project     string
		noop        int
		outcome     TickOutcome
		wantStatus  string
		wantOutcome string
		wantReason  string
		wantUnavail string
	}{
		{
			name: "model_noop", project: "mech-noop", noop: 1,
			outcome:    TickOutcome{Status: TickCompleted, TokensIn: 40_000, TokensOut: 300, CostSource: CostSourceGateway},
			wantStatus: "completed", wantOutcome: "dry_run",
			wantReason: database.TickReasonCodeModelNoop,
		},
		{
			name: "no_artifact", project: "mech-dry",
			outcome:    TickOutcome{Status: TickCompleted, TokensIn: 800_000, TokensOut: 900, CostSource: CostSourceGateway},
			wantStatus: "completed", wantOutcome: "dry_run",
			wantReason: database.TickReasonCodeNoArtifact,
		},
		{
			name: "malformed_tool_call", project: "mech-silent",
			outcome: TickOutcome{Status: TickCompleted, CostSource: CostSourceGateway,
				TokensOutUnavailable: database.TokensOutUnavailableGatewayNoUsage},
			wantStatus:  "completed",
			wantReason:  database.TickReasonCodeMalformedToolCall,
			wantUnavail: database.TokensOutUnavailableGatewayNoUsage,
		},
		{
			name: "not_spawned", project: "mech-deferred",
			outcome: TickOutcome{Status: TickDeferred, Error: "gateway unreachable: transient blip", CostSource: CostSourceGateway,
				TokensOutUnavailable: database.TokensOutUnavailableNoSession},
			wantStatus: "deferred", wantOutcome: "deferred",
			wantReason:  database.TickReasonCodeNotSpawned,
			wantUnavail: database.TokensOutUnavailableNoSession,
		},
		{
			name: "lane_unavailable", project: "mech-transport",
			outcome:    TickOutcome{Status: TickFailed, Error: "gateway is draining (503): refusing new work", DispatchReason: database.DispatchReasonUnavailable},
			wantStatus: "failed", wantOutcome: "failed",
			wantReason: database.TickReasonCodeLaneUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			mustCreateProjectINFRA012(t, db, tc.project)
			status, outcome, reason, unavailable := sched16947Complete(t, db, tc.project, tc.project+"-t1", tc.noop, tc.outcome)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantOutcome != "" && outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tc.wantOutcome)
			}
			if reason != tc.wantReason {
				t.Errorf("reason_code = %q, want %q", reason, tc.wantReason)
			}
			if unavailable != tc.wantUnavail {
				t.Errorf("tokens_out_unavailable = %q, want %q", unavailable, tc.wantUnavail)
			}
		})
	}
}

// TestSCHEDGAP16947_TokensOutExplicitlyUnavailable is AC3: an unmeasured token
// figure is recorded as an explicit unavailable-with-reason, never as an absent
// key and never as an indistinguishable 0.
func TestSCHEDGAP16947_TokensOutExplicitlyUnavailable(t *testing.T) {
	cases := []struct {
		name string
		o    TickOutcome
		want string
	}{
		{"measured is a measurement (a genuine 0 included)", TickOutcome{CostSource: CostSourceMeasured}, ""},
		{"gateway usage is a measurement", TickOutcome{CostSource: CostSourceGateway}, ""},
		{"legacy/unset source stays honest-empty", TickOutcome{}, ""},
		{"simulated figures are fabricated", TickOutcome{CostSource: CostSourceSimulated}, database.TokensOutUnavailableSimulated},
		{"exec estimate has no telemetry", TickOutcome{CostSource: CostSourceEstimated}, database.TokensOutUnavailableEstimatedNoTelemetry},
		{"sub-included estimate has no telemetry", TickOutcome{CostSource: CostSourceEstimatedSubincluded}, database.TokensOutUnavailableEstimatedNoTelemetry},
		{"explicit mark wins verbatim", TickOutcome{TokensOutUnavailable: "remote_dispatch_no_usage", CostSource: CostSourceGateway}, "remote_dispatch_no_usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTokensOutUnavailable(tc.o); got != tc.want {
				t.Fatalf("resolveTokensOutUnavailable = %q, want %q", got, tc.want)
			}
		})
	}

	// Simulated ticks persist the mark (the sim finalizer is a separate writer):
	// its reason_code + tokens_out_unavailable ride ONE UPDATE, so a fabricated
	// 500-3,500 token figure can never read as a measurement.
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sim-lane")
	sim := NewSimSpawner(db, 1.0)
	sim.SetIdleRate(1.0) // every completed sim tick is artifact-less
	if _, err := sim.Spawn(PackedProject{Name: "sim-lane", Urgency: 1, Weight: 10}, "sim-tick-0001"); err != nil {
		t.Fatalf("sim spawn: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var simReason, simUnavailable string
	for {
		err := db.QueryRow(`SELECT reason_code, tokens_out_unavailable FROM ticks WHERE id='sim-tick-0001'`).
			Scan(&simReason, &simUnavailable)
		if err == nil && simUnavailable != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sim finalizer never stamped tokens_out_unavailable (last err=%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if simUnavailable != database.TokensOutUnavailableSimulated {
		t.Errorf("sim tick tokens_out_unavailable = %q, want %q", simUnavailable, database.TokensOutUnavailableSimulated)
	}
	if simReason != database.TickReasonCodeNotSpawned {
		t.Errorf("artifact-less sim tick reason_code = %q, want %q (a simulated tick never opens a real session)",
			simReason, database.TickReasonCodeNotSpawned)
	}
}

// TestSCHEDGAP16947_UsagePresenceDetection is the signal behind mechanism (c):
// an ABSENT usage key must be distinguishable from a measured zero.
func TestSCHEDGAP16947_UsagePresenceDetection(t *testing.T) {
	var withUsage Response
	if err := json.Unmarshal([]byte(`{"id":"s1","status":"completed","usage":{"input_tokens":10,"output_tokens":4}}`), &withUsage); err != nil {
		t.Fatalf("decode envelope with usage: %v", err)
	}
	if !withUsage.UsagePresent {
		t.Fatal("UsagePresent = false for an envelope that carries usage")
	}
	if got := usageUnavailableMark(&withUsage); got != "" {
		t.Errorf("usageUnavailableMark(with usage) = %q, want \"\" (a real measurement)", got)
	}

	var withoutUsage Response
	if err := json.Unmarshal([]byte(`{"id":"s2","status":"completed","output":[]}`), &withoutUsage); err != nil {
		t.Fatalf("decode envelope without usage: %v", err)
	}
	if withoutUsage.UsagePresent {
		t.Fatal("UsagePresent = true for an envelope with no usage key")
	}
	if got := usageUnavailableMark(&withoutUsage); got != database.TokensOutUnavailableGatewayNoUsage {
		t.Errorf("usageUnavailableMark(no usage) = %q, want %q", got, database.TokensOutUnavailableGatewayNoUsage)
	}

	var nullUsage Response
	if err := json.Unmarshal([]byte(`{"id":"s3","status":"completed","usage":null}`), &nullUsage); err != nil {
		t.Fatalf("decode envelope with null usage: %v", err)
	}
	if nullUsage.UsagePresent {
		t.Fatal("UsagePresent = true for `usage: null` — a null key is not a measurement")
	}

	// A measured zero is a MEASUREMENT: present with 0/0 must not be marked
	// unavailable (that is mechanism (b), not (c)).
	var zeroUsage Response
	if err := json.Unmarshal([]byte(`{"id":"s4","status":"completed","usage":{"input_tokens":0,"output_tokens":0}}`), &zeroUsage); err != nil {
		t.Fatalf("decode envelope with zero usage: %v", err)
	}
	if !zeroUsage.UsagePresent {
		t.Fatal("UsagePresent = false for an enumerated zero measurement")
	}
	if got := usageUnavailableMark(&zeroUsage); got != "" {
		t.Errorf("usageUnavailableMark(measured zero) = %q, want \"\"", got)
	}

	// The SSE terminal envelope path wraps Response in a `response` object; the
	// nested decode must still record presence.
	var sse struct {
		Response Response `json:"response"`
	}
	if err := json.Unmarshal([]byte(`{"type":"response.completed","response":{"id":"s5","status":"completed"}}`), &sse); err != nil {
		t.Fatalf("decode nested SSE envelope: %v", err)
	}
	if sse.Response.UsagePresent {
		t.Fatal("nested SSE envelope without usage reported UsagePresent = true")
	}
}

// TestSCHEDGAP16947_ReapPathsStampReasonCode closes the coverage claim: the
// conclusion paths that do NOT run through lifecycle.Complete still stamp the
// vocabulary, so "every concluded tick" holds for the reaps too.
func TestSCHEDGAP16947_ReapPathsStampReasonCode(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "reap-lane")
	lt := NewLifecycleTracker(db)

	// (1) the stale reap: a running row past the backstop window.
	old := time.Now().Add(-6 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES ('reap-stale', 'reap-lane', 'running', ?, ?)`, old, old); err != nil {
		t.Fatalf("seed stale running tick: %v", err)
	}
	if _, _, err := lt.CleanupStaleProjects(90 * time.Minute); err != nil {
		t.Fatalf("CleanupStaleProjects: %v", err)
	}
	var code string
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='reap-stale'`).Scan(&code); err != nil {
		t.Fatalf("read stale-reaped row: %v", err)
	}
	if code != database.TickReasonCodeLaneUnavailable {
		t.Errorf("stale reap reason_code = %q, want %q (owner-gone class)", code, database.TickReasonCodeLaneUnavailable)
	}

	// (2) the dead-pid/zombie reap SQL (the constant cleanDanglingOnStartup
	// executes): the same owner-gone verdict, in the statement itself.
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES ('reap-dead', 'reap-lane', 'running', ?, ?)`, old, old); err != nil {
		t.Fatalf("seed dead-pid running tick: %v", err)
	}
	if _, err := db.Exec(timeoutReapSQL, time.Now().Format(time.RFC3339), "reap-dead"); err != nil {
		t.Fatalf("timeoutReapSQL: %v", err)
	}
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='reap-dead'`).Scan(&code); err != nil {
		t.Fatalf("read dead-reaped row: %v", err)
	}
	if code != database.TickReasonCodeLaneUnavailable {
		t.Errorf("dead-pid reap reason_code = %q, want %q", code, database.TickReasonCodeLaneUnavailable)
	}
}

// TestSCHEDGAP16947_NoWorkVsSilentSession pins the priority that keeps the two
// "nothing happened" verdicts distinguishable: an UNMEASURED no_work row is
// mechanism (c), a measured one is no_work.
func TestSCHEDGAP16947_NoWorkVsSilentSession(t *testing.T) {
	measured := TickOutcome{Status: TickCompleted, NoTools: true, TokensIn: 15_000, TokensOut: 40, CostSource: CostSourceGateway}
	if got := resolveTickReasonCode(measured, terminalOutcome(measured), "", 0); got != database.TickReasonCodeNoWork {
		t.Errorf("measured zero-tool tick = %q, want no_work", got)
	}
	unmeasured := TickOutcome{Status: TickCompleted, NoTools: true, CostSource: CostSourceGateway,
		TokensOutUnavailable: database.TokensOutUnavailableGatewayNoUsage}
	got := resolveTickReasonCode(unmeasured, terminalOutcome(unmeasured), "", 0)
	if got != database.TickReasonCodeMalformedToolCall {
		t.Errorf("unmeasured zero-tool tick = %q, want malformed_tool_call (nothing was measured)", got)
	}
}
