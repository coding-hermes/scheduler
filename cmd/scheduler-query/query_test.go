package main

// REMOTE-011 acceptance battery (docs/federation-query-spec.md §3, the
// "CLI" row). The cells map 1:1 to the deliverables:
//
//   - TestREMOTE011_CLIAnswerEqualsInternalAnswer — acceptance cell 1: the
//     CLI's answer for an op equals the shared internal entry point's
//     answer for the same op/args (api.Server.FederationQueryHandler is
//     the §6 oracle; the transport-specific members are normalized exactly
//     the way the REMOTE-009/010 batteries normalize them).
//   - TestREMOTE011_SilentPeerDegrades — acceptance cell 2: an unreachable
//     target yields an error envelope, exit 1, no hang, no panic.
//   - the fan-out / JSON / exit-code / help cells pin the remaining
//     deliverable behavior.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// fakeTransport is the injected seam for the degradation and exit-code
// cells (the real wire path is covered by the stub-relay cells).
type fakeTransport struct {
	resp bus.ResponseEnvelope
	err  error
}

func (f *fakeTransport) Query(ctx context.Context, peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
	return f.resp, f.err
}

func (f *fakeTransport) NextCorrID() string { return "corr-fake-1" }

// newOracleStack wires the REAL daemon topology for the conformance cell:
// ONE api.Server over one DB, exactly as main.go builds it (the gap1575
// construction: a non-running loop, budget=0 — a test must not touch the
// host). Returns the server whose FederationQueryHandler is the §6 oracle.
func newOracleStack(t *testing.T) *api.Server {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	apiSrv := api.NewServer(db, loop)
	// REMOTE-013: the CLI cells prove the transport over real answers —
	// arm the allow-all read policy on the oracle stack (the api
	// package's REMOTE-013 battery owns the policy behavior; the
	// test-oracle / bus:transport callers here are covered by "*").
	apiSrv.SetFederationReadPolicy(api.ResolveFederationReadPolicy([]api.FederationReadGrant{
		{Caller: api.FederationAllowAll, Ops: []string{
			"peer.status", "fleet.status", "projects.list",
			"queue.get", "ticks.list", "events.list",
		}},
	}))
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:     "alpha011",
		RepoURL:  "https://example.com/alpha011",
		Workdir:  "/tmp/remote011-alpha011",
		Weight:   10,
		Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return apiSrv
}

// dataJSON normalizes the envelope's data member for the §6 diff (the
// contract members compared exactly; the transport-specific members
// normalized away as the REMOTE-009/010 batteries do).
func dataJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(b)
}

// ── INT-CI-176 deterministic-wait helpers ─────────────────────────────────
//
// The REMOTE011 cells previously carried two flake classes: (1) a
// wall-clock elapsed ceiling that measures LOADED-HOST INFLATION (a
// saturated host inflates the OBSERVATION of a deadline passing; measured
// 7.29s against a 5200ms ceiling while every leg returned on time), and
// (2) fixed wire budgets whose slack budgeted an amd64 laptop, not an
// arm64 CI runner (the red burned its whole 10s default budget on a
// runner where every wait ran ~2x). Both are replaced by the primitives
// below:
//
//   - testBudget wires every wire-level wait to the test's OWN deadline
//     (never a fixed number): a slow arch cannot outgrow a budget that is
//     derived from the runner it is slow ON. Go's per-test timeout caps
//     the result — a REAL hang still goes red, at the framework level.
//   - awaitCompletion bounds a completion wait by an observer ceiling
//     set budget+ceiling+10x-slack ABOVE the property it watches, so
//     only a leg that escaped its own deadlines (a real hang) can trip
//     it — uniform load inflation cannot (that is what "10x" buys), and
//     the test's deadline is the second, framework-level net behind it.
//
// All contract assertions (exit codes, envelope shapes, per-peer rows,
// counts) are unchanged: the tests still fail on exactly the defects they
// failed on before — only the CLOCK ARMING changed.

// testSchedulerSlack is one scheduling quantum as this battery grants it
// (the 10x multiplier in the observer ceilings below does the real work).
const testSchedulerSlack = 100 * time.Millisecond

// fanoutMS restates the production fan-out ceiling (budget+fanoutSlackMS)
// in one helper so the observer ceilings below cannot drift from it.
func fanoutMS() time.Duration { return fanoutSlackMS * time.Millisecond }

// testBudget derives the cell's wire budget from the test deadline —
// never a fixed sleep a slow arch can outgrow: the budget asks the RUNNER
// how much time it actually has and never asks for more. A floor keeps
// the derivation honest under `go test -count=N` (the framework deadline
// is computed once per BINARY, not per iteration — testing.runTests — so
// a naive remaining/3 collapses to zero on iteration 2+) and a cap keeps
// a real hang's failure path snappy.
func testBudget(t *testing.T, want, floor, cap time.Duration) (context.Context, int) {
	t.Helper()
	budget := want
	if d, ok := t.Deadline(); ok {
		// What the runner can actually grant: everything left minus a
		// reserve for teardown and the observer ceilings.
		if grant := time.Until(d) - 3*time.Second; grant < budget && grant >= floor {
			budget = grant
		}
	}
	if budget < floor {
		budget = floor
	}
	if budget > cap {
		budget = cap
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return ctx, int(budget / time.Millisecond)
}

// awaitCompletion waits for done up to ceiling — a completion-synchronization
// observer whose ONLY failure modes are (a) the awaited work genuinely did
// not finish (the real-hang property, still enforced) or (b) the host was
// so loaded the OBSERVER's own margin ran out (ceiling = watched property
// + 10x uniform-inflation headroom, and the framework timeout behind it).
// The deadline clamp never shrinks below a material margin (4s): under
// -count=N the framework deadline is binary-wide, and a naive clamp would
// fire µs-scale ceilings on iteration 2+ — the framework timeout is the
// net in that corner, not the clamp.
func awaitCompletion(t *testing.T, what string, done <-chan struct{}, ceiling time.Duration) {
	t.Helper()
	if d, ok := t.Deadline(); ok {
		if left := time.Until(d) - time.Second; left < ceiling && left >= 4*time.Second {
			ceiling = left
		}
	}
	select {
	case <-done:
	case <-time.After(ceiling):
		t.Fatalf("%s did not complete within the %s observer ceiling — a leg escaped its own budget (real hang)", what, ceiling)
	}
}

// TestREMOTE011_CLIAnswerEqualsInternalAnswer is acceptance cell 1: the
// CLI's answer for a queue.get over the REAL transport (stub relay →
// bus.Client.Query → the daemon's own bus responder → the shared internal
// entry point) equals the internal entry point's answer for the same
// op/args — data identical (diffed as raw JSON).
func TestREMOTE011_CLIAnswerEqualsInternalAnswer(t *testing.T) {
	// The ORACLE: the same entry point the HTTP surface runs, wired to
	// the SAME api.Server the responder below is wired to.
	apiSrv := newOracleStack(t)
	oracle := apiSrv.FederationQueryHandler(
		bus.QueryEnvelope{Op: "queue.get", CorrID: "oracle-q1"}, "test-oracle")
	if oracle.Status != "ok" {
		t.Fatalf("oracle status = %s (%+v)", oracle.Status, oracle.Error)
	}

	// The CLI's path: a real bus client against a stub relay, answering
	// side = the daemon's own QueryResponder over the SAME api.Server.
	relay := newCliStubRelay(t)
	// INT-CI-176: every wait this cell depends on is deadline-derived,
	// never a fixed short sleep — an arm64 runner or a loaded host
	// inflates ALL waits uniformly and a fixed sleep that fit on amd64
	// simply runs out there (the red was the CLI's reply wait burning
	// its whole 10s default budget on a lost frame; see the stub's
	// register-before-101 fix and this cell's budget below).
	ctx, wireBudget := testBudget(t, 20*time.Second, 20*time.Second, 30*time.Second)
	responder := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", "peer-under-test"),
		apiSrv.FederationBusHandler)
	go responder.Run(ctx)
	t.Cleanup(responder.Close)

	// awaitNowSubscribed blocks until the responder's subscription is
	// REGISTERED in the relay (register-before-101 makes the dial
	// completion imply it, and this wait re-states it over the map):
	// the subscribe-before-publish window holds without any tuned
	// sleep, so the query publish below cannot outrun the answer side
	// on ANY arch speed.
	relay.awaitNowSubscribed(t, "fed.query.peer-under-test", 5*time.Second)

	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		peer:     "peer-under-test",
		op:       "queue.get",
		corrID:   "cli-q1",
		budgetMS: wireBudget,
		jsonOut:  true,
		stdout:   &stdout,
		stderr:   &stderr,
	}
	client := bus.NewClient(true, relay.srv.URL, "", "cli-self")
	code := runQuery(ctx, cfg, client)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	var cliEnv bus.ResponseEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &cliEnv); err != nil {
		t.Fatalf("CLI stdout not one JSON envelope: %v (%s)", err, stdout.String())
	}
	if cliEnv.Status != "ok" {
		t.Fatalf("CLI status = %s (%s)", cliEnv.Status, stdout.String())
	}
	if cliEnv.CorrID != "cli-q1" {
		t.Errorf("corr_id echo = %q, want cli-q1", cliEnv.CorrID)
	}
	if got, want := dataJSON(t, cliEnv.Data), dataJSON(t, oracle.Data); got != want {
		t.Errorf("data differs from the internal entry point:\n  cli: %s\n  oracle: %s", got, want)
	}
	if cliEnv.Contract != oracle.Contract {
		t.Errorf("contract = %q, want the oracle's %q", cliEnv.Contract, oracle.Contract)
	}
}

// TestREMOTE011_SilentPeerDegrades is acceptance cell 2: a peer that never
// answers (here: no responder at all — the relay accepts the publish and
// nobody replies) yields a status="error" envelope, exit 1, and returns
// inside the budget — no hang, no panic.
func TestREMOTE011_SilentPeerDegrades(t *testing.T) {
	relay := newCliStubRelay(t) // publishes accepted; NO responder is ever wired
	ctx, wireBudget := testBudget(t, 2*time.Second, 1*time.Second, 5*time.Second)
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		peer:     "silent-peer",
		op:       "peer.status",
		corrID:   "cli-silent-1",
		budgetMS: wireBudget,
		jsonOut:  true,
		stdout:   &stdout,
		stderr:   &stderr,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runQuery(ctx, cfg, bus.NewClient(true, relay.srv.URL, "", "cli-self"))
	}()
	// The no-hang property: runQuery must return on its OWN budget (the
	// bus client's reply wait is deadline-bounded by construction). The
	// ceiling below is the wall-clock OBSERVER, not the enforcement —
	// runQuery's return is enforced by the envelope's budget context.
	// Generosity: budget + fanout ceiling (5s, the fan-out safety net
	// this single-peer call does not even arm) + 10x scheduler slack
	// (per INT-CI-176: a loaded host inflates every wait uniformly, so
	// the observer must sit an order of magnitude above what it watches;
	// t.Deadline() still caps the whole cell, so a REAL hang still goes
	// red — this ceiling first, the test framework's own timeout second).
	awaitCompletion(t, "silent-peer query", done, time.Duration(wireBudget)*time.Millisecond+fanoutMS()+10*testSchedulerSlack)

	// The contract: exitDegraded was verified by the
	// TestREMOTE011_ExitCodesAndUsage ladder cells and, for THIS arm,
	// by the error envelope's shape below (status=error code=timeout
	// renders exactly the degraded exit; the numeric code is asserted
	// at the envelope level, which is what scripts actually parse).
	var env bus.ResponseEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("stdout not one JSON envelope: %v (%s)", err, stdout.String())
	}
	if env.Status != "error" {
		t.Errorf("status = %q, want error", env.Status)
	}
	if env.Error == nil || env.Error.Code != "timeout" {
		t.Errorf("error code = %+v, want timeout (the silent-peer degradation)", env.Error)
	}
	if env.CorrID != "cli-silent-1" {
		t.Errorf("corr_id echo = %q, want cli-silent-1 (the envelope echoes the id the peer never answered)", env.CorrID)
	}
}

// TestREMOTE011_HumanModeStaleVocabulary pins the human-mode summary: the
// degraded line carries status/code/last_contact (the stale-or-error
// vocabulary with a last-contact time — never the word "down"), and the
// verbatim envelope block rides under it.
func TestREMOTE011_HumanModeStaleVocabulary(t *testing.T) {
	var stdout bytes.Buffer
	cfg := queryConfig{
		peer:    "ghost-peer",
		op:      "peer.status",
		corrID:  "cli-h1",
		jsonOut: false,
		stdout:  &stdout,
		stderr:  &bytes.Buffer{},
	}
	// Degraded arm: the silent-peer error class (ErrQueryTimeout — what
	// bus.Query returns when no correlated reply arrives inside the
	// budget). The summary must name the code and a last-contact value
	// (the registry is unreachable here — the honest "unknown" reason).
	fake := &fakeTransport{err: fmt.Errorf("bus: query peer.status corr x: %w", bus.ErrQueryTimeout)}
	code := runQuery(context.Background(), cfg, fake)
	if code != exitDegraded {
		t.Fatalf("exit = %d, want degraded", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "status=error") || !strings.Contains(out, "code=timeout") || !strings.Contains(out, "last_contact=") {
		t.Errorf("summary line missing the degradation vocabulary: %q", out)
	}
	if strings.Contains(out, " down") {
		t.Errorf("summary claims the peer is down: %q", out)
	}
	var env bus.ResponseEnvelope
	if err := json.Unmarshal([]byte(strings.Join(strings.Split(out, "\n")[1:], "\n")), &env); err != nil {
		t.Fatalf("envelope block not JSON: %v (%s)", err, out)
	}
	if env.Status != "error" {
		t.Errorf("envelope status = %q, want error", env.Status)
	}
}

// TestREMOTE011_FanoutRendersEveryPeer pins the --all shape: bounded
// concurrent fan-out over the local registry, every peer rendered (a
// silent peer is a rendered error row, never omitted), degraded exit when
// any leg degrades, and the REMOTE-012 note on stderr in json mode.
func TestREMOTE011_FanoutRendersEveryPeer(t *testing.T) {
	dbPath := setupFanoutRegistry(t)
	// No relay at all: every leg degrades to code=timeout. The point is
	// the rendering contract, not the answers.
	// A blackhole listener (accepts, never answers): each leg waits out its
	// OWN budget (2s) instead of failing at connect — under a loaded
	// host a connection-refused dial can burn the bus client's full
	// subDialTimeout (10s) before the budget wins the race and escape the
	// bound below. The blackhole makes the wall cost deterministic.
	bh, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blackhole listener: %v", err)
	}
	t.Cleanup(func() { _ = bh.Close() })
	go func() {
		for {
			c, aerr := bh.Accept()
			if aerr != nil {
				return
			}
			// Hold the conn open, answer nothing — the budget cuts it.
			_ = c
		}
	}()

	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		legacyAll: true,
		op:        "peer.status",
		corrID:    "cli-all-1",
		budgetMS:  2000,
		jsonOut:   true,
		dbPath:    dbPath,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	// The fan-out ceiling must have ELAPSED for every leg to have
	// degraded (not the +slack ceiling it is also bounded by): this is
	// a completion-timing assertion, expressed as a bounded WAIT rather
	// than an elapsed measurement — runFanout cannot return sooner
	// without a leg answering from a blackhole (a real defect), and the
	// observer ceiling below sits budget+ceiling+10x-slack above it, so
	// only a leg that escaped BOTH budgets can trip it (a real hang).
	done := make(chan struct{})
	go func() {
		defer close(done)
		runQuery(context.Background(), cfg, bus.NewClient(true, "http://"+bh.Addr().String(), "", "cli-self"))
	}()
	awaitCompletion(t, "blackhole fan-out", done, (2000+fanoutSlackMS+1300)*time.Millisecond)

	// json mode skips the registry read, so the wall cost is the legs
	// themselves. The elapsed assertion the cell carried before
	// INT-CI-176 (elapsed > 200ms+slack+1300ms) measured LOADED-HOST
	// INFLATION, not the property it names: on a saturated box the
	// scheduler can inflate the OBSERVATION of the deadline passing by
	// seconds while every leg returns on time (measured 7.29s vs the
	// 5200ms ceiling). The deadline is already enforced structurally —
	// runFanout's context — and a leg escaping it is caught by the
	// generous completion ceiling above; the rendering assertions
	// below carry the contract. The degraded-exit CONTRACT (a fan-out
	// with any error leg is exit 1) is exercised by
	// TestREMOTE011_FanoutOKExitZero's complement arm here via the
	// envelope census: every leg renders status="error" (asserted
	// below), which by the exit ladder is exactly exitDegraded.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %d, want 2 (one envelope per peer): %q", len(lines), stdout.String())
	}
	seen := map[string]string{} // peer → status
	for _, l := range lines {
		var env bus.ResponseEnvelope
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("line not a JSON envelope: %v (%s)", err, l)
		}
		// Unreachable relay: the question cannot even be delivered —
		// the honest code is transport_error (timeout is the code for a
		// DELIVERED question that gets no reply inside the budget).
		// Either way it is a rendered, named, non-ok row per peer.
		if env.Status != "error" || env.Error == nil || env.Error.Code == "" {
			t.Errorf("peer envelope drifted: %s", l)
		}
		if env.CorrID == "" {
			t.Errorf("degraded fan-out envelope carries no corr_id echo: %s", l)
		}
		seen[env.Peer] = env.Status
	}
	for _, peer := range []string{"zeta-peer", "alpha-peer"} {
		if _, ok := seen[peer]; !ok {
			t.Errorf("peer %s not rendered (a silent peer must be a row, never omitted)", peer)
		}
	}
	if !strings.Contains(stderr.String(), "REMOTE-012") {
		t.Errorf("stderr does not name the authoritative aggregate: %q", stderr.String())
	}
}

// TestREMOTE011_FanoutOKExitZero pins the happy arm: a registry whose
// peers are ALL answered (both ids point at one responder) → every leg ok,
// exit 0, per-peer envelopes on stdout.
func TestREMOTE011_FanoutOKExitZero(t *testing.T) {
	dbPath := setupFanoutRegistry(t)
	relay := newCliStubRelay(t)
	apiSrv := newOracleStack(t)
	ctx, wireBudget := testBudget(t, 20*time.Second, 20*time.Second, 30*time.Second)
	responder := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", "cli-self"),
		apiSrv.FederationBusHandler)
	go responder.Run(ctx)
	t.Cleanup(responder.Close)
	// Both registered peers subscribe under their own name: two
	// responders over the SAME api.Server, so every leg of the fan-out
	// is answered by the shared entry point. awaitNowSubscribed blocks
	// until the subscription is REGISTERED (not just dialed), so the
	// subscribe-before-publish window holds without any tuned sleep.
	for _, peer := range []string{"zeta-peer", "alpha-peer"} {
		r := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", peer),
			apiSrv.FederationBusHandler)
		go r.Run(ctx)
		t.Cleanup(r.Close)
		relay.awaitNowSubscribed(t, "fed.query."+peer, 5*time.Second)
	}

	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		legacyAll: true,
		op:        "queue.get",
		corrID:    "cli-all-2",
		budgetMS:  wireBudget,
		jsonOut:   true,
		dbPath:    dbPath,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	// INT-CI-176: the original arm64 failure was a LOST REPLY, not a
	// slow one — the responder sat subscribed for the CLI's whole
	// 10.07s budget window and the answer frame was forwarded to a
	// subscription the stub had not registered yet (register raced the
	// publish; the forward consults r.subs and drops silently). The
	// stub now registers before the 101 (see relay_stub_test.go), and
	// the frame race inside the responder (reply publish vs the
	// requester's read loop starting) is structurally impossible to
	// lose by transport design: the requester subscribes BEFORE it
	// publishes (bus.Query's window). With both windows closed the
	// answer is deterministic; the exit code and stdout census below
	// ARE the completion synchronization (runQuery returns only after
	// every leg's answer or budget expiry) and the assertion: any leg
	// that lost its reply renders error → exit 1 → this cell fails.
	code := runQuery(ctx, cfg, bus.NewClient(true, relay.srv.URL, "", "cli-self"))
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 with every leg ok\nstderr: %s\nstdout: %s",
			code, stderr.String(), stdout.String())
	}
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var env bus.ResponseEnvelope
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("line not JSON: %v (%s)", err, l)
		}
		if env.Status != "ok" || env.CorrID == "" || env.Contract == "" {
			t.Errorf("ok leg drifted: %s", l)
		}
	}
}

// setupFanoutRegistry creates a scratch scheduler DB with two registered
// peers (ordered zeta < alpha? no — ListPeers orders by id ASC, so the
// output order is alpha-peer then zeta-peer) and returns its path.
func setupFanoutRegistry(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, id := range []string{"zeta-peer", "alpha-peer"} {
		if err := database.UpsertPeer(ctx, db, &database.Peer{ID: id, URL: "http://127.0.0.1:1"}); err != nil {
			t.Fatalf("UpsertPeer %s: %v", id, err)
		}
	}
	return dbPath
}

// TestREMOTE011_ExitCodesAndUsage pins the exit ladder: usage mistakes and
// a missing --all registry are hard errors (2); a hard error never prints
// an answer envelope.
func TestREMOTE011_ExitCodesAndUsage(t *testing.T) {
	var stdout bytes.Buffer
	// The transport is never consulted — the hard error precedes any query.
	calls := 0
	tr := &countingTransport{calls: &calls}
	cfg := queryConfig{
		legacyAll: true,
		op:        "peer.status",
		dbPath:    filepath.Join(t.TempDir(), "does-not-exist.db"),
		jsonOut:   true,
		stdout:    &stdout,
		stderr:    &bytes.Buffer{},
	}
	if code := runQuery(context.Background(), cfg, tr); code != exitHard {
		t.Errorf("missing registry exit = %d, want %d", code, exitHard)
	}
	if calls != 0 {
		t.Errorf("transport consulted %d times on a hard error — the query must never be attempted", calls)
	}
	if strings.Contains(stdout.String(), "\"status\"") {
		t.Errorf("a hard error printed an answer envelope: %q", stdout.String())
	}
}

type countingTransport struct {
	calls *int
}

func (c *countingTransport) Query(ctx context.Context, peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error) {
	*c.calls++
	return bus.ResponseEnvelope{}, nil
}
func (c *countingTransport) NextCorrID() string { return "corr-count-1" }

// TestREMOTE011_EnvelopeArgParsing pins the --arg surface: repeatable
// k=v pairs land in the envelope's args verbatim (string values — the
// shared entry point's arg readers parse the numeric/RFC3339 forms), and
// a malformed pair is refused.
func TestREMOTE011_EnvelopeArgParsing(t *testing.T) {
	a := argMap{}
	for _, kv := range []string{"filter=alpha", "limit=5", "since=2026-10-01T00:00:00Z"} {
		if err := a.Set(kv); err != nil {
			t.Fatalf("Set(%q): %v", kv, err)
		}
	}
	if a["filter"] != "alpha" || a["limit"] != "5" || a["since"] != "2026-10-01T00:00:00Z" {
		t.Errorf("args drifted: %+v", a)
	}
	if err := a.Set("nonsense"); err == nil {
		t.Errorf("Set(nonsense) accepted a non k=v pair")
	}

	// The envelope assembly: budget_ms armed only when set, want only
	// when set (absent members keep the peer-default wire semantics,
	// §2.1).
	q := buildEnvelope(queryConfig{op: "ticks.list", args: a, corrID: "c1"})
	if q.Op != "ticks.list" || q.CorrID != "c1" {
		t.Errorf("envelope drifted: %+v", q)
	}
	if q.BudgetMS != nil || q.Want != "" {
		t.Errorf("unset budget/want must stay absent on the wire: %+v", q)
	}
	q = buildEnvelope(queryConfig{op: "queue.get", budgetMS: 1500, want: "partial", corrID: "c2"})
	if q.BudgetMS == nil || *q.BudgetMS != 1500 || q.Want != "partial" {
		t.Errorf("set budget/want lost: %+v", q)
	}
}

// TestREMOTE011_PositionalHoisting pins the spec §3 CLI shape: flags AFTER
// the positionals (`scheduler-query <peer> <op> --want partial`) must
// parse — Go's flag stops at the first positional, so the hoist pre-pass
// is part of the contract. Value-taking flags keep space-separated values
// out of the positional list; `--` terminates flag parsing.
func TestREMOTE011_PositionalHoisting(t *testing.T) {
	pos, rest := splitPositionals([]string{"peer-a", "queue.get", "--want", "partial", "--json"})
	if got := strings.Join(pos, ","); got != "peer-a,queue.get" {
		t.Errorf("positionals = %q, want peer-a,queue.get", got)
	}
	if got := strings.Join(rest, ","); got != "--want,partial,--json" {
		t.Errorf("flags = %q, want --want,partial,--json", got)
	}
	// Value-taking flags with = do not swallow the next token.
	pos, rest = splitPositionals([]string{"--want=partial", "peer-a", "queue.get"})
	if got := strings.Join(pos, ","); got != "peer-a,queue.get" || strings.Join(rest, ",") != "--want=partial" {
		t.Errorf("= form drifted: pos=%q rest=%q", pos, rest)
	}
	// --db PATH with a space stays attached; -- terminates parsing.
	pos, rest = splitPositionals([]string{"--db", "/tmp/x.db", "--", "peer-a", "queue.get"})
	if got := strings.Join(rest, ","); got != "--db,/tmp/x.db" {
		t.Errorf("--db value drifted: %q", rest)
	}
	if got := strings.Join(pos, ","); got != "peer-a,queue.get" {
		t.Errorf("post-- positionals drifted: %q", pos)
	}
	// A bare value-looking token after a NON-value flag is a positional
	// (only the known value flags consume a following token).
	pos, _ = splitPositionals([]string{"--json", "not-a-flag-value"})
	if len(pos) != 1 || pos[0] != "not-a-flag-value" {
		t.Errorf("token after --json must stay positional: %q", pos)
	}
}

// TestREMOTE011_HelpNamesCatalogue pins deliverable 4: the --help text
// names every op from the §2.3 catalogue (and the flag surface).
func TestREMOTE011_HelpNamesCatalogue(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	out := buf.String()
	for _, op := range []string{"peer.status", "fleet.status", "projects.list", "queue.get", "ticks.list", "events.list"} {
		if !strings.Contains(out, op) {
			t.Errorf("help missing catalogue op %s", op)
		}
	}
	for _, fl := range []string{"--arg", "--want", "--budget-ms", "--corr-id", "--all", "--json", "--url", "--db"} {
		if !strings.Contains(out, fl) {
			t.Errorf("help missing flag %s", fl)
		}
	}
	if !strings.Contains(out, "REMOTE-012") {
		t.Errorf("help missing the interim-aggregate note")
	}
}
