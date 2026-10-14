package api_test

// REMOTE-012 acceptance battery (docs/federation-query-spec.md §5, the
// aggregate). The cells map 1:1 to the row's acceptance list:
//
//   - TestREMOTE012_AggregateMergesPerPeerWithStatusPreserved — N stubbed
//     peers merge with per-peer status preserved (a peer's partial stays
//     partial on its row and drops the aggregate to partial, §5).
//   - TestREMOTE012_SlowPeerDoesNotHoldAnswerHostage — one hanging peer
//     never holds the answer: the others still return inside the
//     aggregate's own ceiling.
//   - TestREMOTE012_AllPeersSilentStillAnswers — every peer silent STILL
//     yields a non-empty answer, everything stale/error, gaps named; the
//     word "down" appears nowhere (the registry rendering law).
//   - the remaining cells pin the addressing laws: refusals, the empty
//     registry (peer_count = self only), replay byte-identity, the
//     fail-closed gate, and the catalogue's six-op purity (peers are never
//     asked to aggregate — spec §7).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// aggStubTransport builds an ask-side transport that dispatches per peer.
// The map's error arm (missing peer) fails the leg loudly — a test that
// reaches an unstubbed peer asked for the wrong registry.
func aggStubTransport(
	respond func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error),
) api.AggregateTransport {
	return func(ctx context.Context, peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		return respond(peerID, q)
	}
}

// aggOKEnvelope is a minimal §2.2 ok answer a stubbed peer returns.
func aggOKEnvelope(op string, data map[string]any) bus.ResponseEnvelope {
	return bus.ResponseEnvelope{
		CorrID:   "stub-corr",
		Op:       op,
		Peer:     "stub",
		Status:   "ok",
		AsOf:     "2026-10-02T00:00:00Z",
		AgeMS:    0,
		Data:     data,
		Gaps:     []bus.FederationGap{},
		Contract: "1.0.0",
	}
}

// registerAggPeer registers a peer and stamps a fresh heartbeat when fresh
// is true (a never-heartbeat peer stays "" — the honest stale arm).
func registerAggPeer(t *testing.T, a *apiTestServer, id string, fresh bool) {
	t.Helper()
	if err := database.UpsertPeer(context.Background(), a.db, &database.Peer{
		ID:           id,
		URL:          "http://127.0.0.1:1",
		Capabilities: "query",
	}); err != nil {
		t.Fatalf("UpsertPeer %s: %v", id, err)
	}
	if fresh {
		if err := database.PeerHeartbeat(context.Background(), a.db, id); err != nil {
			t.Fatalf("PeerHeartbeat %s: %v", id, err)
		}
	}
}

// aggAsk posts one aggregate envelope with the operator credential and
// returns the status + the parsed envelope.
func aggAsk(t *testing.T, a *apiTestServer, corrID string, aggArgs map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"op": "fleet.aggregate", "corr_id": corrID, "args": aggArgs}
	status, raw := fedQuery(t, a, body)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("aggregate response not JSON: %v\nbody: %s", err, raw)
	}
	return status, env
}

// aggRows extracts the peers rows + the answers map from an aggregate
// envelope's data member.
func aggRows(t *testing.T, env map[string]any) (rows []map[string]any, answers map[string]any) {
	t.Helper()
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("aggregate data member missing or not an object: %v", env["data"])
	}
	rowsAny, ok := data["peers"].([]any)
	if !ok {
		t.Fatalf("aggregate data.peers missing: %v", data)
	}
	rows = make([]map[string]any, 0, len(rowsAny))
	for _, r := range rowsAny {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("peer row not an object: %v", r)
		}
		rows = append(rows, rm)
	}
	answers, _ = data["answers"].(map[string]any)
	return rows, answers
}

// aggRowsByPeer indexes the rows by their peer id.
func aggRowsByPeer(t *testing.T, env map[string]any) map[string]map[string]any {
	t.Helper()
	rows, _ := aggRows(t, env)
	out := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		id, _ := r["peer"].(string)
		out[id] = r
	}
	return out
}

// TestREMOTE012_AggregateMergesPerPeerWithStatusPreserved is acceptance
// cell 1: the aggregate over N stubbed peers merges their answers (per-peer
// data preserved under answers, keyed by peer id) WITH per-peer status
// preserved — a peer that answered partial keeps status=partial on its row
// and drops the aggregate to partial with the gap named (§5 merge rules).
func TestREMOTE012_AggregateMergesPerPeerWithStatusPreserved(t *testing.T) {
	a := newAPITestServer(t)
	registerAggPeer(t, a, "agg-alpha", true)
	registerAggPeer(t, a, "agg-beta", true)
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		switch peerID {
		case "agg-alpha":
			env := aggOKEnvelope(q.Op, map[string]any{"lane": peerID, "queue": []string{"t1", "t2"}})
			env.Peer = peerID
			return env, nil
		case "agg-beta":
			// A degraded answer: the peer answered partial with its own
			// gap — the merge must PRESERVE that status on the row.
			env := aggOKEnvelope(q.Op, map[string]any{"lane": peerID})
			env.Peer = peerID
			env.Status = "partial"
			env.Gaps = []bus.FederationGap{{What: "budget", Why: "peer read hit its own budget"}}
			return env, nil
		}
		return bus.ResponseEnvelope{}, fmt.Errorf("unstubbed peer %q", peerID)
	}))

	status, env := aggAsk(t, a, "agg-merge-1", map[string]any{"op": "queue.get"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if env["status"] != "partial" {
		t.Errorf("aggregate status = %v, want partial (a peer answered partial — §5 never reports complete)", env["status"])
	}
	rowsByPeer := aggRowsByPeer(t, env)
	if len(rowsByPeer) != 3 {
		t.Fatalf("peer rows = %d, want 3 (self + 2 peers): %v", len(rowsByPeer), rowsByPeer)
	}
	selfID := database.SchedulerID()
	if row := rowsByPeer[selfID]; row == nil || row["self"] != true {
		t.Errorf("self row missing/mislabeled: %v", rowsByPeer[selfID])
	}
	if row := rowsByPeer["agg-alpha"]; row == nil || row["status"] != "ok" {
		t.Errorf("alpha row status drifted: %v", rowsByPeer["agg-alpha"])
	}
	if row := rowsByPeer["agg-beta"]; row == nil || row["status"] != "partial" {
		t.Errorf("beta per-peer status not preserved: %v", rowsByPeer["agg-beta"])
	}
	// The merged answers: every peer's data present under its own id
	// (per-peer blocks merged, never flattened — spec §7 no invented
	// cross-peer aggregation).
	_, answers := aggRows(t, env)
	if answers == nil {
		t.Fatalf("aggregate answers map missing")
	}
	alphaData, ok := answers["agg-alpha"].(map[string]any)
	if !ok || alphaData["lane"] != "agg-alpha" {
		t.Errorf("alpha data not merged under its peer id: %v", answers["agg-alpha"])
	}
	betaData, ok := answers["agg-beta"].(map[string]any)
	if !ok || betaData["lane"] != "agg-beta" {
		t.Errorf("beta data not merged under its peer id: %v", answers["agg-beta"])
	}
	if _, ok := answers[selfID]; !ok {
		t.Errorf("the primary's own answer is not in the merge (deliverable 2): %v", answers)
	}
	// The gap names the degraded peer — never a silently-shortened answer.
	gapsRaw, _ := env["gaps"].([]any)
	found := false
	for _, g := range gapsRaw {
		gm, _ := g.(map[string]any)
		what, _ := gm["what"].(string)
		if strings.Contains(what, "agg-beta") {
			found = true
		}
	}
	if !found {
		t.Errorf("no gap names agg-beta: %v", env["gaps"])
	}
}

// TestREMOTE012_SlowPeerDoesNotHoldAnswerHostage is acceptance cell 2: a
// slow/hanging peer never holds the answer hostage — the others still
// return inside the aggregate's own ceiling (the collector timer at the
// per-peer budget), the hanging peer renders as a named timeout row.
func TestREMOTE012_SlowPeerDoesNotHoldAnswerHostage(t *testing.T) {
	a := newAPITestServer(t)
	registerAggPeer(t, a, "agg-fast-a", true)
	registerAggPeer(t, a, "agg-hang", true)
	registerAggPeer(t, a, "agg-fast-b", true)
	release := make(chan struct{})
	var once sync.Once
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		if peerID == "agg-hang" {
			// The true hang: the transport does NOT watch ctx — only the
			// aggregate's own ceiling can cut this leg. t.Cleanup releases
			// the goroutine so the test process never leaks it.
			<-release
			env := aggOKEnvelope(q.Op, map[string]any{"lane": peerID})
			env.Peer = peerID
			return env, nil
		}
		env := aggOKEnvelope(q.Op, map[string]any{"lane": peerID})
		env.Peer = peerID
		return env, nil
	}))
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	start := time.Now()
	status, env := aggAskBudget(t, a, "agg-hang-1", map[string]any{"op": "queue.get"}, 400)
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the aggregate must still answer)", status)
	}
	// The ceiling is the 400ms budget; the bound below sits well above it
	// (a loaded host inflates waits uniformly and must not flake the cell)
	// while staying far under the hang (which would only end at cleanup).
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("aggregate took %s — the hanging peer held the answer hostage", elapsed)
	}
	if env["status"] != "partial" {
		t.Errorf("aggregate status = %v, want partial (a peer went silent)", env["status"])
	}
	rowsByPeer := aggRowsByPeer(t, env)
	hang := rowsByPeer["agg-hang"]
	if hang == nil {
		t.Fatalf("the hanging peer is OMITTED — §5 forbids omitted rows: %v", rowsByPeer)
	}
	if hang["status"] != "error" {
		t.Errorf("hang row status = %v, want error", hang["status"])
	}
	if errObj, ok := hang["error"].(map[string]any); !ok || errObj["code"] != "timeout" {
		t.Errorf("hang row error = %v, want code timeout", hang["error"])
	}
	for _, fast := range []string{"agg-fast-a", "agg-fast-b"} {
		row := rowsByPeer[fast]
		if row == nil || row["status"] != "ok" {
			t.Errorf("fast peer %s did not ride along: %v", fast, row)
		}
	}
	_, answers := aggRows(t, env)
	if answers == nil || answers["agg-fast-a"] == nil || answers["agg-fast-b"] == nil {
		t.Errorf("the fast peers' answers are missing from the merge: %v", answers)
	}
}

// aggAskBudget posts an aggregate envelope with an explicit budget_ms.
func aggAskBudget(t *testing.T, a *apiTestServer, corrID string, aggArgs map[string]any, budgetMS int) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"op": "fleet.aggregate", "corr_id": corrID,
		"args": aggArgs, "budget_ms": budgetMS,
	}
	status, raw := fedQuery(t, a, body)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("aggregate response not JSON: %v\nbody: %s", err, raw)
	}
	return status, env
}

// TestREMOTE012_AllPeersSilentStillAnswers is acceptance cell 3: EVERY peer
// unreachable still yields a NON-EMPTY answer — the envelope exists, every
// peer renders (stale=true with its last-contact time, code timeout /
// reply_closed per peer), the gaps name them all, and the word "down"
// appears nowhere (the §5 + remote-spec §2 rendering law).
func TestREMOTE012_AllPeersSilentStillAnswers(t *testing.T) {
	a := newAPITestServer(t)
	// Never-heartbeat peers (last_contact "") — the honest stale arm.
	registerAggPeer(t, a, "agg-silent-a", false)
	registerAggPeer(t, a, "agg-silent-b", false)
	var mu sync.Mutex
	calls := 0
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Peer A: silent past the budget (the §5 timeout degradation);
		// Peer B: the relay hung up (a DIFFERENT named code — preserved).
		if peerID == "agg-silent-a" {
			return bus.ResponseEnvelope{}, fmt.Errorf("bus: query %s corr x: %w", q.Op, bus.ErrQueryTimeout)
		}
		return bus.ResponseEnvelope{}, fmt.Errorf("bus: query %s corr x: %w", q.Op, bus.ErrReplyClosed)
	}))

	status, env := aggAskBudget(t, a, "agg-silent-1", map[string]any{"op": "queue.get"}, 300)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an all-silent fleet STILL answers (§5)", status)
	}
	if env["status"] != "partial" {
		t.Errorf("aggregate status = %v, want partial (never ok over a silent fleet)", env["status"])
	}
	rowsByPeer := aggRowsByPeer(t, env)
	if len(rowsByPeer) != 3 {
		t.Fatalf("peer rows = %d, want 3 — an EMPTY aggregate is a bug: %v", len(rowsByPeer), rowsByPeer)
	}
	rowA := rowsByPeer["agg-silent-a"]
	if rowA == nil || rowA["status"] != "error" || rowA["stale"] != true || rowA["last_contact"] != "" {
		t.Errorf("silent-a row drifted (status/stale/last_contact): %v", rowA)
	}
	if errA, ok := rowA["error"].(map[string]any); !ok || errA["code"] != "timeout" {
		t.Errorf("silent-a error = %v, want code timeout", rowA["error"])
	}
	rowB := rowsByPeer["agg-silent-b"]
	if errB, ok := rowB["error"].(map[string]any); !ok || errB["code"] != "reply_closed" {
		t.Errorf("silent-b error = %v, want code reply_closed (per-peer codes preserved)", rowB["error"])
	}
	// The gaps name every silent peer with the WHY (named gaps, §2.3/§5 —
	// never a silently-shortened answer).
	gapsRaw, _ := env["gaps"].([]any)
	if len(gapsRaw) < 2 {
		t.Errorf("gaps = %v, want at least one per silent peer", env["gaps"])
	}
	for _, peer := range []string{"agg-silent-a", "agg-silent-b"} {
		named := false
		for _, g := range gapsRaw {
			gm, _ := g.(map[string]any)
			what, _ := gm["what"].(string)
			why, _ := gm["why"].(string)
			if strings.Contains(what, peer) && why != "" {
				named = true
			}
		}
		if !named {
			t.Errorf("no gap names %s with a why: %v", peer, env["gaps"])
		}
	}
	// The vocabulary law: "down" appears nowhere in the answer.
	raw, _ := json.Marshal(env)
	if strings.Contains(string(raw), "\"down\"") {
		t.Errorf("the answer claims a peer is down: %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("transport called %d times, want 2 (every registered peer is asked)", calls)
	}
}

// TestREMOTE012_AggregateRefusalsAndRegistryArms pins the addressing laws:
// missing args.op → bad_request; unknown args.op → unknown_op (catalogue
// inline); args.op=fleet.aggregate → unknown_op (no recursive aggregate);
// an EMPTY registry still answers with the self row only; the catalogue
// stays exactly the six §2.3 ops (peers never aggregate — spec §7).
func TestREMOTE012_AggregateRefusalsAndRegistryArms(t *testing.T) {
	a := newAPITestServer(t) // empty registry — no peers registered

	// Missing args.op: a named refusal, never a guessed default.
	status, env := aggAsk(t, a, "agg-ref-1", map[string]any{})
	if status != http.StatusBadRequest {
		t.Errorf("missing args.op: status = %d, want 400", status)
	}
	if e, ok := env["error"].(map[string]any); !ok || e["code"] != "bad_request" {
		t.Errorf("missing args.op error = %v, want bad_request", env["error"])
	}
	// Unknown args.op: named with the catalogue inline.
	status, env = aggAsk(t, a, "agg-ref-2", map[string]any{"op": "not.an.op"})
	if status != http.StatusBadRequest {
		t.Errorf("unknown args.op: status = %d, want 400", status)
	}
	if e, ok := env["error"].(map[string]any); !ok || e["code"] != "unknown_op" {
		t.Errorf("unknown args.op error = %v, want unknown_op", env["error"])
	}
	// No aggregate-of-aggregate: the §7 non-goal (only the primary
	// aggregates; a peer asked about the fleet refuses).
	status, env = aggAsk(t, a, "agg-ref-3", map[string]any{"op": "fleet.aggregate"})
	if status != http.StatusBadRequest {
		t.Errorf("recursive aggregate: status = %d, want 400", status)
	}
	if e, ok := env["error"].(map[string]any); !ok || e["code"] != "unknown_op" {
		t.Errorf("recursive aggregate error = %v, want unknown_op", env["error"])
	}

	// Empty registry: STILL an answer — the primary's own row only, ok,
	// never an empty aggregate.
	status, env = aggAsk(t, a, "agg-empty-1", map[string]any{"op": "queue.get"})
	if status != http.StatusOK {
		t.Fatalf("empty-registry aggregate: status = %d, want 200", status)
	}
	if env["status"] != "ok" {
		t.Errorf("empty-registry status = %v, want ok (self answered)", env["status"])
	}
	rows, answers := aggRows(t, env)
	if len(rows) != 1 {
		t.Errorf("empty-registry rows = %d, want 1 (self)", len(rows))
	}
	if rows[0]["self"] != true || rows[0]["status"] != "ok" {
		t.Errorf("empty-registry self row drifted: %v", rows[0])
	}
	selfID := database.SchedulerID()
	if answers == nil || answers[selfID] == nil {
		t.Errorf("empty-registry answers missing the self block: %v", answers)
	}

	// Catalogue purity: the six §2.3 ops only — fleet.aggregate is the
	// primary's addressing, not a peer answer.
	_, body := a.do(t, "GET", "/api/v1/federation/catalogue", nil)
	opsAny, _ := body["ops"].([]any)
	for _, o := range opsAny {
		entry, _ := o.(map[string]any)
		if entry["op"] == "fleet.aggregate" {
			t.Errorf("fleet.aggregate must NOT sit in the §2.3 catalogue (peers do not aggregate)")
		}
	}
}

// TestREMOTE012_AggregateReplayIsByteIdentical pins §2.5 for the aggregate:
// the same (caller, corr_id, op) returns the FIRST answer byte-for-byte —
// the aggregate rides the same replay window every other op uses.
func TestREMOTE012_AggregateReplayIsByteIdentical(t *testing.T) {
	a := newAPITestServer(t)
	registerAggPeer(t, a, "agg-replay", true)
	var n int
	var mu sync.Mutex
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		mu.Lock()
		n++
		mu.Unlock()
		env := aggOKEnvelope(q.Op, map[string]any{"lane": peerID})
		env.Peer = peerID
		return env, nil
	}))
	body := map[string]any{
		"op": "fleet.aggregate", "corr_id": "agg-replay-1",
		"args": map[string]any{"op": "queue.get"},
	}
	s1, raw1 := fedQuery(t, a, body)
	s2, raw2 := fedQuery(t, a, body)
	if s1 != http.StatusOK || s2 != http.StatusOK {
		t.Fatalf("statuses = %d/%d, want 200/200", s1, s2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Errorf("replayed aggregate differs byte-wise:\n  first:  %s\n  second: %s", raw1, raw2)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Errorf("transport called %d times for a replayed aggregate, want 1 (the replay must not re-fan-out)", n)
	}
}

// TestREMOTE012_AggregateFailClosed pins the §4 gate: the aggregate is a
// federation read on a gated surface — no credential configured is a 503,
// a wrong credential a 401, before anything fans out.
func TestREMOTE012_AggregateFailClosed(t *testing.T) {
	// (a) Bare server: SetAuthConfig never called → authOff → 503.
	bareSrv := newBareFederationServer(t)
	body := `{"op":"fleet.aggregate","corr_id":"agg-auth-1","args":{"op":"queue.get"}}`
	req, err := http.NewRequest(http.MethodPost, bareSrv+"/api/v1/federation/query", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-credential status = %d, want 503 (fail-closed)", resp.StatusCode)
	}
	// (b) Armed server, wrong credential → 401.
	a := newAPITestServer(t)
	req2, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/federation/query", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Operator-Token", "wrong-token-agg")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-credential status = %d, want 401", resp2.StatusCode)
	}
}

// newBareFederationServer builds an HTTP server over a BARE api.Server (no
// auth config — the fail-closed arm), the same shape
// TestREMOTE008_AuthFailClosed's bare arm uses.
func newBareFederationServer(t *testing.T) string {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bare := api.NewServer(db, nil)
	ts := httptest.NewServer(bare.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}
