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
	responder := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", "peer-under-test"),
		apiSrv.FederationBusHandler)
	go responder.Run(context.Background())
	t.Cleanup(responder.Close)

	relay.awaitSubscription(t, "fed.query.peer-under-test", 5*time.Second)

	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		peer:    "peer-under-test",
		op:      "queue.get",
		corrID:  "cli-q1",
		jsonOut: true,
		stdout:  &stdout,
		stderr:  &stderr,
	}
	client := bus.NewClient(true, relay.srv.URL, "", "cli-self")
	code := runQuery(context.Background(), cfg, client)
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
// well inside the budget — no hang, no panic.
func TestREMOTE011_SilentPeerDegrades(t *testing.T) {
	relay := newCliStubRelay(t) // publishes accepted; NO responder is ever wired
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		peer:     "silent-peer",
		op:       "peer.status",
		corrID:   "cli-silent-1",
		budgetMS: 250,
		jsonOut:  true,
		stdout:   &stdout,
		stderr:   &stderr,
	}
	start := time.Now()
	code := runQuery(context.Background(), cfg,
		bus.NewClient(true, relay.srv.URL, "", "cli-self"))
	elapsed := time.Since(start)

	if code != exitDegraded {
		t.Errorf("exit = %d, want %d (degraded)", code, exitDegraded)
	}
	if elapsed > 3*time.Second {
		t.Errorf("query took %s — blew far past the 250ms budget (hang?)", elapsed)
	}
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
	// OWN budget (200ms) instead of failing at connect — under a loaded
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
		budgetMS:  200,
		jsonOut:   true,
		dbPath:    dbPath,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	start := time.Now()
	code := runQuery(context.Background(), cfg, bus.NewClient(true, "http://"+bh.Addr().String(), "", "cli-self"))
	elapsed := time.Since(start)

	if code != exitDegraded {
		t.Errorf("exit = %d, want degraded", code)
	}
	// json mode skips the registry read, so the wall cost is the legs
	// themselves: budget 200ms + the 5s fan-out ceiling. The bound below
	// is set JUST above the ceiling so only a real hang (a leg escaping
	// the ceiling) can trip it — a loaded host inflates all waits
	// uniformly and must not flake the cell.
	if elapsed > (200+fanoutSlackMS+1300)*time.Millisecond {
		t.Errorf("fan-out took %s — a leg escaped the %dms ceiling", elapsed, fanoutSlackMS+200)
	}
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
	responder := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", "cli-self"),
		apiSrv.FederationBusHandler)
	go responder.Run(context.Background())
	t.Cleanup(responder.Close)
	// Both registered peers subscribe under their own name: two
	// responders over the SAME api.Server, so every leg of the fan-out
	// is answered by the shared entry point.
	for _, peer := range []string{"zeta-peer", "alpha-peer"} {
		r := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", peer),
			apiSrv.FederationBusHandler)
		go r.Run(context.Background())
		t.Cleanup(r.Close)
		relay.awaitSubscription(t, "fed.query."+peer, 5*time.Second)
	}

	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		legacyAll: true,
		op:        "queue.get",
		corrID:    "cli-all-2",
		budgetMS:  5000,
		jsonOut:   true,
		dbPath:    dbPath,
		stdout:    &stdout,
		stderr:    &stderr,
	}
	code := runQuery(context.Background(), cfg, bus.NewClient(true, relay.srv.URL, "", "cli-self"))
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
