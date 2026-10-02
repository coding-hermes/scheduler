package api_test

// REMOTE-014 (docs/federation-query-spec.md §6 + §5): the AGGREGATE leg of
// the surface conformance battery. The aggregate (op=fleet.aggregate) is
// the primary's own §2.2 envelope, so it must satisfy the same contract
// every other surface answers with — and its merged answer must equal the
// per-surface answers combined by the §5 rule:
//
//   - ok blocks merge (per-peer data preserved under its peer id, never
//     flattened into an invented cross-peer aggregation);
//   - any partial/error/timeout becomes a NAMED gap for that peer and the
//     overall status drops to partial (never "complete" over a silent
//     peer);
//   - per-peer degradation is explicit: every peer renders a row with its
//     own status — a timed-out peer is status="error" code="timeout" with
//     the registry's stale=true + last_contact rendering law, never an
//     omitted row and never a "down" state.
//
// The single-peer answer (the shared internal entry point) is the oracle:
// the aggregate's per-peer answer blocks must equal it byte-for-byte.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// fconfEnvFields is the §2.2 member set every envelope on every surface —
// the aggregate included — must carry (non-null members, sorted).
var fconfEnvFields = []string{
	"age_ms", "as_of", "contract", "corr_id", "data", "gaps", "op", "peer", "status",
}

// fconfDataOf marshals any envelope data member to its canonical JSON.
func fconfDataOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(b)
}

// fconfAggEnvelope is a §2.2 ok/partial envelope builder whose data is
// unconstrained (the queue.get answer is an ARRAY — decoding the oracle's
// data into a map, as the merge-equality arm first did, would fail; the
// merge must be byte-equal to the oracle's data verbatim, whatever its
// JSON type).
func fconfAggEnvelope(op string, data any) bus.ResponseEnvelope {
	env := aggOKEnvelope(op, nil)
	env.Data = data
	return env
}

// TestREMOTE014_AggregateConformance proves the aggregate's §2.2 envelope
// and the §5 merge against the shared-entry oracle: an all-ok fleet merges
// to status=ok with every per-peer answer equal to the single-peer answer;
// one silent peer drops the merge to partial with the peer named by a
// gap and rendered as an explicit error/timeout row (never omitted).
func TestREMOTE014_AggregateConformance(t *testing.T) {
	a := newAPITestServer(t)
	registerAggPeer(t, a, "fconf-agg-peer", true)

	// THE ORACLE: the single-peer answer the shared entry point produces
	// for the same op on the same fixture board.
	oracle := a.server.FederationQueryHandler(
		bus.QueryEnvelope{Op: "queue.get", CorrID: "fconf-agg-oracle"}, "fconf-shared-entry")
	if oracle.Status != "ok" {
		t.Fatalf("oracle status = %s (%+v)", oracle.Status, oracle.Error)
	}
	oracleData := fconfDataOf(t, oracle.Data)
	var oracleDataAny any
	if err := json.Unmarshal([]byte(oracleData), &oracleDataAny); err != nil {
		t.Fatalf("decode oracle data: %v (%s)", err, oracleData)
	}

	// The stubbed peer answers queue.get with the SAME read (the peer's
	// §3 duty: the same internal entry point, here approximated by the
	// same fixture answer).
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		if peerID == "fconf-agg-peer" {
			env := fconfAggEnvelope(q.Op, oracleDataAny)
			env.Peer = peerID
			env.CorrID = q.CorrID
			return env, nil
		}
		return bus.ResponseEnvelope{}, fmt.Errorf("unstubbed peer %q", peerID)
	}))

	// ── arm 1: the all-ok merge is status=ok, byte-equal to the oracle ──
	status, env := aggAsk(t, a, "fconf-agg-1", map[string]any{"op": "queue.get"})
	if status != http.StatusOK {
		t.Fatalf("aggregate status = %d, want 200", status)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshal aggregate envelope: %v", err)
	}
	aggRes := fconfNormalize(t, "aggregate", raw)
	if got := fmt.Sprint(aggRes.fields); got != fmt.Sprint(fconfEnvFields) {
		t.Errorf("aggregate §2.2 field set = [%s], want [%s] (the envelope must satisfy the same contract as every surface)",
			aggRes.fields, fconfEnvFields)
	}
	if env["status"] != "ok" {
		t.Errorf("all-ok aggregate status = %v, want ok (§5: ok blocks merge, nothing degraded)", env["status"])
	}
	if env["error"] != nil {
		t.Errorf("aggregate error member = %v, want absent on an ok envelope", env["error"])
	}
	data, _ := env["data"].(map[string]any)
	if data["op"] != "queue.get" {
		t.Errorf("data.op = %v, want queue.get (the fanned-out read op)", data["op"])
	}
	if n, _ := data["peer_count"].(float64); n != 2 {
		t.Errorf("peer_count = %v, want 2 (self + one peer)", data["peer_count"])
	}
	if n, _ := data["answered"].(float64); n != 2 {
		t.Errorf("answered = %v, want 2", data["answered"])
	}
	if n, _ := data["degraded"].(float64); n != 0 {
		t.Errorf("degraded = %v, want 0", data["degraded"])
	}
	if gaps, ok := env["gaps"].([]any); !ok || len(gaps) != 0 {
		t.Errorf("gaps = %v, want an explicit empty array on the all-ok merge", env["gaps"])
	}
	_, answers := aggRows(t, env)
	if got := fconfDataOf(t, answers[database.SchedulerID()]); got != oracleData {
		t.Errorf("self answer block differs from the oracle:\n  got:    %s\n  oracle: %s", got, oracleData)
	}
	if got := fconfDataOf(t, answers["fconf-agg-peer"]); got != oracleData {
		t.Errorf("peer answer block differs from the oracle (§5: ok blocks merge verbatim):\n  got:    %s\n  oracle: %s", got, oracleData)
	}

	// ── arm 2: one silent peer → partial + NAMED gap + explicit row ────
	registerAggPeer(t, a, "fconf-agg-silent", false) // never heartbeat: stale=true, last_contact ""
	a.server.SetAggregateTransport(aggStubTransport(func(peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
		if peerID == "fconf-agg-silent" {
			return bus.ResponseEnvelope{}, fmt.Errorf("bus: query %s corr x: %w", q.Op, bus.ErrQueryTimeout)
		}
		env := fconfAggEnvelope(q.Op, oracleDataAny)
		env.Peer = peerID
		env.CorrID = q.CorrID
		return env, nil
	}))

	status2, env2 := aggAskBudget(t, a, "fconf-agg-2", map[string]any{"op": "queue.get"}, 500)
	if status2 != http.StatusOK {
		t.Fatalf("degraded aggregate status = %d, want 200 (an all-silent fleet still answers)", status2)
	}
	if env2["status"] != "partial" {
		t.Errorf("aggregate with a silent peer = %v, want partial (§5: the caller is never told the answer is complete)", env2["status"])
	}
	rowsByPeer := aggRowsByPeer(t, env2)
	silent := rowsByPeer["fconf-agg-silent"]
	if silent == nil {
		t.Fatalf("silent peer OMITTED — §5 forbids omitted rows: %v", rowsByPeer)
	}
	if silent["status"] != "error" || silent["stale"] != true || silent["last_contact"] != "" {
		t.Errorf("silent row = %v, want status=error stale=true last_contact=\"\" (the registry rendering law)", silent)
	}
	if e, ok := silent["error"].(map[string]any); !ok || e["code"] != "timeout" {
		t.Errorf("silent row error = %v, want code timeout (the §5 degradation vocabulary)", silent["error"])
	}
	gaps2, _ := env2["gaps"].([]any)
	namedGap := false
	for _, g := range gaps2 {
		gm, _ := g.(map[string]any)
		what, _ := gm["what"].(string)
		why, _ := gm["why"].(string)
		if what != "" && why != "" {
			namedGap = true
		}
	}
	if len(gaps2) == 0 || !namedGap {
		t.Errorf("gaps = %v, want at least one NAMED gap for the silent peer", env2["gaps"])
	}
	_, answers2 := aggRows(t, env2)
	if _, still := answers2["fconf-agg-silent"]; still {
		t.Errorf("a silent peer's answer block must not exist (timeout is not data): %v", answers2)
	}
	if got := fconfDataOf(t, answers2["fconf-agg-peer"]); got != oracleData {
		t.Errorf("ok peer answer degraded by its silent sibling:\n  got:    %s\n  oracle: %s", got, oracleData)
	}
	d2, _ := env2["data"].(map[string]any)
	if n, _ := d2["degraded"].(float64); n != 1 {
		t.Errorf("degraded = %v, want 1", d2["degraded"])
	}
	if n, _ := d2["answered"].(float64); n != 2 {
		t.Errorf("answered = %v, want 2 (self + the ok peer)", d2["answered"])
	}

	// The peers-refuse arm: the aggregate is the PRIMARY's op — a peer
	// asked about the fleet answers unknown_op (spec §7: only the primary
	// aggregates), the same code every catalogue surface answers with.
	refused := a.server.FederationQueryHandler(
		bus.QueryEnvelope{Op: "fleet.aggregate", CorrID: "fconf-agg-peer-ask"}, "fconf-shared-entry")
	if refused.Status != "error" || refused.Error == nil || refused.Error.Code != "unknown_op" {
		t.Errorf("peer asked fleet.aggregate = %+v, want error/unknown_op (§7 catalogue purity)", refused)
	}
}
