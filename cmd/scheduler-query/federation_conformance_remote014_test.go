package main

// REMOTE-014 (docs/federation-query-spec.md §6): the CLI leg of the
// surface conformance battery — the RENDERED form. The CLI is a thin
// adapter (spec §3: it parses its surface, builds the envelope, and
// renders the §2.2 envelope verbatim), so its answer must carry the SAME
// §2.2 field set, the SAME status/staleness label, and the SAME contract
// members the internal entry point (api.Server.FederationQueryHandler —
// the §6 oracle, wired here through the real stub-relay bus transport the
// REMOTE-011 battery uses) produced for the same op/args.
//
// This file is the CLI's conformance row in the REMOTE-014 registry: the
// in-process surfaces (shared-entry / http / bus / mcp) live in
// internal/api/federation_conformance_remote014_test.go. A NEW access
// type satisfies the battery by (a) routing its answers through
// api.Server.FederationQueryHandler and (b) adding a surface row there —
// see the file comment on that battery.

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// newFedCLIOracleStack is the CLI conformance cell's own oracle stack —
// the newOracleStack construction (non-running loop, budget=0, allow-all
// read policy, one fixture project) on an in-memory DB: the cell never
// reads a registry file, and the in-memory DB keeps the battery cheap
// (the file-backed tempdir DB costs ~30s of SQLite init on a loaded dev
// host — the established cost the file-based REMOTE-010/011 cells pay).
func newFedCLIOracleStack(t *testing.T) *api.Server {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	apiSrv := api.NewServer(db, loop)
	// REMOTE-013: the CLI cell proves the transport over real answers —
	// arm the allow-all read policy on the oracle stack (the api
	// package's REMOTE-013 battery owns the policy behavior).
	apiSrv.SetFederationReadPolicy(api.ResolveFederationReadPolicy([]api.FederationReadGrant{
		{Caller: api.FederationAllowAll, Ops: []string{
			"peer.status", "fleet.status", "projects.list",
			"queue.get", "ticks.list", "events.list",
		}},
	}))
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:     "alpha014",
		RepoURL:  "https://example.com/alpha014",
		Workdir:  "/tmp/remote014-alpha014",
		Weight:   10,
		Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return apiSrv
}

// TestREMOTE014_CLIRenderedFormConforms runs one queue.get through the
// real CLI path (runQuery over bus.Client → stub relay → the daemon's own
// QueryResponder → the shared internal entry point) and diffs the
// rendered envelope against the oracle's.
func TestREMOTE014_CLIRenderedFormConforms(t *testing.T) {
	apiSrv := newFedCLIOracleStack(t)
	oracle := apiSrv.FederationQueryHandler(
		bus.QueryEnvelope{Op: "queue.get", CorrID: "fconf-cli-oracle"}, "test-oracle")
	if oracle.Status != "ok" {
		t.Fatalf("oracle status = %s (%+v)", oracle.Status, oracle.Error)
	}

	// The CLI's real transport: stub relay → bus responder over the SAME
	// api.Server (the daemon topology, no network-real component).
	relay := newCliStubRelay(t)
	responder := bus.NewQueryResponder(bus.NewClient(true, relay.srv.URL, "", "peer-under-test"),
		apiSrv.FederationBusHandler)
	go responder.Run(context.Background())
	t.Cleanup(responder.Close)
	relay.awaitNowSubscribed(t, "fed.query.peer-under-test", 5*time.Second)

	// --json mode: the NDJSON contract scripts parse. Exactly one line,
	// the §2.2 envelope verbatim.
	var stdout, stderr bytes.Buffer
	cfg := queryConfig{
		peer:    "peer-under-test",
		op:      "queue.get",
		corrID:  "fconf-cli-1",
		jsonOut: true,
		stdout:  &stdout,
		stderr:  &stderr,
	}
	code := runQuery(context.Background(), cfg, bus.NewClient(true, relay.srv.URL, "", "cli-self"))
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	lines := nonEmptyLines(stdout.String())
	if len(lines) != 1 {
		t.Fatalf("--json stdout = %d lines, want exactly one NDJSON envelope: %q", len(lines), stdout.String())
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("CLI stdout line is not a §2.2 JSON envelope: %v (%s)", err, lines[0])
	}

	// The SAME field set as the oracle's envelope (byte-level §6 rule:
	// members present, not omitted, not reworded).
	oracleRaw, err := json.Marshal(oracle)
	if err != nil {
		t.Fatalf("marshal oracle: %v", err)
	}
	var oracleEnv map[string]any
	if err := json.Unmarshal(oracleRaw, &oracleEnv); err != nil {
		t.Fatalf("decode oracle: %v", err)
	}
	gotFields, wantFields := envFieldNames(env), envFieldNames(oracleEnv)
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Errorf("CLI field set = [%s], oracle [%s] — the rendered form must carry the same §2.2 member set",
			gotFields, wantFields)
	}
	// The contract members: staleness/ok labelling verbatim, data
	// byte-equal, contract version verbatim, corr_id echoed.
	if s, _ := env["status"].(string); s != oracle.Status {
		t.Errorf("CLI status = %q, oracle %q (the staleness labelling must never be reworded per surface)", s, oracle.Status)
	}
	if got, want := mustJSON(t, env["data"]), mustJSON(t, oracleEnv["data"]); got != want {
		t.Errorf("CLI data differs from the oracle:\n  cli:    %s\n  oracle: %s", got, want)
	}
	if c, _ := env["contract"].(string); c != oracle.Contract {
		t.Errorf("CLI contract = %q, oracle %q", c, oracle.Contract)
	}
	if c, _ := env["corr_id"].(string); c != "fconf-cli-1" {
		t.Errorf("CLI corr_id echo = %q, want fconf-cli-1", c)
	}

	// Human mode: the summary line carries the SAME status vocabulary
	// (never a "down" state — the rendering law the CLI shares with the
	// aggregate rows).
	var hOut, hErr bytes.Buffer
	hcfg := queryConfig{
		peer:   "peer-under-test",
		op:     "queue.get",
		corrID: "fconf-cli-2",
		stdout: &hOut,
		stderr: &hErr,
	}
	if code := runQuery(context.Background(), hcfg, bus.NewClient(true, relay.srv.URL, "", "cli-self")); code != exitOK {
		t.Fatalf("human-mode exit = %d, want 0 (stderr: %s)", code, hErr.String())
	}
	human := hOut.String()
	if !strings.Contains(human, "status=ok") {
		t.Errorf("human summary does not carry the §2.2 status label verbatim: %q", human)
	}
	if strings.Contains(human, " down") {
		t.Errorf("human summary claims a peer is down — the vocabulary is stale/error, never \"down\": %q", human)
	}
}

// envFieldNames returns the NON-NULL §2.2 members of an envelope, sorted.
func envFieldNames(env map[string]any) []string {
	names := make([]string, 0, len(env))
	for k, v := range env {
		if v == nil {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// mustJSON marshals v to canonical JSON for the §6 data diff.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// nonEmptyLines splits s and drops blank lines.
func nonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
