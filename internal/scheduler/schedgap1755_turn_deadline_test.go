package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-1755 regression suite — the 30m per-turn "stalled" deadline vs the
// sync family.
//
// LIVE DEFECT this pins: the gateway /v1/responses per-turn deadline
// (SCHED-GAP-117/119, default 30m) accounted for 55 of 140 failed ticks in a
// 24h window, and duckbrain-sync carried 55 failed / 55 completed (50%).
// Mechanism: a sync lane's product is long DuckBrain HTTP writes. While the
// agent sits inside one write the SSE stream emits NOTHING — no
// output_text.delta, no tool event — so the idle deadline (which resets only
// on real SSE events) measures a productive turn as a hung one and kills it at
// exactly 30m. The family has no progress channel the deadline can read.
//
// FIX (the row's approach (a)): the per-turn deadline is resolved
// PER-NAMESPACE from namespaces.turn_deadline. '0s' = the family is EXEMPT
// (the POST runs on the tick deadline alone — pre-117 behavior); '' = inherit
// the global knob; a positive duration overrides it for that family.
// migration v73 arms duckbrain-sync to '0s'. Every other family keeps the
// 30m deadline, byte-identical.
//
// AC 4's contract, restated here so the tests read against it: a tick that
// emits no progress for 31m on a sync lane is classified "stalled" ONLY IF
// the heartbeat channel is empty AND the deadline applies to that family.
// The silence in these tests is total (zero SSE events = an empty heartbeat
// channel), so the two arms isolate the SECOND conjunct:
//
//   - deadline does NOT apply (duckbrain-sync, '0s')  → NOT stalled;
//   - deadline DOES apply (any other family)          → stalled.
//
// The production window (31m vs 30m) is scaled to 600ms vs 250ms — same
// arithmetic, 1/7440th of the wall clock, so the suite stays fast while the
// assertion shape is identical.

// mustCreateNamespaceWithTurnDeadline inserts a namespace row with the given
// turn_deadline value and assigns projectName to it. projectName must already
// exist. "" means inherit the global knob (the pre-1755 shape).
func mustCreateNamespaceWithTurnDeadline(t *testing.T, db *sql.DB, nsID, turnDeadline, projectName string) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO namespaces (id) VALUES (?)`, nsID); err != nil {
		t.Fatalf("insert namespace %s: %v", nsID, err)
	}
	if _, err := db.Exec(`UPDATE namespaces SET turn_deadline = ? WHERE id = ?`, turnDeadline, nsID); err != nil {
		t.Fatalf("set turn_deadline for %s: %v", nsID, err)
	}
	if projectName != "" {
		if _, err := db.Exec(`UPDATE projects SET namespace_id = ? WHERE name = ?`, nsID, projectName); err != nil {
			t.Fatalf("assign %s to namespace %s: %v", projectName, nsID, err)
		}
	}
}

// silentThenCompleteGateway answers /v1/responses after a deliberate silence
// (no bytes at all — an empty SSE/idle heartbeat channel by construction, the
// duckbrain-sync write shape) and then returns a completed envelope. With
// supervision armed the POST is aborted mid-silence; exempt, it completes.
func silentThenCompleteGateway(silence time.Duration) (*httptest.Server, *bool) {
	reached := new(bool)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(silence)
		if r.Context().Err() != nil {
			return // the caller (scheduler) tore the request down
		}
		mu.Lock()
		*reached = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_sgap1755","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sync done"}]}],"usage":{"input_tokens":51200,"output_tokens":8400,"total_tokens":59600}}`)
	}))
	return srv, reached
}

// TestSgap1755_SyncLaneSilentTurnNotStalled is the primary AC 4 arm: a
// duckbrain-sync lane (namespace turn_deadline='0s', migration v73's value)
// whose turn emits NO SSE activity for 600ms — 2.4x the 250ms per-turn
// deadline, the scaled 31m-vs-30m shape — must NOT be classified "stalled".
// The POST runs to completion on the tick deadline, the tick is status
// completed, and the row's error is empty.
func TestSgap1755_SyncLaneSilentTurnNotStalled(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1755-sync-lane"
	mustCreateProjectINFRA012(t, db, projectName)
	mustCreateNamespaceWithTurnDeadline(t, db, "duckbrain-sync", "0s", projectName)

	srv, reached := silentThenCompleteGateway(600 * time.Millisecond)
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second                        // tick wall far above the silence
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond) // global knob: 2.4x BELOW the silence

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{
		Name:        projectName,
		Workdir:     t.TempDir(),
		NamespaceID: "duckbrain-sync",
	}, "sgap1755-sync-exempt-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()

	if outcome.Status != TickCompleted {
		t.Fatalf("Wait() status = %s (error %q), want %s — an EXEMPT sync lane must not be failed by the per-turn deadline",
			outcome.Status, outcome.Error, TickCompleted)
	}
	if strings.Contains(outcome.Error, "stalled") {
		t.Errorf("Wait() error = %q — a duckbrain-sync tick (turn_deadline=0s) must never carry the stalled classification", outcome.Error)
	}
	if !*reached {
		t.Errorf("the gateway never saw the POST complete — the turn was aborted despite the family exemption")
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Errorf("turn completed in %v, want >= 600ms — the silent window must actually have been sat through", elapsed)
	}
}

// TestSgap1755_NonExemptFamilySilentTurnStalls is the other AC 4 arm — the
// NEGATIVE control that keeps the deadline honest for every family that has
// not been exempted. Same silent gateway, same 250ms knob, but the project
// lives in a namespace with turn_deadline=” (inherit): the deadline DOES
// apply, the heartbeat channel IS empty, so the tick must be classified
// "stalled" exactly as before SCHED-GAP-1755.
func TestSgap1755_NonExemptFamilySilentTurnStalls(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1755-plain-lane"
	mustCreateProjectINFRA012(t, db, projectName)
	mustCreateNamespaceWithTurnDeadline(t, db, "qa", "", projectName)

	srv, _ := silentThenCompleteGateway(5 * time.Second) // never reached: the deadline aborts first
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{
		Name:        projectName,
		Workdir:     t.TempDir(),
		NamespaceID: "qa",
	}, "sgap1755-plain-stall-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s, want %s — a non-exempt family keeps the 30m-class stall classification",
			outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") || !strings.Contains(outcome.Error, "no progress") {
		t.Errorf("Wait() error = %q, want the 'stalled: no progress' contract text", outcome.Error)
	}
	// The contract text must name the EFFECTIVE deadline that fired (the
	// global knob here) — the census class text and the row agree.
	if !strings.Contains(outcome.Error, "250ms") {
		t.Errorf("Wait() error = %q, want it to name the 250ms deadline that fired", outcome.Error)
	}
}

// TestSgap1755_NamespaceDeadlineOverride pins the middle rung of the
// vocabulary: a namespace with a POSITIVE turn_deadline overrides the global
// knob for its lanes only. The global knob is 5s (never fires); the
// namespace's 250ms must be the value that fires, and the row's contract text
// must name it.
func TestSgap1755_NamespaceDeadlineOverride(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1755-override-lane"
	mustCreateProjectINFRA012(t, db, projectName)
	mustCreateNamespaceWithTurnDeadline(t, db, "pm", "250ms", projectName)

	srv, _ := silentThenCompleteGateway(5 * time.Second)
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(5 * time.Second) // global: would never fire inside this test

	tick, err := spawner.Spawn(PackedProject{
		Name:        projectName,
		Workdir:     t.TempDir(),
		NamespaceID: "pm",
	}, "sgap1755-override-2026-10-22-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s, want %s — the namespace override must fire, not the 5s global knob",
			outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "no progress for 250ms") {
		t.Errorf("Wait() error = %q, want the namespace's own 250ms deadline named in the contract text", outcome.Error)
	}
}

// TestSgap1755_TurnDeadlineResolver pins the resolver's vocabulary directly
// (no gateway): ” inherits the global knob (ok=false), '0s' means EXEMPT
// (0, ok=true), a positive duration returns itself, garbage and negative
// values fall open to the global knob, and a lane with no namespace inherits.
func TestSgap1755_TurnDeadlineResolver(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-plain")
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-nons")
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-exempt")
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-override")
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-garbage")
	mustCreateProjectINFRA012(t, db, "sgap1755-resolver-negative")

	for _, row := range []struct{ ns, val, project string }{
		{"ns-inherit", "", "sgap1755-resolver-plain"},
		{"ns-exempt", "0s", "sgap1755-resolver-exempt"},
		{"ns-override", "90m", "sgap1755-resolver-override"},
		{"ns-garbage", "soon", "sgap1755-resolver-garbage"},
		{"ns-negative", "-5m", "sgap1755-resolver-negative"},
	} {
		mustCreateNamespaceWithTurnDeadline(t, db, row.ns, row.val, row.project)
	}

	cases := []struct {
		project string
		wantDur time.Duration
		wantOK  bool
		explain string
	}{
		{"sgap1755-resolver-plain", 0, false, "'' inherits the global 30m knob"},
		{"sgap1755-resolver-nons", 0, false, "a project with no namespace inherits"},
		{"sgap1755-resolver-exempt", 0, true, "'0s' is the explicit exempt"},
		{"sgap1755-resolver-override", 90 * time.Minute, true, "a positive duration overrides"},
		{"sgap1755-resolver-garbage", 0, false, "garbage falls open to the global knob"},
		{"sgap1755-resolver-negative", 0, false, "a negative duration falls open to the global knob"},
	}
	for _, tc := range cases {
		got, ok := turnDeadlineOf(ctx, db, tc.project)
		if ok != tc.wantOK || got != tc.wantDur {
			t.Errorf("turnDeadlineOf(%s) = (%v, %v), want (%v, %v) — %s",
				tc.project, got, ok, tc.wantDur, tc.wantOK, tc.explain)
		}
	}
}
