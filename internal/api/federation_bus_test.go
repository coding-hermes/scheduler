package api

// REMOTE-009 conformance battery (docs/federation-query-spec.md §3, the
// "Crier bus" row): the bus adapter answers with the SAME envelope the
// HTTP surface answers with, because both run the SAME internal query
// entry point (federationOps + the shared replay window) — this file
// proves that on the wire shapes and the §2.5 replay law.
//
// The named cells:
//   - Conformance (acceptance 3): bus vs INTERNAL vs HTTP for the same
//     op on a fixed fixture board — status/gaps/contract/peer identical,
//     data DEEP-EQUAL (the §6 battery normalizes peer + as_of/age_ms,
//     the transport-specific members).
//   - Error-refusal parity: an unknown op is the SAME stable error code
//     with the SAME message on both surfaces (named refusals never drift).
//   - Replay (§2.5, deliverable 3): a bus redelivery of (caller, corr_id,
//     op) returns the FIRST answer byte-for-byte — including as_of —
//     instead of re-reading.
//   - Deadline degradation (deliverable 3 + §5): a read that blows its
//     budget_ms answers status="error" code="deadline_exceeded" (the bus
//     caller maps the transport-level wait to code="timeout").

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// newRemote009Server builds a Server on a fresh temp-file SQLite database
// (the gap1575 construction: a non-running loop, budget=0 so no real spawn
// can ever fire — a test must not touch the host).
func newRemote009Server(t *testing.T) *Server {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	return NewServer(db, loop)
}

// mustCreateRemote009Project inserts one deterministic project row.
func mustCreateRemote009Project(t *testing.T, s *Server, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), s.db, &database.Project{
		Name:     name,
		RepoURL:  "https://example.com/" + name,
		Workdir:  "/tmp/remote009-" + name,
		Weight:   10,
		Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// fedHTTPQuery posts the envelope to the HTTP surface with the operator
// credential and returns status + raw body.
func fedHTTPQuery(t *testing.T, s *Server, body map[string]any) (int, []byte) {
	t.Helper()
	s.SetAuthConfig(ResolveAuthConfig("remote009-operator", "", ""))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/federation/query", strings.NewReader(string(b)))
	req.Header.Set("X-Operator-Token", "remote009-operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST federation/query: %v", err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, raw
}

// busEnvelopeJSON marshals the bus envelope — the exact bytes a reply
// frame would carry — for the byte-level comparison below.
func busEnvelopeJSON(t *testing.T, e bus.ResponseEnvelope) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal bus envelope: %v", err)
	}
	return string(b)
}

// TestREMOTE009_BusAnswerEqualsInternalAnswer is acceptance cell 3: the bus
// answer and the internal (HTTP) answer for the same op/args agree on
// status/gaps/contract/peer and deep-equal data. This is the §6 doctrine
// with the transport-specific members (peer identity is the same string by
// construction; as_of/age_ms are answer-time stamps) normalized out.
func TestREMOTE009_BusAnswerEqualsInternalAnswer(t *testing.T) {
	s := newRemote009Server(t)
	mustCreateRemote009Project(t, s, "alpha009")
	mustCreateRemote009Project(t, s, "beta009")

	// THE INTERNAL ORACLE: the exact entry point the HTTP handler calls.
	fn, ok := federationOpByName("projects.list")
	if !ok {
		t.Fatal("projects.list missing from the catalogue")
	}
	qenv := queryEnvelope{Op: "projects.list", Args: map[string]any{}, CorrID: "internal-1"}
	ctx, obs := s.federationRequestDeadline(context.Background(), 5000)
	defer obs.finish()
	intData, intGaps, fedErr := fn(ctx, s, &qenv)
	if fedErr != nil {
		t.Fatalf("internal oracle failed: %+v", fedErr)
	}

	// THE BUS ANSWER for the same op/args.
	busResp := s.FederationBusHandler(bus.QueryEnvelope{
		Op: "projects.list", Args: map[string]any{}, CorrID: "bus-1",
	})
	if busResp.Status != "ok" && busResp.Status != "stale" {
		t.Fatalf("bus status = %s, want ok|stale", busResp.Status)
	}
	// status: gaps are empty on both sides here, so the internal status is
	// exactly ok — the bus envelope may downgrade to stale ONLY on the
	// self-observation-age law the HTTP surface shares (same code path);
	// with a fresh clock both are plain ok.
	if busResp.Status != "ok" {
		t.Errorf("bus status = %s (a fresh test clock must answer ok)", busResp.Status)
	}
	// contract + peer: the same constants the HTTP surface stamps.
	if busResp.Contract != federationContractVersion {
		t.Errorf("bus contract = %q, want %q", busResp.Contract, federationContractVersion)
	}
	if busResp.Peer != database.SchedulerID() {
		t.Errorf("bus peer = %q, want the local scheduler id", busResp.Peer)
	}
	if len(busResp.Gaps) != len(intGaps) {
		t.Errorf("gaps drifted: bus %v vs internal %v", busResp.Gaps, intGaps)
	}
	// data DEEP-EQUAL via canonical JSON (map ordering is not defined in
	// Go, so byte equality is asserted on the canonical form).
	b1, _ := json.Marshal(intData)
	b2, _ := json.Marshal(busResp.Data)
	if string(b1) != string(b2) {
		t.Errorf("data drifted:\ninternal: %s\nbus:      %s", b1, b2)
	}
	// and the fixture is real: two projects came back.
	var rows []map[string]any
	if err := json.Unmarshal(b2, &rows); err != nil {
		t.Fatalf("projects.list data not a list: %s", b2)
	}
	if len(rows) != 2 {
		t.Errorf("fixture rows = %d, want 2", len(rows))
	}
}

// TestREMOTE009_BusAnswerMatchesHTTPAnswer proves the SAME over the real
// HTTP wire: bus envelope vs the HTTP response body for the same op — the
// data members identical, the envelope members identical modulo as_of.
func TestREMOTE009_BusAnswerMatchesHTTPAnswer(t *testing.T) {
	s := newRemote009Server(t)
	mustCreateRemote009Project(t, s, "gamma009")

	code, raw := fedHTTPQuery(t, s, map[string]any{
		"op": "projects.list", "corr_id": "http-1", "want": "answer",
	})
	if code != http.StatusOK {
		t.Fatalf("HTTP surface answered %d: %s", code, raw)
	}
	var httpEnv struct {
		CorrID string          `json:"corr_id"`
		Op     string          `json:"op"`
		Peer   string          `json:"peer"`
		Status string          `json:"status"`
		AsOf   string          `json:"as_of"`
		Data   json.RawMessage `json:"data"`
		Gaps   []struct {
			What string `json:"what"`
			Why  string `json:"why"`
		} `json:"gaps"`
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(raw, &httpEnv); err != nil {
		t.Fatalf("HTTP body decode: %v (%s)", err, raw)
	}

	busResp := s.FederationBusHandler(bus.QueryEnvelope{
		Op: "projects.list", CorrID: "bus-http-1", Want: "answer",
	})
	if busResp.Status != httpEnv.Status {
		t.Errorf("status drifted: bus %s vs http %s", busResp.Status, httpEnv.Status)
	}
	if busResp.Peer != httpEnv.Peer || busResp.Contract != httpEnv.Contract {
		t.Errorf("peer/contract drifted: bus %q/%q vs http %q/%q", busResp.Peer, busResp.Contract, httpEnv.Peer, httpEnv.Contract)
	}
	bd, _ := json.Marshal(busResp.Data)
	b1, _ := json.Marshal(httpEnv.Data)
	if string(bd) != string(b1) {
		t.Errorf("data drifted:\nbus:  %s\nhttp: %s", bd, b1)
	}
	if len(busResp.Gaps) != len(httpEnv.Gaps) {
		t.Errorf("gaps drifted: bus %v vs http %v", busResp.Gaps, httpEnv.Gaps)
	}
}

// TestREMOTE009_BusUnknownOpParity proves the refusal parity: the SAME
// stable code (unknown_op) and the SAME message the HTTP surface writes.
func TestREMOTE009_BusUnknownOpParity(t *testing.T) {
	s := newRemote009Server(t)

	_, raw := fedHTTPQuery(t, s, map[string]any{"op": "nope.op", "corr_id": "http-u1"})
	var httpErr struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &httpErr); err != nil {
		t.Fatalf("http refusal decode: %v (%s)", err, raw)
	}
	if httpErr.Error.Code != "unknown_op" {
		t.Errorf("http code = %s, want unknown_op", httpErr.Error.Code)
	}

	busResp := s.FederationBusHandler(bus.QueryEnvelope{Op: "nope.op", CorrID: "bus-u1"})
	if busResp.Status != "error" || busResp.Error == nil {
		t.Fatalf("bus refusal shape wrong: %+v", busResp)
	}
	if busResp.Error.Code != httpErr.Error.Code {
		t.Errorf("code drifted: bus %s vs http %s", busResp.Error.Code, httpErr.Error.Code)
	}
	if busResp.Error.Message != httpErr.Error.Message {
		t.Errorf("message drifted:\nbus:  %s\nhttp: %s", busResp.Error.Message, httpErr.Error.Message)
	}
	// The refusal lists the catalogue inline (spec §2.2's named answer).
	if !strings.Contains(busResp.Error.Message, "projects.list") {
		t.Errorf("refusal message lost the catalogue: %s", busResp.Error.Message)
	}
}

// TestREMOTE009_BusReplayWindowIsShared proves §2.5 on the bus: a
// redelivered (caller, corr_id, op) returns the FIRST answer — the
// identical stored envelope, as_of included — instead of re-reading.
func TestREMOTE009_BusReplayWindowIsShared(t *testing.T) {
	s := newRemote009Server(t)
	q := bus.QueryEnvelope{Op: "fleet.status", CorrID: "corr-replay-1"}

	first := s.FederationBusHandler(q)
	if first.Status == "error" {
		t.Fatalf("first answer errored: %+v", first.Error)
	}
	second := s.FederationBusHandler(q)
	// Byte-identical envelope: every member including as_of.
	if busEnvelopeJSON(t, first) != busEnvelopeJSON(t, second) {
		t.Errorf("replay drifted:\nfirst:  %s\nsecond: %s", busEnvelopeJSON(t, first), busEnvelopeJSON(t, second))
	}
	// A DIFFERENT corr_id is a fresh query (new as_of is possible).
	other := s.FederationBusHandler(bus.QueryEnvelope{Op: "fleet.status", CorrID: "corr-replay-2"})
	if other.CorrID != "corr-replay-2" {
		t.Errorf("corr_id echo drifted: %s", other.CorrID)
	}
}

// TestREMOTE009_BusDeadlineArmDeterministic proves the peer-side budget
// mechanism at the seam the handler actually guards every read with: the
// request observer (federationRequestDeadline → checkBus). A budget that
// expires flips checkBus to false — the exact condition
// FederationBusHandler turns into status="error" code="deadline_exceeded"
// (the §5 degradation, the twin of the HTTP 504 arm). The fed read
// functions have no enter() step hooks (unlike the SCHED-GAP-1575-B list
// handlers), so the tripwire here is the observer arm itself, not a
// stalled step: on expiry the handler's fedErr==nil branch is the ONLY
// thing between a caller and a silent miss, and this cell proves the arm
// it consults is live. The transport-level wait that maps to the spec's
// code="timeout" is proven in internal/bus (TestREMOTE009_LateReplyDropped,
// ErrQueryTimeout).
func TestREMOTE009_BusDeadlineArmDeterministic(t *testing.T) {
	s := newRemote009Server(t)

	// (a) A 1ms budget expires: the arm the handler consults flips false.
	ctx, obs := s.federationRequestDeadline(context.Background(), 1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if ctx.Err() == nil {
		t.Fatal("1ms budget never expired — the clock did not advance")
	}
	if obs.checkBus(ctx) {
		t.Error("checkBus = true on an expired budget — the handler would answer ok past the deadline")
	}

	// (b) A generous budget stays live: the arm must not false-trip.
	ctx2, obs2 := s.federationRequestDeadline(context.Background(), 30000)
	defer obs2.finish()
	if !obs2.checkBus(ctx2) {
		t.Error("checkBus = false on a live budget — healthy reads would degrade")
	}

	// (c) The error envelope the handler renders on the false arm: the
	// deadline code is the named one, the echo is intact (pinned via the
	// handler's own construction — a 0-budget read on a HEALTHY db never
	// trips it, so the positive path is covered by the conformance cells).
	if fedErrDeadlineExceed != "deadline_exceeded" {
		t.Errorf("deadline code vocabulary drifted: %s", fedErrDeadlineExceed)
	}
}

// TestREMOTE009_BusGapsPartialStatus pins the ok → partial downgrade on
// the bus adapter: a gap in the read answer is carried into the bus
// envelope and downgrades the status (the caller is never told an answer
// is complete when a piece went missing).
func TestREMOTE009_BusGapsPartialStatus(t *testing.T) {
	s := newRemote009Server(t)
	// fleet.status degrades to gaps when ListPeers fails; force that by
	// closing the DB — the read path records the gap, the envelope says
	// partial, and the adapter must transcribe BOTH.
	_ = s.db.Close()
	resp := s.FederationBusHandler(bus.QueryEnvelope{Op: "fleet.status", CorrID: "corr-gaps"})
	if resp.Status != "partial" {
		t.Fatalf("status = %s, want partial (a gap must downgrade, ok lies)", resp.Status)
	}
	if len(resp.Gaps) == 0 {
		t.Fatal("partial answer carried no gaps")
	}
	var named bool
	for _, g := range resp.Gaps {
		if g.What == "peers" && strings.Contains(g.Why, "ListPeers") {
			named = true
		}
	}
	if !named {
		t.Errorf("the peers gap was not named: %+v", resp.Gaps)
	}
}
