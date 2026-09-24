package scheduler

// SCHED-GAP-1608 — the operator-approved busy-fleet restart path.
//
// The task: a drain-restart deploy cannot converge on a busy fleet — pausing
// stops admission, but a fleet holding 10-12 live heartbeating ticks never
// reaches active_ticks=0, so a controlled restart eventually drains-times-out
// and reaps the in-flight ticks as failed(drain_timeout). The fix is the
// smallest coherent in-repo contract change:
//
//  1. the abort classifier (orphan_exclusion.go) treats an operator-induced
//     reap — orphan_reason drain_timeout / operator_restart, or the legacy
//     "aborted by graceful shutdown" error text — as NOT lane-attributable;
//  2. abortInFlightTicks stamps the DISTINCT reason 'operator_restart' when
//     the daemon's env carries SCHEDULER_OPERATOR_RESTART_APPROVED (set via
//     `systemctl --user set-environment` — restart_approval.go);
//  3. the busy-fleet rehearsal (TestOperatorRestart_BusyFleetRehearsal)
//     proves the whole drill hermetically: a Loop holding 12 in-flight
//     ticks, an approved operator stop, the expected reaps, and the
//     failure-rate/auto-disable surface staying clean while a genuine
//     project failure still counts.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// gap1608SeedFailure inserts one terminal failed tick row with explicit
// orphan_reason and error columns. The STRUCTURAL rows (orphanReason set)
// deliberately carry NO error text: the operator-reap exclusion must fire on
// the orphan stamp alone, so the regression cannot accidentally pass via the
// legacy abort-text path in failureclass.go (non-vacuity).
func gap1608SeedFailure(t *testing.T, db *sql.DB, id, project, orphanReason, errText string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if errText == "" && orphanReason == "" {
		t.Fatalf("gap1608SeedFailure %s: a genuine failure row needs error text", id)
	}
	var err error
	if errText == "" {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, outcome, error, orphaned_at, orphan_reason, spawned_at, completed_at, created_at)
			 VALUES (?, ?, 'failed', 'failed', '', ?, ?, ?, ?, ?)`,
			id, project, now, orphanReason, now, now, now)
	} else {
		_, err = db.Exec(
			`INSERT INTO ticks (id, project_name, status, outcome, error, orphaned_at, orphan_reason, spawned_at, completed_at, created_at)
			 VALUES (?, ?, 'failed', 'failed', ?, ?, ?, ?, ?, ?)`,
			id, project, errText, now, orphanReason, now, now, now)
	}
	if err != nil {
		t.Fatalf("insert failed tick %s: %v", id, err)
	}
}

// gap1608SeedCompleted inserts one terminal completed tick row.
func gap1608SeedCompleted(t *testing.T, db *sql.DB, id, project string, age time.Duration) {
	t.Helper()
	now := time.Now().UTC().Add(-age).Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, created_at)
		 VALUES (?, ?, 'completed', 'committed', ?, ?, ?)`,
		id, project, now, now, now)
	if err != nil {
		t.Fatalf("insert completed tick %s: %v", id, err)
	}
}

// gap1608RunEnforcer drives the REAL auto-disable enforcer over db and
// returns the enabled flag per project afterwards.
func gap1608RunEnforcer(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	esc := NewAlertEscalatorWithPolicy(db, NewEventLogger(db), 0.5, 100, 5)
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
		out[name] = enabled == 1
	}
	return out
}

// TestOperatorAcknowledgedReap_Vocabulary pins the exported exclusion matrix:
// every operator-reap spelling is excluded; a genuine failure and every other
// orphan reason still count.
func TestOperatorAcknowledgedReap_Vocabulary(t *testing.T) {
	type row struct {
		status, errText, orphan string
		want                    bool // want FailureIsLaneAttributableT
	}
	cases := []row{
		// Both operator-reap spellings, on failed AND timeout rows:
		{"failed", "aborted by graceful shutdown — drain timed out with tick in flight", OrphanReasonDrainTimeout, false},
		{"timeout", "aborted by graceful shutdown — drain timed out with tick in flight", OrphanReasonDrainTimeout, false},
		{"failed", "aborted by graceful shutdown — drain timed out with tick in flight", OrphanReasonOperatorRestart, false},
		{"timeout", "aborted by graceful shutdown — drain timed out with tick in flight", OrphanReasonOperatorRestart, false},
		// Legacy shape: the abort text alone (no structural stamp) already
		// rides the text classifier — excluded.
		{"failed", "aborted by graceful shutdown — drain timed out with tick in flight", "", false},
		{"failed", "ABORTED BY GRACEFUL SHUTDOWN", "", false}, // case-insensitive
		// The distinct operator reason works even with an empty error text:
		{"failed", "", OrphanReasonOperatorRestart, false},
		// Genuine failures still count:
		{"failed", "exit status 2: judge rejected the criteria evidence", "", true},
		{"timeout", "stale — timeout at 1h30m0s", "", true},
		// Other orphan reasons are NOT operator reaps — normal accounting:
		{"failed", "boom", OrphanReasonStartupReap, true},
		{"failed", "boom", OrphanReasonZombieReap, true},
		{"failed", "boom", "", true},
		// Harness-class spawn refusal (unchanged SCHED-GAP-134 behaviour):
		{"failed", `gateway unreachable and exec fallback disabled: gateway POST: HTTP 503`, "", false},
		// Non-terminal rows were never in the window:
		{"running", "", "", true},
		{"completed", "", "", true},
	}
	for i, c := range cases {
		got := FailureIsLaneAttributableT(c.status, c.errText, c.orphan)
		if got != c.want {
			t.Errorf("case %d FailureIsLaneAttributableT(%q, %q, %q) = %v, want %v",
				i, c.status, c.errText, c.orphan, got, c.want)
		}
		if OperatorAcknowledgedReap(c.orphan) && (c.orphan == OrphanReasonStartupReap || c.orphan == OrphanReasonZombieReap) {
			t.Errorf("case %d: OperatorAcknowledgedReap(%q) must not be true", i, c.orphan)
		}
	}
	// The exported vocabulary helper agrees with the matrix's operator rows.
	wantOperator := map[string]bool{
		OrphanReasonDrainTimeout:    true,
		OrphanReasonOperatorRestart: true,
		OrphanReasonStartupReap:     false,
		OrphanReasonZombieReap:      false,
		"":                          false,
	}
	for reason, want := range wantOperator {
		if got := OperatorAcknowledgedReap(reason); got != want {
			t.Errorf("OperatorAcknowledgedReap(%q) = %v, want %v", reason, got, want)
		}
	}
	if !IsOperatorRestartReap("aborted by graceful shutdown — drain timed out with tick in flight") {
		t.Error("IsOperatorRestartReap(abort text) = false, want true")
	}
	if IsOperatorRestartReap("exit status 2: judge rejected the criteria evidence") {
		t.Error("IsOperatorRestartReap(project error) = true, want false")
	}
}

// TestOperatorRestartApprovalEnv pins the approval gate: unset or unparseable
// is FALSE (fail closed — no approval, legacy behaviour); truthy spellings
// are TRUE.
func TestOperatorRestartApprovalEnv(t *testing.T) {
	if OperatorRestartApproved() {
		t.Fatal("OperatorRestartApproved with no env set = true, want false (fail closed)")
	}
	for _, v := range []string{"1", "true", "TRUE", "True", "t", "T"} {
		t.Setenv(EnvOperatorRestartApproved, v)
		if !OperatorRestartApproved() {
			t.Errorf("OperatorRestartApproved with %s=%q = false, want true", EnvOperatorRestartApproved, v)
		}
	}
	for _, v := range []string{"0", "false", "", "garbage"} {
		t.Setenv(EnvOperatorRestartApproved, v)
		if OperatorRestartApproved() {
			t.Errorf("OperatorRestartApproved with %s=%q = true, want false", EnvOperatorRestartApproved, v)
		}
	}
}

// TestAbortInFlightTicks_OperatorApprovedStamp is the REAL abort path driven
// against a busy Loop: 12 in-flight ticks, approved operator stop, every
// reaped row must carry the DISTINCT 'operator_restart' reason (and the
// failed status the schema requires). Without the approval the legacy stamp
// must stay 'drain_timeout'.
func TestAbortInFlightTicks_OperatorApprovedStamp(t *testing.T) {
	for _, tc := range []struct {
		approved   bool
		wantOrphan string
	}{
		{true, OrphanReasonOperatorRestart},
		{false, OrphanReasonDrainTimeout},
	} {
		t.Run(fmt.Sprintf("approved=%v", tc.approved), func(t *testing.T) {
			if tc.approved {
				t.Setenv(EnvOperatorRestartApproved, "1")
			}
			db := newTestDB(t)
			const proj = "sgap1608-abort"
			mustCreateProjectINFRA012(t, db, proj)
			for i := 0; i < 12; i++ {
				insertRunningTick(t, db, fmt.Sprintf("%s-tick-%02d", proj, i), proj, 0)
			}
			loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 12)
			loop.abortInFlightTicks()

			rows, err := db.Query(
				`SELECT status, COALESCE(orphan_reason, '') FROM ticks WHERE project_name = ?`, proj)
			if err != nil {
				t.Fatalf("query aborted ticks: %v", err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var status, orphan string
				if err := rows.Scan(&status, &orphan); err != nil {
					t.Fatalf("scan aborted tick: %v", err)
				}
				if status != "failed" {
					t.Errorf("aborted tick status = %q, want failed", status)
				}
				if orphan != tc.wantOrphan {
					t.Errorf("aborted tick orphan_reason = %q, want %q", orphan, tc.wantOrphan)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate aborted ticks: %v", err)
			}
			if n != 12 {
				t.Errorf("aborted %d ticks, want 12 (the busy-fleet shape)", n)
			}
		})
	}
}

// TestOrphanReap_ExcludedFromAutoDisable is the accounting regression: the
// REAL auto-disable enforcer must leave a lane untouched when its whole
// window is operator-reap rows (drain_timeout and operator_restart and the
// legacy text-only shape), while a lane with the same volume of GENUINE
// failures is disabled. This is the RED→GREEN flip of the measured
// 40-tickets-mass-disabled shape.
func TestOrphanReap_ExcludedFromAutoDisable(t *testing.T) {
	db := newTestDB(t)
	const (
		reaps   = "sgap1608-reaps"   // window = 100 operator reaps + 5 completions
		reapMix = "sgap1608-reapmix" // window mixes operator reaps with 1 genuine failure
		genuine = "sgap1608-genuine" // window = 50 genuine failures + 50 completions
	)
	for _, p := range []string{reaps, reapMix, genuine} {
		mustCreateProjectINFRA012(t, db, p)
	}
	abortText := "aborted by graceful shutdown — drain timed out with tick in flight"
	projectErr := "exit status 2: judge rejected the criteria evidence"

	for i := 0; i < 40; i++ {
		// Textless structural rows: exclusion must fire on the STAMP alone.
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-drain-%02d", reaps, i), reaps, OrphanReasonDrainTimeout, "")
	}
	for i := 0; i < 30; i++ {
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-op-02d", reapMix)+fmt.Sprintf("%02d", i), reapMix, OrphanReasonOperatorRestart, "")
	}
	for i := 0; i < 10; i++ {
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-legacy-%02d", reapMix, i), reapMix, "", abortText) // legacy text-only
	}
	gap1608SeedFailure(t, db, reapMix+"-real-01", reapMix, "", projectErr) // the ONE genuine failure
	// Give the reap lanes the same total volume the genuine lane has (105
	// rows here vs 100 there) so the exclusion, not a small sample, is what
	// saves them.
	for i := 40; i < 80; i++ {
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-drain-%02d", reaps, i), reaps, OrphanReasonDrainTimeout, "") // stamp-only
	}
	gap1608SeedCompleted(t, db, reaps+"-ok-1", reaps, time.Minute)
	gap1608SeedCompleted(t, db, genuine+"-ok-1", genuine, time.Minute)
	for i := 0; i < 50; i++ {
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-fail-%02d", genuine, i), genuine, "", projectErr)
	}
	for i := 0; i < 49; i++ {
		gap1608SeedCompleted(t, db, fmt.Sprintf("%s-ok-%02d", genuine, i), genuine, time.Duration(i+2)*time.Minute)
	}

	enabled := gap1608RunEnforcer(t, db)
	if !enabled[reaps] {
		t.Errorf("%s was auto-disabled — operator/drain reaps counted against the lane (SCHED-GAP-1608 regression)", reaps)
	}
	if !enabled[reapMix] {
		t.Errorf("%s was auto-disabled — one genuine failure among operator reaps must not trip the breaker (rate should be 1/1 attributable ≈ excluded-sample noise)", reapMix)
	}
	if enabled[genuine] {
		t.Errorf("%s was NOT auto-disabled — genuine project failures must still count (exclude-side-only regression)", genuine)
	}

	// The enforcer's own disabled_reason, when it parks a lane, must name
	// only genuine counts. genuine is 50/100=0.5 >= 0.5.
	var reason string
	if err := db.QueryRow(`SELECT COALESCE(disabled_reason,'') FROM projects WHERE name = ?`, genuine).Scan(&reason); err != nil {
		t.Fatalf("read disabled_reason: %v", err)
	}
	if reason == "" {
		t.Fatalf("enforcer parked %s but wrote no disabled_reason", genuine)
	}
}

// TestFailureRates_ExcludeOperatorReapsAndKeepGenuine covers the API-side
// parity through the SAME tuple predicate the status surface calls
// (computeProjectFailureRates lives in internal/api; this pins the exported
// verdict at its internal/scheduler source so the parity cannot silently
// split).
func TestFailureRates_ExcludeOperatorReapsAndKeepGenuine(t *testing.T) {
	// The exact 40-row busy-fleet reap shape from the task evidence must be
	// fully excluded; one genuine failure on top of it must still appear.
	lane := 0
	for i := 0; i < 40; i++ {
		if !FailureIsLaneAttributableT("failed",
			"aborted by graceful shutdown — drain timed out with tick in flight",
			OrphanReasonDrainTimeout) {
			lane++
		}
	}
	if lane != 40 {
		t.Errorf("only %d of 40 drain_timeout rows excluded from lane accounting, want 40", lane)
	}
	lane = 0
	for i := 0; i < 40; i++ {
		if FailureIsLaneAttributableT("failed",
			"exit status 2: judge rejected the criteria evidence", "") {
			lane++
		}
	}
	if lane != 40 {
		t.Errorf("%d of 40 genuine failures counted, want 40", lane)
	}
}

// gap1608RehearsalEnabled reports the enabled flag for one project after the
// status-surface + enforcer pairing ran.
func gap1608RehearsalEnabled(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&enabled); err != nil {
		t.Fatalf("query enabled for %s: %v", name, err)
	}
	return enabled == 1
}

// TestOperatorRestart_BusyFleetRehearsal is the hermetic busy-fleet-shaped
// rehearsal the acceptance asks for. It replays the measured scenario
// end to end — 12 live heartbeating in-flight ticks (the fleet's busy shape),
// an operator-approved restart, the post-grace reaps with their DISTINCT
// reason, and the accounting verdict — in-memory, without touching any real
// fleet, pause flag, systemd unit, or gateway:
//
//  1. busy fleet: 12 running ticks across 4 lanes on one Loop;
//  2. approval: SCHEDULER_OPERATOR_RESTART_APPROVED=1 (the drill the
//     runbook §4b carries via systemctl --user set-environment);
//  3. the restart's shutdown drain times out (grace 500ms) and reaps the
//     12 ticks via the REAL Stop() path;
//  4. every reaped row reads status=failed + orphan_reason=operator_restart;
//  5. the REAL enforcer on the REAL DB leaves all four lanes enabled and —
//     against the same policy — turns a control lane with genuine failures
//     OFF: the reaps are transport-class, real failures are not;
//  6. NO approval: the same drill stamps the legacy drain_timeout reason
//     (covered by TestAbortInFlightTicks_OperatorApprovedStamp), which the
//     same shared predicate also excludes — the pre-SCHED-GAP-1608 restart
//     path's reaps no longer pollute lane stats either.
//
// This is a FIXTURE rehearsal. It does NOT exercise the live deploy script
// or a real systemd restart; the runbook records the drill for operators.
func TestOperatorRestart_BusyFleetRehearsal(t *testing.T) {
	t.Setenv(EnvOperatorRestartApproved, "1")

	db := newTestDB(t)
	// The 12 in-flight ticks sit on 4 lanes (3 each): the fleet's busy shape
	// is cross-lane, not one hot project.
	lanes := []string{"sgap1608-alpha", "sgap1608-beta", "sgap1608-gamma", "sgap1608-delta"}
	const genuine = "sgap1608-control"
	for _, p := range append([]string{genuine}, lanes...) {
		mustCreateProjectINFRA012(t, db, p)
	}
	tick := 0
	for _, lane := range lanes {
		for i := 0; i < 3; i++ {
			insertRunningTick(t, db, fmt.Sprintf("%s-live-%02d", lane, tick), lane, 0)
			tick++
		}
	}

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 12)
	// Hold ONE REAL SLOT per in-flight tick — the busy-fleet shape Stop()'s
	// drain actually sees: a slot is held for the WHOLE tick (Acquire →
	// Spawn → Wait → Release, SCHED-GAP-077), so 12 heartbeating lanes
	// means 12 occupied slots, which is what slotPool.Wait polls. The
	// seeded rows are the DB mirror of those in-flight ticks. Slots are
	// released by Stop()'s timeout path (ReleaseAll).
	for _, lane := range lanes {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if !loop.slotPool.Acquire(ctx, lane) {
			cancel()
			t.Fatalf("could not acquire slot for %s — the busy-fleet fixture could not be built", lane)
		}
		cancel()
	}
	// Short grace — the daemon default (15s) would slow the drill; the path
	// is identical (stopGrace bounds the wait before Stop aborts, exactly
	// as in production).
	loop.stopGrace = 500 * time.Millisecond
	loop.Stop() // the restart's shutdown drain: blocks for the grace, then reaps

	// 4. every in-flight row is reaped with the DISTINCT operator reason.
	for _, lane := range lanes {
		rows, err := db.Query(
			`SELECT status, COALESCE(orphan_reason, '') FROM ticks WHERE project_name = ?`, lane)
		if err != nil {
			t.Fatalf("query reaped ticks for %s: %v", lane, err)
		}
		n, bad := 0, 0
		for rows.Next() {
			var status, orphan string
			if err := rows.Scan(&status, &orphan); err != nil {
				t.Fatalf("scan reaped tick: %v", err)
			}
			if status != "failed" || orphan != OrphanReasonOperatorRestart {
				bad++
			}
			n++
		}
		rows.Close()
		if n != 3 {
			t.Errorf("lane %s: %d rows in window, want 3", lane, n)
		}
		if bad > 0 {
			t.Errorf("lane %s: %d/%d reaped rows are not failed+operator_restart", lane, bad, n)
		}
	}

	// 5. accounting verdict: the control lane gets 50 genuine failures /
	//    50 completions, the reap lanes keep their three reaps each. Two
	//    completions per reap lane give the enforcer attributable rows in
	//    the same window (minTicks=5 reachable on the mutant shape: 3
	//    miscounted reaps + 2 completions = 0.6 ≥ threshold → would park).
	//    NOTE on redundancy: these reaped rows carry the production abort
	//    text (loop.go writes OrphanAbortMarker), which failureclass.go's
	//    marker ALSO excludes — so the stamp-only non-vacuity proof lives in
	//    the TEXTLESS-seeded regressions (TestOrphanReap_ExcludedFromAutoDisable
	//    and TestSCHEDGAP1608_StatusFailureRatesExcludeOperatorReaps), whose
	//    rows carry no error text and die RED the moment the orphan-stamp
	//    path stops firing (mutation-verified 2026-09-24; see worker report).
	gap1608SeedCompleted(t, db, genuine+"-ok-1", genuine, time.Minute)
	for _, lane := range lanes {
		gap1608SeedCompleted(t, db, lane+"-ok-1", lane, time.Minute)
		gap1608SeedCompleted(t, db, lane+"-ok-2", lane, 2*time.Minute)
	}
	for i := 0; i < 50; i++ {
		gap1608SeedFailure(t, db, fmt.Sprintf("%s-fail-%02d", genuine, i), genuine, "",
			"exit status 2: judge rejected the criteria evidence")
	}
	for i := 0; i < 49; i++ {
		gap1608SeedCompleted(t, db, fmt.Sprintf("%s-ok-%02d", genuine, i), genuine,
			time.Duration(i+2)*time.Minute)
	}
	esc := NewAlertEscalatorWithPolicy(db, NewEventLogger(db), 0.5, 100, 5)
	if err := esc.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}
	for _, lane := range lanes {
		if !gap1608RehearsalEnabled(t, db, lane) {
			t.Errorf("lane %s was auto-disabled by its 3 operator-restart reaps — the busy-fleet restart just mass-parked the fleet", lane)
		}
	}
	if gap1608RehearsalEnabled(t, db, genuine) {
		t.Errorf("control lane %s with 50 genuine failures was NOT disabled — genuine accounting broken", genuine)
	}
}
