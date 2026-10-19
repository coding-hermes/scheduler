package scheduler

// SCHED-GAP-1665 — TARGETED tick dispatch.
//
// Two properties are proven here, both about the dispatch leg rather than the
// HTTP surface:
//
//  1. a per-tick agent override ROUTES the tick: with no dispatch-target row
//     for the lane at all, the hand-out still reaches the named agent, the
//     shared gateway is never touched, and the receipt records that agent;
//  2. a REGISTERED agent's declared endpoint is honoured for a targeted
//     dispatch, while every other dispatch keeps riding the configured relay
//     byte-identically (the additive half of the change).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// TestResolveSpawnTargetOverride is the resolution table: the lane's own target
// still supplies the references and host identity, the override replaces only
// the AGENT, and a missing/unreadable file cannot cancel an explicit override.
func TestResolveSpawnTargetOverride(t *testing.T) {
	laneFile := writeDispatchTargets(t, `{"lane":"helix","agent":"lane-agent","workdir":"/agent/helix","host_id":"bunker-a"}`+"\n")

	cases := []struct {
		name        string
		targets     func(*testing.T) string
		override    string
		wantRemote  bool
		wantAgent   string
		wantWorkdir string
		wantErr     bool
	}{
		{
			name:    "no row, no override — historical local spawn",
			targets: func(t *testing.T) string { return writeDispatchTargets(t, "") }, wantRemote: false,
		},
		{
			name:    "no row, override — the override alone makes it remote",
			targets: func(t *testing.T) string { return writeDispatchTargets(t, "") }, override: "tick-agent",
			wantRemote: true, wantAgent: "tick-agent",
		},
		{
			name:    "lane row, no override — the lane's own agent",
			targets: func(*testing.T) string { return laneFile }, wantRemote: true, wantAgent: "lane-agent", wantWorkdir: "/agent/helix",
		},
		{
			name:    "lane row, override — agent replaced, references kept",
			targets: func(*testing.T) string { return laneFile }, override: "tick-agent",
			wantRemote: true, wantAgent: "tick-agent", wantWorkdir: "/agent/helix",
		},
		{
			// A FRESH broken file per case: the loader memoizes by path, so a
			// reused path would be a warm cache hit (and the parse error is a
			// cold-read signal, not a cached one).
			name:    "unreadable file, override — override survives, error reported",
			targets: func(t *testing.T) string { return writeDispatchTargets(t, `{"lane":"helix","agent":`+"\n") }, override: "tick-agent",
			wantRemote: true, wantAgent: "tick-agent", wantErr: true,
		},
		{
			name:       "unreadable file, no override — historical behaviour (local + error)",
			targets:    func(t *testing.T) string { return writeDispatchTargets(t, `{"lane":"helix","agent":`+"\n") },
			wantRemote: false, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDispatchTargets, tc.targets(t))
			proj := PackedProject{Name: "helix", Workdir: "/tmp/helix", DispatchAgent: tc.override}
			target, remote, err := resolveSpawnTarget(proj)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if remote != tc.wantRemote {
				t.Errorf("remote = %v, want %v", remote, tc.wantRemote)
			}
			if remote && target.Agent != tc.wantAgent {
				t.Errorf("agent = %q, want %q", target.Agent, tc.wantAgent)
			}
			if tc.wantWorkdir != "" && target.Workdir != tc.wantWorkdir {
				t.Errorf("workdir = %q, want the lane's own reference %q", target.Workdir, tc.wantWorkdir)
			}
		})
	}
}

// TestDispatchLeg_TargetedOverrideHandsWorkToNamedAgent is the acceptance in one
// test: a lane with NO dispatch-target row (so the historical routing would run
// locally) is spawned with an explicit agent override, and the work reaches that
// agent — receipt, correlation and answer included — while the shared gateway
// records ZERO requests.
func TestDispatchLeg_TargetedOverrideHandsWorkToNamedAgent(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "tick-agent", "TARGETED ANSWER: done")
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, true)
	// Replace the rig's lane row with an EMPTY file: the override must be the
	// only reason this tick is remote.
	t.Setenv(EnvDispatchTargets, writeDispatchTargets(t, ""))

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix", DispatchAgent: "tick-agent"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "completed" {
		t.Fatalf("tick status = %q (error %q), want completed via the named agent's answer", row.Status, row.Err)
	}
	got, _, acks := relay.counts()
	if got != 1 {
		t.Fatalf("hand-outs = %d, want exactly 1", got)
	}
	if agent := relay.dispatches[0].agent; agent != "tick-agent" {
		t.Errorf("hand-out agent = %q, want tick-agent (the override)", agent)
	}
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, ok, err)
	}
	if rec.Agent != "tick-agent" {
		t.Errorf("receipt agent = %q, want tick-agent (the agent USED)", rec.Agent)
	}
	if rec.State != database.DispatchStateReplied {
		t.Errorf("receipt state = %q, want replied", rec.State)
	}
	if !strings.Contains(rec.Reply, "TARGETED ANSWER") {
		t.Errorf("receipt reply = %q, want the agent's answer", rec.Reply)
	}
	if len(acks) == 0 {
		t.Error("the correlated answer was never acked")
	}
	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests — a targeted dispatch must never fall back to the shared gateway", n)
	}
	// The hand-out carries the lane + a reference (the lane workdir fallback)
	// and never a task id — the payload law holds for the targeted path too.
	d := relay.dispatches[0]
	if d.payload["lane"] != "helix" {
		t.Errorf("payload lane = %v, want helix", d.payload["lane"])
	}
	if d.payload["workdir"] != "/tmp/helix" {
		t.Errorf("payload workdir = %v, want the lane workdir fallback", d.payload["workdir"])
	}
	if _, bad := d.payload["task"]; bad {
		t.Error("payload carries a TASK member — the scheduler picks the lane, the foreman picks the task")
	}
}

// recordingRelay is a minimal relay stand-in: it accepts the dispatch endpoint
// and counts the POSTs, so a test can tell WHICH relay a request went to.
func recordingRelay(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/inbox") {
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"m-1","transport":"inbox"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestDispatchToTargetUsesRegisteredEndpointForTargetedDispatch proves the
// registry's `endpoint` is LOAD-BEARING: a targeted dispatch to a registered
// agent with an endpoint hands the work to THAT relay. The control arm proves
// the same call without an override keeps the configured relay.
func TestDispatchToTargetUsesRegisteredEndpointForTargetedDispatch(t *testing.T) {
	configured, configuredHits := recordingRelay(t)
	registered, registeredHits := recordingRelay(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	client := bus.NewClient(true, configured.URL, "", "test-sched")
	client.SetAgentIdentity("test-sched-inbox", priv)

	db := newTestDB(t)
	ctx := context.Background()
	if _, err := database.UpsertAgent(ctx, db, &database.Agent{
		Name: "helix-agent", Capabilities: "dispatch", Endpoint: registered.URL,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	item := bus.WorkItem{Lane: "helix", Workdir: "/tmp/helix", ReplyTo: "test-sched-inbox"}
	target := DispatchTarget{Lane: "helix", Agent: "helix-agent"}

	// Targeted arm: the registry endpoint wins.
	proj := PackedProject{Name: "helix", Workdir: "/tmp/helix", DispatchAgent: "helix-agent"}
	if _, err := dispatchToTarget(ctx, client, proj, target, item, db); err != nil {
		t.Fatalf("targeted dispatchToTarget: %v", err)
	}
	if n := atomic.LoadInt64(registeredHits); n != 1 {
		t.Errorf("registered endpoint saw %d hand-outs, want 1", n)
	}
	if n := atomic.LoadInt64(configuredHits); n != 0 {
		t.Errorf("configured relay saw %d hand-outs on the targeted arm, want 0", n)
	}

	// Control arm: no override → the configured relay, byte-identical to the
	// pre-registry behaviour (even though the agent IS registered).
	plain := PackedProject{Name: "helix", Workdir: "/tmp/helix"}
	if _, err := dispatchToTarget(ctx, client, plain, target, item, db); err != nil {
		t.Fatalf("plain dispatchToTarget: %v", err)
	}
	if n := atomic.LoadInt64(configuredHits); n != 1 {
		t.Errorf("configured relay saw %d hand-outs on the control arm, want 1", n)
	}
	if n := atomic.LoadInt64(registeredHits); n != 1 {
		t.Errorf("registered endpoint saw %d hand-outs after the control arm, want still 1", n)
	}
}
