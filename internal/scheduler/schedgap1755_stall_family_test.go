package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SCHED-GAP-1755 regression tests.
//
// The row's AC 4 asks that "a tick emitting no progress for 31m on a sync lane
// is classified 'stalled' ONLY IF the heartbeat channel is empty AND the
// deadline applies to that family". Every arm below drives the REAL decision
// path (the turn watch, the spawn classification, the family policy); only the
// MAGNITUDE is compressed — the 30m deadline becomes 250ms and the 31m of
// silence becomes a silent handler, because both are one timer on one code
// path. The 31-minute figure is the production window the census measured;
// `sgap1755SilentWindow` names it so the intent is not lost in the compression.
const sgap1755SilentWindow = 31 * time.Minute

// sgap1755SilentHandler accepts the POST and never answers — the "no progress"
// shape (no headers, no SSE, no body). It releases on test cleanup.
func sgap1755SilentHandler(requestSeen chan struct{}, release <-chan struct{}) http.HandlerFunc {
	var once atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) {
		if once.CompareAndSwap(false, true) && requestSeen != nil {
			close(requestSeen)
		}
		<-release
	}
}

// sgap1755SlowJSONHandler answers after delay with a completed non-SSE envelope
// (the gateway's buffered/un-streamed shape the satellite lanes see). Usage is
// productive-scale so the SCHED-GAP-1641/1655 classifiers stay out of the way.
func sgap1755SlowJSONHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1755","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sync done"}]}],"usage":{"input_tokens":52400,"output_tokens":6200,"total_tokens":58600}}`))
	}
}

// withStallHeartbeat swaps the liveness heartbeat channel probe for the
// duration of one test and restores it (plus any family pin) afterwards.
func withStallHeartbeat(t *testing.T, probe func(string) int64) {
	t.Helper()
	prev := stallHeartbeatProbe
	if probe != nil {
		stallHeartbeatProbe = probe
	}
	t.Cleanup(func() {
		stallHeartbeatProbe = prev
		ResetStallFamilyDeadlines()
	})
}

// TestSchedGap1755_FamilyPolicy names the mechanism (AC 1): the per-turn
// deadline is family-scoped, role-suffix (satellite) families are exempt by
// default, primary lanes keep the daemon knob.
func TestSchedGap1755_FamilyPolicy(t *testing.T) {
	// The daemon knob is 30m against an AC window of 31m of silence.
	global := sgap1755SilentWindow - time.Minute
	cases := []struct {
		lane string
		fam  string
		want time.Duration
	}{
		{"duckbrain-sync", "sync", 0},
		{"eduos-sync", "sync", 0},
		{"muster-pm", "pm", 0},
		{"off-by-one-qa", "qa", 0},
		{"mischief-chaos-dogfood", "dogfood", 0},
		{"h3-perf", "perf", 0},
		{"crier-docs", "docs", 0},
		{"bunker-readme", "readme", 0},
		{"asce-releng", "releng", 0},
		{"consensus-review", "review", 0},
		// Primary lanes: the GAP-119 shape (stream established, then quiet)
		// is theirs, so the deadline stays armed.
		{"duckbrain-foreman", StallFamilyPrimary, global},
		{"coding-hermes-tools-foreman", StallFamilyPrimary, global},
		{"my-project", StallFamilyPrimary, global},
	}
	for _, c := range cases {
		if got := StallFamilyOf(c.lane); got != c.fam {
			t.Errorf("StallFamilyOf(%q) = %q, want %q", c.lane, got, c.fam)
		}
		if got := StallDeadlineForLane(c.lane, global); got != c.want {
			t.Errorf("StallDeadlineForLane(%q) = %v, want %v (family %s)", c.lane, got, c.want, c.fam)
		}
	}
	// The census view (AC 2's data source) must carry the same resolutions.
	for _, p := range StallFamilyPolicies(global) {
		if p.Family == StallFamilyPrimary {
			if p.Exempt || p.Deadline != global || p.DeadlineLabel != global.String() {
				t.Errorf("primary policy = %+v, want armed at %v", p, global)
			}
			continue
		}
		if !p.Exempt || p.Deadline != 0 || p.DeadlineLabel != "exempt" {
			t.Errorf("family %q policy = %+v, want exempt", p.Family, p)
		}
		if !p.Heartbeat {
			t.Errorf("family %q policy reports no heartbeat channel", p.Family)
		}
	}
	// A knob that is already off stays off for every family.
	if got := StallDeadlineForLane("duckbrain-foreman", 0); got != 0 {
		t.Errorf("primary deadline with knob 0 = %v, want 0", got)
	}
}

// TestSchedGap1755_ExplicitPinReArmsFamily proves the exemption is POLICY, not
// removal: an operator (or this test) can arm a family, and the census then
// reports the armed value.
func TestSchedGap1755_ExplicitPinReArmsFamily(t *testing.T) {
	withStallHeartbeat(t, nil)
	if got := StallDeadlineForLane("duckbrain-sync", 30*time.Minute); got != 0 {
		t.Fatalf("baseline sync deadline = %v, want 0 (exempt)", got)
	}
	SetStallFamilyDeadline("sync", 7*time.Minute)
	if got := StallDeadlineForLane("duckbrain-sync", 30*time.Minute); got != 7*time.Minute {
		t.Errorf("pinned sync deadline = %v, want 7m", got)
	}
	for _, p := range StallFamilyPolicies(30 * time.Minute) {
		if p.Family != "sync" {
			continue
		}
		if p.Exempt || p.Deadline != 7*time.Minute || p.DeadlineLabel != "7m0s" {
			t.Errorf("sync policy after pin = %+v, want armed at 7m", p)
		}
	}
	// An explicit 0 keeps the family exempt.
	SetStallFamilyDeadline("sync", 0)
	if got := StallDeadlineForLane("duckbrain-sync", 30*time.Minute); got != 0 {
		t.Errorf("sync deadline after 0-pin = %v, want 0", got)
	}
}

// TestSchedGap1755_TurnWatchHeartbeatChannel is the AC 4 predicate at the
// mechanism: an idle deadline ignores a SYNC lane's silence only while its
// heartbeat channel keeps moving.
func TestSchedGap1755_TurnWatchHeartbeatChannel(t *testing.T) {
	const deadline = 120 * time.Millisecond

	// Arm 1: EMPTY channel — the deadline fires (this is "no progress AND no
	// heartbeat" ⇒ stalled, the fail-closed corner).
	empty, emptyCtx := startTurnWatch(context.Background(), deadline, func() int64 { return 0 })
	select {
	case <-emptyCtx.Done():
		// expected
	case <-time.After(4 * deadline):
		t.Fatalf("empty heartbeat channel: deadline did not fire within %v", 4*deadline)
	}
	empty.stop()
	if !empty.fired.Load() || empty.heartbeats.Load() != 0 {
		t.Fatalf("empty channel: fired=%v heartbeats=%d, want fired=true heartbeats=0",
			empty.fired.Load(), empty.heartbeats.Load())
	}

	// Arm 2: LIVE channel — the same silence, but the session keeps doing
	// work in the agent store, so the deadline must keep resetting.
	var sig atomic.Int64
	live, liveCtx := startTurnWatch(context.Background(), deadline, func() int64 {
		return sig.Add(1)
	})
	select {
	case <-liveCtx.Done():
		t.Fatalf("live heartbeat channel: deadline fired despite progress (heartbeats=%d)", live.heartbeats.Load())
	case <-time.After(8 * deadline):
		// expected: kept alive
	}
	live.stop()
	if live.fired.Load() {
		t.Errorf("live channel: fired=true, want false")
	}
	if got := live.heartbeats.Load(); got < 3 {
		t.Errorf("live channel: heartbeats=%d, want >= 3 resets over 8 deadlines", got)
	}

	// Arm 3: the channel goes STALE mid-turn — progress stops, so the
	// deadline must still fire eventually (a heartbeat is not a blanket
	// exemption).
	var fixed atomic.Bool
	var stale atomic.Int64
	staleWatch, staleCtx := startTurnWatch(context.Background(), deadline, func() int64 {
		if fixed.Load() {
			return 42 // frozen signature: no new work
		}
		return stale.Add(1)
	})
	time.Sleep(3 * deadline)
	fixed.Store(true)
	select {
	case <-staleCtx.Done():
		// expected: the frozen channel is an empty one
	case <-time.After(6 * deadline):
		staleWatch.stop()
		t.Fatalf("stale heartbeat channel never fired — a heartbeat must not exempt a silent turn forever")
	}
	staleWatch.stop()
	if got := staleWatch.heartbeats.Load(); got < 1 {
		t.Errorf("stale channel: heartbeats=%d, want >= 1 before it froze", got)
	}
}

// TestSchedGap1755_SyncLaneOutlivesDeadlineWithoutStall is AC 4 arm A: a sync
// lane whose POST emits nothing for longer than the per-turn knob completes
// instead of being failed as stalled, because the deadline does not apply to
// its family.
func TestSchedGap1755_SyncLaneOutlivesDeadlineWithoutStall(t *testing.T) {
	withStallHeartbeat(t, func(string) int64 { return 0 })
	db := newTestDB(t)
	const projectName = "duckbrain-sync"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := httptest.NewServer(sgap1755SlowJSONHandler(600 * time.Millisecond))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond) // knob << turn wall

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1755-sync-exempt-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	elapsed := time.Since(start)

	if outcome.Status != TickCompleted {
		t.Errorf("sync lane status = %s, want %s (error=%q) — the per-turn deadline must not apply to the sync family",
			outcome.Status, TickCompleted, outcome.Error)
	}
	if strings.Contains(outcome.Error, "stalled") {
		t.Errorf("sync lane recorded %q — a family-exempt lane must never carry the stall class", outcome.Error)
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("tick resolved in %v, want >= the handler's 600ms — the POST was not actually left alone", elapsed)
	}
}

// TestSchedGap1755_ArmedFamilyStallsOnEmptyChannel is AC 4 arms B and D: the
// SAME sync lane is classified "stalled" once the deadline is armed for its
// family AND the heartbeat channel is empty. Nothing else changed.
func TestSchedGap1755_ArmedFamilyStallsOnEmptyChannel(t *testing.T) {
	withStallHeartbeat(t, func(string) int64 { return 0 })
	SetStallFamilyDeadline("sync", 250*time.Millisecond) // arm the family explicitly

	db := newTestDB(t)
	const projectName = "eduos-sync"
	mustCreateProjectINFRA012(t, db, projectName)

	release := make(chan struct{})
	seen := make(chan struct{})
	srv := httptest.NewServer(sgap1755SilentHandler(seen, release))
	// Order matters: httptest's Close blocks until outstanding handlers
	// return, and this handler returns only when release closes — so the
	// release must be registered AFTER (i.e. run BEFORE) the server close.
	defer srv.Close()
	defer close(release)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(30 * time.Minute) // daemon knob; the pin wins for sync

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1755-sync-armed-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Spawn blocked %v — the armed family deadline did not abort the silent POST", elapsed)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("status = %s, want %s", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") || !strings.Contains(outcome.Error, "no progress") {
		t.Errorf("error = %q, want the GAP-117 'stalled: no progress' contract text", outcome.Error)
	}
	if !strings.Contains(outcome.Error, `family "sync"`) {
		t.Errorf("error = %q, want the stalled classification to NAME the family it applied to", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "heartbeat resets 0") {
		t.Errorf("error = %q, want the empty-heartbeat evidence in the classification", outcome.Error)
	}
}

// TestSchedGap1755_PrimaryLaneHeartbeatSparesTurn is AC 4 arm C: a PRIMARY lane
// whose deadline applies, whose POST emits nothing, but whose gateway session
// keeps doing work (the heartbeat channel moves) must NOT be failed as stalled.
func TestSchedGap1755_PrimaryLaneHeartbeatSparesTurn(t *testing.T) {
	var sig atomic.Int64
	// Every read returns a NEW signature = the session keeps producing work.
	withStallHeartbeat(t, func(string) int64 { return sig.Add(100) })
	db := newTestDB(t)
	const projectName = "duckbrain-foreman"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := httptest.NewServer(sgap1755SlowJSONHandler(900 * time.Millisecond))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1755-primary-beat-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("status = %s (error=%q), want %s — a live heartbeat channel must spare the turn",
			outcome.Status, outcome.Error, TickCompleted)
	}
	if strings.Contains(outcome.Error, "stalled") {
		t.Errorf("error = %q, want no stall class while the heartbeat channel moves", outcome.Error)
	}
}

// TestSchedGap1755_PrimaryLaneEmptyChannelStalls pins the AND-ness from the
// other side: the primary lane keeps the deadline, and with an EMPTY channel the
// silence is still a stall (no weakening of the GAP-117/119 guarantee).
func TestSchedGap1755_PrimaryLaneEmptyChannelStalls(t *testing.T) {
	withStallHeartbeat(t, func(string) int64 { return 0 })
	db := newTestDB(t)
	const projectName = "monitoring-foreman"
	mustCreateProjectINFRA012(t, db, projectName)

	release := make(chan struct{})
	seen := make(chan struct{})
	srv := httptest.NewServer(sgap1755SilentHandler(seen, release))
	// Same defer ordering as the armed-family test: release before close.
	defer srv.Close()
	defer close(release)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1755-primary-silent-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Spawn blocked %v — the primary-lane deadline did not abort the silent POST", elapsed)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("status = %s, want %s", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") {
		t.Errorf("error = %q, want the stall class on an empty heartbeat channel", outcome.Error)
	}
	if !strings.Contains(outcome.Error, `family "primary"`) {
		t.Errorf("error = %q, want the primary family named", outcome.Error)
	}
}
