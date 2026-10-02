package api

// REMOTE-008 in-package cells: the two battery arms that need to DRIVE the
// server's clock (SCHED-GAP-169 discipline — the cells advance the manual
// sim clock the server was built with instead of wall-sleeping). The rest
// of the battery lives in server_federation_test.go (package api_test,
// wire-level, like the other handler tests).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// fedToken is the credential the in-package stack arms (same value as the
// external battery's shared token — the two packages compile separately, so
// each carries its own constant).
const fedToken = "api-test-operator-token"

// newFedSimStack wires the full stack on a MANUAL sim clock: in-memory DB,
// a Loop, the server's clock seam pointed at the sim, operator token armed,
// blocks store installed. Returns the pieces the cells drive.
func newFedSimStack(t *testing.T) (loop *scheduler.Loop, srv *Server, sim *clock.SimClock, ts *httptest.Server) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	sim = clock.NewManualSimClock(time.Now())
	loop = scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	// The loop gets the sim clock BEFORE NewServer: NewServer seeds the
	// server's seam from loop.Clock() (one atomic store — a second SetClock
	// with a different concrete type would panic the Seam), and the loop's
	// lastEval then lands on the SAME timeline the server answers from.
	loop.SetClock(sim)
	srv = NewServer(db, loop)
	srv.SetAuthConfig(ResolveAuthConfig(fedToken, "", ""))
	// REMOTE-013: the freshness cells prove the stale law over real
	// answers — arm the allow-all read policy (the REMOTE-013 battery
	// owns the policy behavior).
	srv.SetFederationReadPolicy(ResolveFederationReadPolicy([]FederationReadGrant{fedGrantAllOps()}))
	dir := t.TempDir()
	srv.SetBlocksStore(blocks.NewStore(
		filepath.Join(dir, "groups.jsonl"),
		filepath.Join(dir, "templates.jsonl"),
	))
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return loop, srv, sim, ts
}

// fedPost submits one query envelope with the armed credential and returns
// status + raw body (byte-level comparison surface).
func fedPost(t *testing.T, ts *httptest.Server, body map[string]any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/federation/query", bytes.NewReader(b))
	req.Header.Set("X-Operator-Token", fedToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestREMOTE008_ReplayExpiresAfterWindow proves the replay window is SHORT
// (§2.5 default 5 min): past it, the same (caller, corr_id, op) is a fresh
// query — a new as_of, a re-read. Driven by advancing the manual sim clock
// 6 minutes; no wall sleeping.
func TestREMOTE008_ReplayExpiresAfterWindow(t *testing.T) {
	_, _, sim, ts := newFedSimStack(t)
	_, raw1 := fedPost(t, ts, map[string]any{"op": "peer.status", "corr_id": "c-exp"})
	sim.Advance(6 * time.Minute)
	_, raw2 := fedPost(t, ts, map[string]any{"op": "peer.status", "corr_id": "c-exp"})
	if bytes.Equal(raw1, raw2) {
		t.Fatal("identity replayed AFTER the 5-minute window expired — the window must be short")
	}
	var first, second struct {
		AsOf string `json:"as_of"`
	}
	if err := json.Unmarshal(raw1, &first); err != nil {
		t.Fatalf("unmarshal first: %v; body: %s", err, raw1)
	}
	if err := json.Unmarshal(raw2, &second); err != nil {
		t.Fatalf("unmarshal second: %v; body: %s", err, raw2)
	}
	if first.AsOf == "" || first.AsOf == second.AsOf {
		t.Fatalf("expired replay must re-read with a new as_of: first=%q second=%q", first.AsOf, second.AsOf)
	}
}

// TestREMOTE008_ReplayWithinWindowIdentical is the in-package twin of the
// wire-level replay cell, pinned on the SAME clock the server answers with:
// within the window the body is byte-identical — including as_of.
func TestREMOTE008_ReplayWithinWindowIdentical(t *testing.T) {
	_, _, sim, ts := newFedSimStack(t)
	_, raw1 := fedPost(t, ts, map[string]any{"op": "peer.status", "corr_id": "c-win"})
	sim.Advance(4 * time.Minute) // still inside the 5-minute window
	_, raw2 := fedPost(t, ts, map[string]any{"op": "peer.status", "corr_id": "c-win"})
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("replay inside the window must be byte-identical:\nfirst:  %s\nsecond: %s", raw1, raw2)
	}
}

// TestREMOTE008_StaleIsFirstClass proves the §2.2 stale invariant: a peer
// whose own last self-observation (the loop's lastEval) is older than its
// freshness window answers status="stale" — WITH data, as_of intact, and
// never conflated with error. The freshness arithmetic reads the same
// manual clock the cells advance.
func TestREMOTE008_StaleIsFirstClass(t *testing.T) {
	loop, _, sim, ts := newFedSimStack(t)
	// Make the loop observe itself once (lastEval = the loop's clock now).
	loop.ForceEvaluate()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !loop.LastEvalTime().IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loop.LastEvalTime().IsZero() {
		t.Fatal("loop never evaluated — test precondition failed")
	}
	post := func(corr string) (int, []byte) {
		return fedPost(t, ts, map[string]any{"op": "fleet.status", "corr_id": corr})
	}
	// Unique corr_ids: a shared one would hit the §2.5 replay window on the
	// second probe and serve the FIRST answer — the replay law is exactly
	// what the fresh-vs-stale pair must not trip over.
	s, rawFresh := post("c-stale-fresh")
	if s != http.StatusOK {
		t.Fatalf("status = %d; body: %s", s, rawFresh)
	}
	var fresh struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rawFresh, &fresh); err != nil {
		t.Fatalf("unmarshal: %v; body: %s", err, rawFresh)
	}
	if fresh.Status != "ok" {
		t.Fatalf("status = %q, want ok while fresh; body: %s", fresh.Status, rawFresh)
	}
	if string(bytes.TrimSpace(fresh.Data)) == "null" || len(bytes.TrimSpace(fresh.Data)) == 0 {
		t.Fatalf("fresh answer carries no data: %s", rawFresh)
	}
	// Age the server's clock past the freshness window (3m default +4x):
	// the same data answers, but the status drops to stale.
	sim.Advance(12 * time.Minute)
	s, rawStale := post("c-stale-aged")
	if s != http.StatusOK {
		t.Fatalf("stale query status = %d; body: %s", s, rawStale)
	}
	var stale struct {
		Status string          `json:"status"`
		Error  *map[string]any `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rawStale, &stale); err != nil {
		t.Fatalf("unmarshal: %v; body: %s", err, rawStale)
	}
	if stale.Status != "stale" {
		t.Fatalf("status = %q, want stale after the freshness window; body: %s", stale.Status, rawStale)
	}
	if stale.Error != nil {
		t.Errorf("stale must never carry an error object (distinct from error): %s", rawStale)
	}
	var dataObj map[string]any
	if err := json.Unmarshal(stale.Data, &dataObj); err != nil {
		t.Errorf("stale is still an ANSWER — a data object must ride along: %s", rawStale)
	} else if len(dataObj) == 0 {
		t.Errorf("stale fleet.status data empty: %s", rawStale)
	}
	if !strings.Contains(string(rawStale[:200]), `"as_of"`) {
		t.Errorf("stale answer lost as_of: %s", rawStale)
	}
}
