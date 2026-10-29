package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-16939 — stall → next-hop advance.
//
// LIVE DEFECT pinned here: 9 of 15 failures in a measured 3h window died as
// "stalled: no progress for 30m0s (gateway /v1/responses)". One gateway call
// per tick meant one stalled hop killed the tick, even though the merged chain
// held other hops that could serve it.
//
// The falsifier (AC 4) is the stub below: hop 1 accepts the POST and emits
// NOTHING (the real wedged-hop shape), hop 2 answers immediately. Before this
// change the tick fails at the per-turn deadline; after it, the tick advances
// and completes on hop 2 in under 2x the deadline, with both hops on the row.
//
// The production deadline is 30m; every arm here scales it to 300-400ms — the
// same arithmetic at 1/4500th of the wall clock.

// hopStallGateway is a two-faced /v1/responses stub: the model named in
// stallOn accepts the POST, opens the stream and then emits nothing until the
// CLIENT aborts (r.Context().Done()), which is also how the test observes that
// the abort actually reached the server. Every other model answers with a
// completed SSE envelope.
type hopStallGateway struct {
	srv     *httptest.Server
	stallOn string

	mu          sync.Mutex
	posted      []string // models in POST order
	stallCancel bool     // the stalled POST's context was canceled by the client
	stallReturn bool     // the stalled handler's goroutine returned
}

func newHopStallGateway(stallOn string) *hopStallGateway {
	g := &hopStallGateway{stallOn: stallOn}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		g.mu.Lock()
		g.posted = append(g.posted, req.Model)
		g.mu.Unlock()

		if req.Model == g.stallOn {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// NO events, no keepalive: the byte-silent hang class.
			<-r.Context().Done()
			g.mu.Lock()
			g.stallCancel = true
			g.stallReturn = true
			g.mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{"+
			"\"id\":\"resp_hop2\",\"status\":\"completed\","+
			"\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hop 2 served the tick\"}]}],"+
			"\"usage\":{\"input_tokens\":4200,\"output_tokens\":320,\"total_tokens\":4520}}}\n\n")
	}))
	return g
}

func (g *hopStallGateway) close() { g.srv.Close() }

func (g *hopStallGateway) modelsPosted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.posted))
	copy(out, g.posted)
	return out
}

func (g *hopStallGateway) stalledRequestAborted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stallCancel
}

// TestSchedGap16939_TwoHopStallAdvancesAndCompletes is the row's FALSIFIER
// (AC 4). Two-hop chain, hop 1 byte-silent, hop 2 healthy:
//   - the tick must COMPLETE (pre-change it fails with the stalled class),
//   - it must complete in less than 2x the per-turn deadline,
//   - the gateway must have been POSTed for BOTH models,
//   - and the stalled hop's POST must have been aborted at the socket (the
//     stub's handler sees r.Context().Done()), i.e. the scheduler did not
//     leave a live request burning upstream.
func TestSchedGap16939_TwoHopStallAdvancesAndCompletes(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap16939-two-hop"
	mustCreateProjectINFRA012(t, db, projectName)

	gw := newHopStallGateway("hop1-model")
	defer gw.close()

	const deadline = 400 * time.Millisecond
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(gw.srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(deadline)

	const tickID = "sgap16939-two-hop-2026-10-29-00-00-00"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, 'queued', ?, ?)`,
		tickID, projectName, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		ModelChain: `["hop1-model@prov-a","hop2-model@prov-b"]`,
	}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	elapsed := time.Since(start)

	if outcome.Status != TickCompleted {
		t.Fatalf("Wait() status = %s (error %q), want %s — a stalled hop 1 with a healthy hop 2 must ADVANCE, not kill the tick",
			outcome.Status, outcome.Error, TickCompleted)
	}
	if strings.Contains(outcome.Error, "stalled") {
		t.Errorf("Wait() error = %q — an advanced tick must not carry the stalled classification", outcome.Error)
	}
	if elapsed >= 2*deadline {
		t.Errorf("tick completed in %v, want < %v (2x the %v per-turn deadline) — the advance must not spend a second full stall window",
			elapsed, 2*deadline, deadline)
	}
	posted := gw.modelsPosted()
	if len(posted) != 2 || posted[0] != "hop1-model" || posted[1] != "hop2-model" {
		t.Errorf("gateway POST order = %v, want [hop1-model hop2-model]", posted)
	}
	if !gw.stalledRequestAborted() {
		t.Errorf("the stalled hop's POST was never aborted at the socket — the scheduler moved on while the upstream request stayed live")
	}
}

// TestSchedGap16939_AttemptsRecordBothHops pins AC 3 on the ROW (not the log):
// a stall-then-advance tick must name both hops, so a reader distinguishes it
// from a first-try success without parsing scheduler.log.
func TestSchedGap16939_AttemptsRecordBothHops(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap16939-attempts"
	mustCreateProjectINFRA012(t, db, projectName)

	gw := newHopStallGateway("hop1-model")
	defer gw.close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(gw.srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(300 * time.Millisecond)

	const tickID = "sgap16939-attempts-2026-10-29-00-00-00"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, 'queued', ?, ?)`,
		tickID, projectName, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	tick, err := spawner.Spawn(PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		ModelChain: `["hop1-model@prov-a","hop2-model@prov-b"]`,
	}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if outcome := tick.Wait(); outcome.Status != TickCompleted {
		t.Fatalf("Wait() status = %s (error %q), want %s", outcome.Status, outcome.Error, TickCompleted)
	}

	var raw string
	if err := db.QueryRow(`SELECT COALESCE(gateway_attempts,'') FROM ticks WHERE id = ?`, tickID).Scan(&raw); err != nil {
		t.Fatalf("read gateway_attempts: %v", err)
	}
	if raw == "" {
		t.Fatalf("ticks.gateway_attempts is empty — the tick completed on hop 2 but the row cannot say it stalled on hop 1")
	}
	var rows []hopAttempt
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("gateway_attempts is not a JSON array: %v (%s)", err, raw)
	}
	if len(rows) != 2 {
		t.Fatalf("gateway_attempts has %d rows (%s), want 2 — one per hop POSTed", len(rows), raw)
	}
	if rows[0].Hop != 0 || rows[0].Model != "hop1-model" || rows[0].Provider != "prov-a" || rows[0].Reason != hopReasonIdleStall {
		t.Errorf("attempts[0] = %+v, want hop 0 hop1-model/prov-a reason=%s", rows[0], hopReasonIdleStall)
	}
	if rows[1].Hop != 1 || rows[1].Model != "hop2-model" || rows[1].Provider != "prov-b" || rows[1].Reason != hopReasonCompleted {
		t.Errorf("attempts[1] = %+v, want hop 1 hop2-model/prov-b reason=%s", rows[1], hopReasonCompleted)
	}
	// The stalled attempt's duration must be the deadline it actually burned,
	// not a fabricated zero.
	if rows[0].DurationMS < 200 {
		t.Errorf("attempts[0].duration_ms = %d, want >= the ~300ms deadline it waited out", rows[0].DurationMS)
	}
}

// TestSchedGap16939_SingleHopStallStillFails is the CONTROL: the same silent
// hop with nowhere to advance must keep the pre-change behavior byte-for-byte
// (status failed, the "stalled: no progress" contract text naming the effective
// deadline, one attempt row). Without this arm the advance could be masking a
// stall classification regression.
func TestSchedGap16939_SingleHopStallStillFails(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap16939-single-hop"
	mustCreateProjectINFRA012(t, db, projectName)

	gw := newHopStallGateway("hop1-model")
	defer gw.close()

	const deadline = 300 * time.Millisecond
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(gw.srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(deadline)

	const tickID = "sgap16939-single-hop-2026-10-29-00-00-00"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, 'queued', ?, ?)`,
		tickID, projectName, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	tick, err := spawner.Spawn(PackedProject{
		Name:             projectName,
		Workdir:          t.TempDir(),
		ModelChain:       `["hop1-model@prov-a"]`,
		NoGlobalFallback: true,
	}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s (error %q), want %s — a single-hop chain has nowhere to advance",
			outcome.Status, outcome.Error, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled: no progress for") {
		t.Errorf("Wait() error = %q, want the unchanged 'stalled: no progress' contract text", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "300ms") {
		t.Errorf("Wait() error = %q, want it to name the 300ms deadline that fired", outcome.Error)
	}
	if posted := gw.modelsPosted(); len(posted) != 1 {
		t.Errorf("gateway POSTs = %v, want exactly 1 — no advance is possible", posted)
	}
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(gateway_attempts,'') FROM ticks WHERE id = ?`, tickID).Scan(&raw); err != nil {
		t.Fatalf("read gateway_attempts: %v", err)
	}
	var rows []hopAttempt
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("gateway_attempts is not a JSON array: %v (%s)", err, raw)
	}
	if len(rows) != 1 || rows[0].Reason != hopReasonIdleStall {
		t.Errorf("gateway_attempts = %s, want one idle-stall row", raw)
	}
}

// TestSchedGap16939_HopListOrderAndExclusion pins the pure resolution rules the
// advance rests on: the dispatched pair is hop 0 even when the router head is
// off-chain, pairs are de-duplicated, and the exclusion set is PER TICK — the
// same chain resolves the same head for the next tick (AC 6: no lane
// blacklist, no durable state).
func TestSchedGap16939_HopListOrderAndExclusion(t *testing.T) {
	chain := []chainEntry{
		{model: "m-a", provider: "p-a"},
		{model: "m-b", provider: "p-b"},
		{model: "m-a", provider: "p-a"}, // duplicate of hop 0
	}

	// The dispatched pair is the router head, which sits OUTSIDE the chain.
	hops := hopList(chain, chainEntry{model: "head", provider: "p-head"})
	if len(hops) != 3 {
		t.Fatalf("hopList = %+v, want 3 distinct pairs (head, m-a, m-b)", hops)
	}
	if hops[0].model != "head" || hops[0].provider != "p-head" {
		t.Errorf("hops[0] = %+v, want the dispatched pair forced to position 0", hops[0])
	}
	if hopIndexOf(hops, "m-b", "p-b") != 2 {
		t.Errorf("hopIndexOf(m-b) = %d, want 2", hopIndexOf(hops, "m-b", "p-b"))
	}
	if hopIndexOf(hops, "off", "chain") != -1 {
		t.Errorf("hopIndexOf(off-chain pair) = %d, want -1", hopIndexOf(hops, "off", "chain"))
	}

	// The head equals chain[0] in the common case (no router head).
	plain := hopList(chain, chainEntry{model: "m-a", provider: "p-a"})
	if len(plain) != 2 || plain[0].model != "m-a" || plain[1].model != "m-b" {
		t.Fatalf("hopList(no router head) = %+v, want [m-a m-b]", plain)
	}

	log := &tickHopLog{}
	log.add(hopAttempt{Hop: 0, Model: "m-a", Provider: "p-a", Reason: hopReasonIdleStall})
	next, hop, ok := nextHopAfter(plain, log.attemptedPairs())
	if !ok || hop != 1 || next.model != "m-b" || next.provider != "p-b" {
		t.Fatalf("nextHopAfter = (%+v, %d, %v), want hop 1 m-b/p-b", next, hop, ok)
	}
	if !log.pairAttempted("p-a", "m-a") || log.pairAttempted("p-b", "m-b") {
		t.Errorf("per-tick exclusion set is wrong: %v", log.attemptedPairs())
	}
	log.add(hopAttempt{Hop: 1, Model: "m-b", Provider: "p-b", Reason: hopReasonTransport})
	if _, _, ok := nextHopAfter(plain, log.attemptedPairs()); ok {
		t.Errorf("nextHopAfter on an exhausted chain returned ok=true, want false")
	}

	// AC 6: the exclusion is per TICK — a fresh tick log on the same chain
	// still offers the pair the previous tick stalled on.
	fresh := &tickHopLog{}
	if got, hop, ok := nextHopAfter(plain, fresh.attemptedPairs()); !ok || hop != 0 || got.model != "m-a" {
		t.Errorf("a fresh tick resolved (%+v, %d, %v), want the chain head m-a — a stall must not blacklist the lane",
			got, hop, ok)
	}
}

// TestSchedGap16939_ClassifyHopErr pins the attempt vocabulary: a tick-wall
// abort is never mislabelled as a stall, and an auth rejection is its own
// reason rather than a generic gateway error.
func TestSchedGap16939_ClassifyHopErr(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		stall  bool
		expect string
	}{
		{"completed", nil, false, hopReasonCompleted},
		{"idle stall", &TurnDeadlineError{After: time.Second}, true, hopReasonIdleStall},
		{"tick wall", fmt.Errorf("%w: session context deadline expired", ErrTickDeadlineExceeded), false, hopReasonTickWall},
		{"context deadline", context.DeadlineExceeded, false, hopReasonTickWall},
		{"auth", fmt.Errorf("%w (HTTP 401): nope", ErrGatewayKeyRejected), false, hopReasonAuth},
		{"transport", &GatewayStatusError{StatusCode: 503, msg: "gateway POST: HTTP 503"}, false, hopReasonTransport},
		{"plain gateway", errors.New("gateway error: server_error — boom"), false, hopReasonGateway},
	}
	for _, tc := range cases {
		if got := classifyHopErr(tc.err, tc.stall); got != tc.expect {
			t.Errorf("%s: classifyHopErr = %q, want %q", tc.name, got, tc.expect)
		}
	}
}

// TestSchedGap16939_ClientAbortReleasesUpstream is AC 1, measured at the seam
// the spawn path actually uses: a stalled POST must be aborted by the per-turn
// watch AND the abort must reach the server (the handler's own context is
// canceled and the handler returns) — otherwise the "stalled" row is a local
// fiction hiding a live upstream request.
//
// The control arm is the same stub with the deadline DISABLED (turnDeadline 0):
// no abort, the handler runs to its own completion. That proves the cancellation
// observed above is the watch's doing and not an artifact of the stub.
func TestSchedGap16939_ClientAbortReleasesUpstream(t *testing.T) {
	type probe struct {
		srv       *httptest.Server
		cancelMu  sync.Mutex
		cancelled bool
		returned  chan struct{}
	}
	newProbe := func(hold time.Duration) *probe {
		p := &probe{returned: make(chan struct{})}
		p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				p.cancelMu.Lock()
				p.cancelled = true
				p.cancelMu.Unlock()
			case <-time.After(hold):
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_slow\",\"status\":\"completed\"}}\n\n")
			}
			close(p.returned)
		}))
		return p
	}
	sawCancel := func(p *probe) bool {
		p.cancelMu.Lock()
		defer p.cancelMu.Unlock()
		return p.cancelled
	}

	// Arm: the watchdog aborts a byte-silent POST at 200ms.
	p := newProbe(5 * time.Second)
	defer p.srv.Close()
	cli := NewGatewayClient(p.srv.URL, "***", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, _, err := cli.SendResponseStream(ctx, "prompt", "m", "p", "k", "tick-abort", 200*time.Millisecond)
	elapsed := time.Since(start)
	var tde *TurnDeadlineError
	if !errors.As(err, &tde) {
		t.Fatalf("SendResponseStream error = %v, want *TurnDeadlineError", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("abort took %v, want ~200ms — the watch must fire on its own deadline", elapsed)
	}
	select {
	case <-p.returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("the server handler never returned — the aborted request stayed live server-side")
	}
	if !sawCancel(p) {
		t.Errorf("the server never observed its request context being canceled — the abort did not reach the wire")
	}

	// Control: turnDeadline 0 = no watchdog. The same silent stub must NOT be
	// aborted; it answers on its own schedule.
	c := newProbe(300 * time.Millisecond)
	defer c.srv.Close()
	ccli := NewGatewayClient(c.srv.URL, "***", 30*time.Second)
	cctx, ccancel := context.WithCancel(context.Background())
	defer ccancel()
	resp, _, cerr := ccli.SendResponseStream(cctx, "prompt", "m", "p", "k", "tick-control", 0)
	if cerr != nil {
		t.Fatalf("control arm: SendResponseStream error = %v, want the stub's own completion", cerr)
	}
	if resp == nil || resp.ID != "resp_slow" {
		t.Errorf("control arm returned %+v, want the stub's completed envelope", resp)
	}
	if sawCancel(c) {
		t.Errorf("control arm: the stub was canceled with the watchdog DISABLED — the arm→abort mapping is not what this test claims")
	}
}
