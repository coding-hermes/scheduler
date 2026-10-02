package main

// REMOTE-012 CLI delegation cells (deliverable 5): --all is the thin
// adapter over the DAEMON's authoritative aggregate — the merge is never
// duplicated in this binary (spec §3). The stub daemon records the request
// body so the cells prove the DELEGATION SHAPE (op=fleet.aggregate,
// args.op = the chosen read op, the §2.1 members carried) and the exit
// ladder (0 ok / 1 degraded / 2 unreachable or refused).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// aggStubDaemon is an httptest server standing in for the daemon's
// federation query surface. It records the last request body and answers
// with the envelope the test installs.
type aggStubDaemon struct {
	srv      *httptest.Server
	lastBody map[string]any
	lastAuth string
}

func newAggStubDaemon(t *testing.T, answer map[string]any, status int) *aggStubDaemon {
	t.Helper()
	d := &aggStubDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/federation/query", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("stub daemon: undecodable body: %v", err)
		}
		d.lastBody = body
		d.lastAuth = r.Header.Get("X-Operator-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// aggDaemonEnvelope is a minimal merged aggregate answer.
func aggDaemonEnvelope(status string) map[string]any {
	return map[string]any{
		"corr_id":  "cli-agg-1",
		"op":       "fleet.aggregate",
		"peer":     "self",
		"status":   status,
		"as_of":    "2026-10-02T00:00:00Z",
		"age_ms":   0,
		"data":     map[string]any{"op": "queue.get", "peer_count": 2, "answered": 1, "degraded": 1, "peers": []any{}, "answers": map[string]any{}},
		"gaps":     []any{map[string]any{"what": "peer agg-beta", "why": "timeout: no reply inside the budget"}},
		"contract": "1.0.0",
	}
}

// TestREMOTE012_CLIAllDelegates proves the --all delegation shape: the CLI
// POSTs ONE envelope to the daemon (op=fleet.aggregate, args.op = the
// positional read op, corr_id echoed, args passthrough) and renders the
// §2.2 envelope verbatim. Exit 0 on an ok aggregate.
func TestREMOTE012_CLIAllDelegates(t *testing.T) {
	d := newAggStubDaemon(t, aggDaemonEnvelope("ok"), http.StatusOK)
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		allPeers:  true,
		op:        "queue.get",
		args:      argMap{"limit": "5"},
		corrID:    "cli-agg-1",
		budgetMS:  1500,
		jsonOut:   true,
		daemonURL: d.srv.URL,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	code := runQuery(context.Background(), cfg, &fakeTransport{})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if d.lastBody == nil {
		t.Fatalf("the daemon never saw a request")
	}
	if d.lastBody["op"] != "fleet.aggregate" {
		t.Errorf("delegated op = %v, want fleet.aggregate", d.lastBody["op"])
	}
	args, _ := d.lastBody["args"].(map[string]any)
	if args == nil || args["op"] != "queue.get" {
		t.Errorf("delegated args.op = %v, want queue.get (the positional read op)", d.lastBody["args"])
	}
	if args["limit"] != "5" {
		t.Errorf("--arg passthrough lost: %v", args)
	}
	if d.lastBody["corr_id"] != "cli-agg-1" {
		t.Errorf("delegated corr_id = %v, want cli-agg-1", d.lastBody["corr_id"])
	}
	if d.lastBody["budget_ms"] != float64(1500) {
		t.Errorf("delegated budget_ms = %v, want 1500", d.lastBody["budget_ms"])
	}
	// The rendered answer is the §2.2 envelope verbatim.
	var env busShim
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("stdout not one JSON envelope: %v (%s)", err, stdout.String())
	}
	if env.Status != "ok" || env.CorrID != "cli-agg-1" || env.Op != "fleet.aggregate" {
		t.Errorf("rendered envelope drifted: %s", stdout.String())
	}
}

// busShim is the local decode shim for the rendered envelope (the fields
// the CLI cells assert on).
type busShim struct {
	Status string `json:"status"`
	CorrID string `json:"corr_id"`
	Op     string `json:"op"`
}

// TestREMOTE012_CLIAllDegradedExitOne pins the degraded aggregate: the
// daemon answered (partial — a peer went silent), the CLI renders it and
// exits 1, never 2.
func TestREMOTE012_CLIAllDegradedExitOne(t *testing.T) {
	d := newAggStubDaemon(t, aggDaemonEnvelope("partial"), http.StatusOK)
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		allPeers:  true,
		op:        "queue.get",
		corrID:    "cli-agg-2",
		jsonOut:   true,
		daemonURL: d.srv.URL,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	if code := runQuery(context.Background(), cfg, &fakeTransport{}); code != exitDegraded {
		t.Fatalf("exit = %d, want %d (a partial aggregate is degraded, not a hard error)", code, exitDegraded)
	}
	var env busShim
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("stdout not one JSON envelope: %v", err)
	}
	if env.Status != "partial" {
		t.Errorf("rendered status = %q, want partial", env.Status)
	}
}

// TestREMOTE012_CLIAllUnreachableExitTwo pins the hard-error arm: no
// daemon at the URL → exit 2 with a NAMED stderr line, never a fabricated
// answer on stdout.
func TestREMOTE012_CLIAllUnreachableExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		allPeers:  true,
		op:        "queue.get",
		corrID:    "cli-agg-3",
		jsonOut:   true,
		daemonURL: "http://127.0.0.1:1",
		stdout:    &stdout,
		stderr:    &stderr,
	}
	if code := runQuery(context.Background(), cfg, &fakeTransport{}); code != exitHard {
		t.Fatalf("exit = %d, want %d (the aggregate could not be attempted)", code, exitHard)
	}
	if !strings.Contains(stderr.String(), "unreachable") {
		t.Errorf("stderr must name the failure: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "\"status\"") {
		t.Errorf("a hard error printed an answer envelope: %q", stdout.String())
	}
}

// TestREMOTE012_CLIAllRefusedExitTwo pins the credential arm: a 503 from
// the daemon's fail-closed gate is a hard error with the credential hint.
func TestREMOTE012_CLIAllRefusedExitTwo(t *testing.T) {
	d := newAggStubDaemon(t, map[string]any{"error": "no credential configured"}, http.StatusServiceUnavailable)
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		allPeers:  true,
		op:        "queue.get",
		corrID:    "cli-agg-4",
		jsonOut:   true,
		daemonURL: d.srv.URL,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	if code := runQuery(context.Background(), cfg, &fakeTransport{}); code != exitHard {
		t.Fatalf("exit = %d, want %d (refused)", code, exitHard)
	}
	if !strings.Contains(stderr.String(), "SCHEDULER_OPERATOR_TOKEN") {
		t.Errorf("stderr must carry the credential hint: %q", stderr.String())
	}
}
