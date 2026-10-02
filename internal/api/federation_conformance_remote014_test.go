package api_test

// REMOTE-014 (docs/federation-query-spec.md §6): the SURFACE CONFORMANCE
// BATTERY — one fixed query set run through EVERY implemented surface, the
// typed results diffed against the ONE internal query entry point (the
// oracle, spec §3: "Every adapter: parses its surface → builds the
// envelope → calls the SAME internal query entry point → returns the SAME
// response").
//
// WHAT IS COMPARED (the §2.2 contract, per surface, per op):
//   - the same field set present — the §2.2 member set actually on the
//     wire (non-null members; `error` present only for status=error per
//     §2.2's own presence rule), never omitted, never reworded;
//   - the SAME staleness labelling for the same peer/self state — driven
//     deterministically by advancing the manual sim clock (SCHED-GAP-169
//     discipline) past the freshness window: every surface must flip to
//     status="stale" together (never error, never omitted, never
//     reworded);
//   - the SAME error classification for the same failure — the stable
//     code vocabulary read off the wire (unknown_op, op_not_allowed,
//     ...), never a per-transport respelling. The error MESSAGE is the
//     human complement of the code: compared byte-exactly for the
//     caller-independent refusals, relaxed to non-empty where the refusal
//     names its caller by design (the §4 op_not_allowed message names WHO
//     was refused — that is per-caller, not per-surface, drift);
//   - the aggregate (op=fleet.aggregate) merged by the §5 rule —
//     TestREMOTE014_AggregateConformance (the aggregate is the primary's
//     HTTP-surface op; peers refuse it, which that cell also pins).
//
// THE SURFACE TABLE IS THE SKEW SEAM (deliverable 2): each row is a
// fconfSurface{Ask}; the battery runs whatever rows exist. The skew cell
// wraps EXACTLY ONE row with a mutator (drop a field, reword the stale
// label, change an error code) and asserts the same runner goes RED — a
// green battery that cannot go red proves nothing, and a refactor that
// unifies surfaces by accident is caught because the skew arm fails.
//
// HOW A NEW ACCESS TYPE SATISFIES THE BATTERY (deliverable 3 — the
// acceptance: a new surface without conformance must fail CI):
//
//  1. The new transport MUST route its answers through
//     Server.FederationQueryHandler(envelope, caller) — the ONE internal
//     query entry point every adapter shares (spec §3). An adapter that
//     computes an answer itself is a bug.
//  2. The new transport MUST add one fconfSurface row in
//     fconfBuiltinSurfaces below whose Ask returns the raw §2.2 JSON the
//     surface answers with. The battery diffs that row against the
//     shared-entry oracle automatically — no new test code.
//  3. fconfBuiltinSurfaceNames asserts the built-in rows are present; the
//     row set is the CI-visible registry of surfaces-under-conformance.
//     A transport implemented without its row has no conformance evidence
//     in CI, which is exactly the REMOTE-014 acceptance failure — and
//     deleting an existing row fails TestREMOTE014 in the same way.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// fconfOperatorToken is this battery's operator credential (the HTTP and
// MCP wires are gated; the token arms the same operator gate every
// control route uses).
const fconfOperatorToken = "remote014-conformance-operator"

// fconfStaleAdvance is how far the sim clock jumps to flip every surface
// past the freshness window in one deterministic step (the window is
// database.PeerFreshnessWindowDefault = 180s; ten minutes clears it with
// margin no loaded-host slowdown can bridge).
const fconfStaleAdvance = 10 * time.Minute

// fconfStack is the ONE fixture every in-process surface runs against:
// a single api.Server on a manual sim clock, the MCP server wired to the
// SAME entry point the daemon's main.go installs, one DB carrying the
// fixture rows. No network-real component: the HTTP and MCP "wires" are
// httptest loops over the real handlers.
type fconfStack struct {
	srv   *api.Server
	ts    *httptest.Server // the HTTP surface (POST /api/v1/federation/query)
	mcpTS *httptest.Server // the MCP surface (POST /mcp, tools/call)
	sim   *clock.SimClock
}

// newFedConfStack builds the shared fixture. withPolicy=false leaves the
// read policy un-armed (the REMOTE-013 deny-all default) for the
// op_not_allowed classification arm.
func newFedConfStack(t *testing.T, withPolicy bool) *fconfStack {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sim := clock.NewManualSimClock(time.Now())
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	// The loop gets the sim clock BEFORE NewServer (the clock.Seam holds
	// one concrete type — the newFedSimStack construction): the loop's
	// last-eval stamp and the server's answers land on the SAME timeline,
	// so one Advance deterministically re-labels every surface.
	loop.SetClock(sim)
	srv := api.NewServer(db, loop)
	srv.SetAuthConfig(api.ResolveAuthConfig(fconfOperatorToken, "", ""))
	if withPolicy {
		srv.SetFederationReadPolicy(api.ResolveFederationReadPolicy([]api.FederationReadGrant{
			{Caller: api.FederationAllowAll, Ops: []string{
				"peer.status", "fleet.status", "projects.list",
				"queue.get", "ticks.list", "events.list", "fleet.aggregate",
			}},
		}))
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// The MCP adapter wired exactly as main.go does: ONE api.Server
	// (one replay window, one audit path, one freshness clock shared by
	// every transport) feeding the MCP server's federation entry point.
	mcpSrv := mcpserver.NewServer(db, loop)
	mcpSrv.SetFederationQueryHandler(srv.FederationQueryHandler)
	mcpSrv.SetOperatorAuth(api.ResolveAuthConfig(fconfOperatorToken, "", ""))
	mcpTS := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(mcpTS.Close)

	// Fixture rows (the established fixture shapes): one project, one
	// event, one tick — data the read ops return identically no matter
	// which surface asks.
	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{
		Name:     "alpha014",
		RepoURL:  "https://example.com/alpha014",
		Workdir:  "/tmp/remote014-alpha014",
		Weight:   10,
		Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "remote014-conformance",
		Message:   "fixture event row",
	}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if err := database.CreateTick(ctx, db, &database.Tick{
		ID:          database.NextTickID(ctx, "alpha014"),
		ProjectName: "alpha014",
		Status:      database.StatusQueued,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	return &fconfStack{srv: srv, ts: ts, mcpTS: mcpTS, sim: sim}
}

// ── the surface table (the seam) ───────────────────────────────────────────

// fconfSurface is one implemented access type under the battery. Ask runs
// one query on the surface and returns the RAW §2.2 JSON the surface
// answers with (the wire form — the battery normalizes, the surface
// renders).
type fconfSurface struct {
	name string
	ask  func(t *testing.T, op string, args map[string]any, corrID string) []byte
}

// fconfBuiltinSurfaceNames is the CI-visible registry of surfaces the
// battery covers (deliverable 3): shared-entry is the oracle itself
// (Server.FederationQueryHandler — the ONE internal query entry point),
// then the HTTP wire, the bus handler, and the MCP JSON-RPC surface. The
// CLI's rendered form is covered by the cmd/scheduler-query battery
// (TestREMOTE014_CLIRenderedFormConforms); the aggregate by
// TestREMOTE014_AggregateConformance. See the file comment for how a NEW
// access type satisfies the battery.
var fconfBuiltinSurfaceNames = []string{"shared-entry", "http", "bus", "mcp"}

// fconfBuiltinSurfaces wires the four in-process rows against one stack.
func fconfBuiltinSurfaces(st *fconfStack) []fconfSurface {
	return []fconfSurface{
		{
			name: "shared-entry",
			ask: func(_ *testing.T, op string, args map[string]any, corrID string) []byte {
				env := st.srv.FederationQueryHandler(
					bus.QueryEnvelope{Op: op, Args: args, CorrID: corrID},
					"fconf-shared-entry")
				b, err := json.Marshal(env)
				if err != nil {
					panic("shared-entry envelope marshal: " + err.Error())
				}
				return b
			},
		},
		{
			name: "http",
			ask: func(t *testing.T, op string, args map[string]any, corrID string) []byte {
				t.Helper()
				body := map[string]any{"op": op, "corr_id": corrID}
				if args != nil {
					body["args"] = args
				}
				b, err := json.Marshal(body)
				if err != nil {
					t.Fatalf("marshal query body: %v", err)
				}
				req, err := http.NewRequest(http.MethodPost, st.ts.URL+"/api/v1/federation/query", bytes.NewReader(b))
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Operator-Token", fconfOperatorToken)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("POST federation/query: %v", err)
				}
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				return raw
			},
		},
		{
			name: "bus",
			ask: func(t *testing.T, op string, args map[string]any, corrID string) []byte {
				t.Helper()
				env := st.srv.FederationQueryHandler(
					bus.QueryEnvelope{Op: op, Args: args, CorrID: corrID},
					api.FederationBusCaller)
				b, err := json.Marshal(env)
				if err != nil {
					t.Fatalf("bus envelope marshal: %v", err)
				}
				return b
			},
		},
		{
			name: "mcp",
			ask: func(t *testing.T, op string, args map[string]any, corrID string) []byte {
				t.Helper()
				toolArgs := map[string]interface{}{"op": op, "corr_id": corrID}
				if args != nil {
					toolArgs["args"] = args
				}
				reqBody, err := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"method":  "tools/call",
					"params":  map[string]interface{}{"name": "fed_query", "arguments": toolArgs},
				})
				if err != nil {
					t.Fatalf("marshal JSON-RPC: %v", err)
				}
				req, err := http.NewRequest(http.MethodPost, st.mcpTS.URL+"/mcp", bytes.NewReader(reqBody))
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Operator-Token", fconfOperatorToken)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("POST /mcp: %v", err)
				}
				defer resp.Body.Close()
				var rpc struct {
					Result *struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
					Error *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
					t.Fatalf("decode JSON-RPC response: %v", err)
				}
				if rpc.Error != nil {
					t.Fatalf("fed_query JSON-RPC error: %s", rpc.Error.Message)
				}
				if rpc.Result == nil || len(rpc.Result.Content) == 0 {
					t.Fatalf("fed_query returned no content")
				}
				return []byte(rpc.Result.Content[0].Text)
			},
		},
	}
}

// ── normalization + comparison (the §2.2 typed view) ───────────────────────

// fedSurfaceResult is the typed view of one surface's answer: the members
// the §2.2 contract fixes, normalized the way §6 prescribes (the
// transport-specific peer/timing members compared only by PRESENCE; the
// contract members compared exactly; every data member canonicalized
// through decode→encode so struct-field order can never masquerade as
// surface drift).
type fedSurfaceResult struct {
	name     string
	corrID   string
	status   string
	dataJSON string // the data member re-marshaled ("" never; "null" for the error-envelope null)
	errCode  string
	errMsg   string
	contract string
	fields   []string // the NON-NULL §2.2 members present on the wire, sorted
}

// fconfNormalize parses one surface's raw answer into the typed view.
func fconfNormalize(t *testing.T, surface string, raw []byte) fedSurfaceResult {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("surface %s: answer is not a §2.2 JSON envelope: %v\n%s", surface, err, raw)
	}
	res := fedSurfaceResult{name: surface}
	for k, v := range env {
		if v == nil {
			continue // the §2.2 presence rule: null members are absent
		}
		res.fields = append(res.fields, k)
	}
	sort.Strings(res.fields)
	res.corrID, _ = env["corr_id"].(string)
	res.status, _ = env["status"].(string)
	res.contract, _ = env["contract"].(string)
	if d, ok := env["data"]; ok && d != nil {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("surface %s: re-marshal data: %v", surface, err)
		}
		res.dataJSON = string(b)
	} else {
		res.dataJSON = "null"
	}
	if e, ok := env["error"].(map[string]any); ok {
		res.errCode, _ = e["code"].(string)
		res.errMsg, _ = e["message"].(string)
	}
	return res
}

// fconfCompare diffs one surface's typed answer against the oracle's for
// the same op. msgExact requires byte-equal error messages (the
// caller-independent refusals); when false, the message must merely be
// non-empty (a named refusal has a human message — never an empty
// answer) because the §4 op_not_allowed message names its CALLER by
// design. Returns the mismatch lines (empty = conforming).
func fconfCompare(op, wantCorrID string, oracle, got fedSurfaceResult, msgExact bool) []string {
	var problems []string
	if got.corrID != wantCorrID {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: corr_id echo = %q, want %q",
			op, got.name, got.corrID, wantCorrID))
	}
	if strings.Join(got.fields, ",") != strings.Join(oracle.fields, ",") {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: §2.2 field set drifted — got [%s], oracle [%s]",
			op, got.name, strings.Join(got.fields, " "), strings.Join(oracle.fields, " ")))
	}
	if got.status != oracle.status {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: status = %q, oracle %q (the staleness/ok/error label must never be reworded per surface)",
			op, got.name, got.status, oracle.status))
	}
	if got.dataJSON != oracle.dataJSON {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: data differs from the internal entry point —\n  got:    %s\n  oracle: %s",
			op, got.name, got.dataJSON, oracle.dataJSON))
	}
	if got.errCode != oracle.errCode {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: error code = %q, oracle %q (the §2.2 code vocabulary is the contract)",
			op, got.name, got.errCode, oracle.errCode))
	}
	if msgExact {
		if got.errMsg != oracle.errMsg {
			problems = append(problems, fmt.Sprintf("op=%s surface=%s: error message drifted from the shared entry point —\n  got:    %s\n  oracle: %s",
				op, got.name, got.errMsg, oracle.errMsg))
		}
	} else if got.errCode != "" && strings.TrimSpace(got.errMsg) == "" {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: error code %q carries no human message (a named refusal, never silence)",
			op, got.name, got.errCode))
	}
	if got.contract != oracle.contract {
		problems = append(problems, fmt.Sprintf("op=%s surface=%s: contract = %q, oracle %q",
			op, got.name, got.contract, oracle.contract))
	}
	return problems
}

// fconfOp is one entry of the fixed query set the battery runs everywhere.
type fconfOp struct {
	name string
	args map[string]any
}

// fconfQuerySet is the fixed set: an ok read, an args-passthrough read,
// and an unknown op (the error-classification arm — every surface must
// answer the SAME unknown_op code, with the SAME caller-free message, the
// oracle answers).
var fconfQuerySet = []fconfOp{
	{name: "queue.get"},
	{name: "projects.list", args: map[string]any{"filter": "alpha014"}},
	{name: "not.an.op"},
}

// fconfRunBattery runs the query set through every surface row and diffs
// each against the shared-entry oracle. tag namespaces the corr_ids so
// two runs against one stack never collide in the §2.5 replay window (a
// replayed answer would freeze the first run's status/as_of into the
// second). Returns the problem lines (empty = every surface conformed).
func fconfRunBattery(t *testing.T, surfaces []fconfSurface, ops []fconfOp, tag string, msgExact bool) []string {
	t.Helper()
	var problems []string
	for _, op := range ops {
		// The oracle runs FIRST for each op (the §6 diff is per op against
		// the internal entry point's answer for THAT op).
		var oracle fedSurfaceResult
		haveOracle := false
		for _, s := range surfaces {
			if s.name != "shared-entry" {
				continue
			}
			if haveOracle {
				t.Fatalf("duplicate shared-entry row in the surface table")
			}
			corrID := "fconf-" + tag + "-" + strings.ReplaceAll(op.name, ".", "-") + "-oracle"
			oracle = fconfNormalize(t, s.name, s.ask(t, op.name, op.args, corrID))
			haveOracle = true
		}
		if !haveOracle {
			t.Fatalf("surface table has no shared-entry oracle row — the battery has no §3 internal entry point to diff against")
		}
		for _, s := range surfaces {
			if s.name == "shared-entry" {
				continue
			}
			corrID := "fconf-" + tag + "-" + strings.ReplaceAll(op.name, ".", "-") + "-" + s.name
			res := fconfNormalize(t, s.name, s.ask(t, op.name, op.args, corrID))
			problems = append(problems, fconfCompare(op.name, corrID, oracle, res, msgExact)...)
		}
	}
	return problems
}

// fconfWithSkew wraps ONE surface row with a mutator run over its parsed
// envelope — THE SKEW SEAM (deliverable 2): the same battery, one surface
// deliberately skewed.
func fconfWithSkew(s fconfSurface, mutate func(env map[string]any)) fconfSurface {
	return fconfSurface{
		name: s.name,
		ask: func(t *testing.T, op string, args map[string]any, corrID string) []byte {
			t.Helper()
			raw := s.ask(t, op, args, corrID)
			var env map[string]any
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("skew wrapper: surface %s answer not JSON: %v", s.name, err)
			}
			mutate(env)
			b, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("skew wrapper re-marshal: %v", err)
			}
			return b
		},
	}
}

// fconfSurfaceRow returns the table row with the given name.
func fconfSurfaceRow(t *testing.T, surfaces []fconfSurface, name string) fconfSurface {
	t.Helper()
	for _, s := range surfaces {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("surface row %q not in the table", name)
	return fconfSurface{}
}

// ── the cells ───────────────────────────────────────────────────────────────

// TestREMOTE014_SurfaceConformance_AllSurfacesAgree is deliverable 1: the
// fixed query set through every surface, twice — fresh (status=ok) and
// after one deterministic sim-clock advance past the freshness window
// (status=stale on EVERY surface together). Zero problems both runs; the
// explicit stale-label assertions keep the second run from being vacuous.
func TestREMOTE014_SurfaceConformance_AllSurfacesAgree(t *testing.T) {
	st := newFedConfStack(t, true)
	surfaces := fconfBuiltinSurfaces(st)

	// Deliverable 3: the table must cover every built-in surface.
	have := map[string]bool{}
	for _, s := range surfaces {
		have[s.name] = true
	}
	for _, want := range fconfBuiltinSurfaceNames {
		if !have[want] {
			t.Errorf("the conformance surface table lost its %q row — a surface without a row has no conformance evidence in CI", want)
		}
	}

	// Run 1: fresh clock — every surface answers ok, identically.
	if problems := fconfRunBattery(t, surfaces, fconfQuerySet, "fresh", true); len(problems) > 0 {
		t.Fatalf("fresh-clock conformance failed:\n  %s", strings.Join(problems, "\n  "))
	}

	// The freshness flip, driven on the shared clock (SCHED-GAP-169): one
	// advance past the window must re-label every surface stale — never
	// error, never omitted, never per-surface reworded.
	st.sim.Advance(fconfStaleAdvance)
	oracleEnv := st.srv.FederationQueryHandler(
		bus.QueryEnvelope{Op: "queue.get", CorrID: "fconf-stale-oracle"}, "fconf-shared-entry")
	if oracleEnv.Status != "stale" {
		t.Fatalf("oracle status after the freshness-window advance = %q, want \"stale\" — the stale arm is not exercised", oracleEnv.Status)
	}

	// Run 2: stale clock — the SAME labelling on every surface.
	if problems := fconfRunBattery(t, surfaces, fconfQuerySet, "stale", true); len(problems) > 0 {
		t.Fatalf("stale-clock conformance failed:\n  %s", strings.Join(problems, "\n  "))
	}
	// ...and the label itself is the §2.2 stale answer on the wire, with
	// the full member set intact (staleness never degrades the envelope).
	httpRaw := fconfSurfaceRow(t, surfaces, "http").ask(t, "queue.get", nil, "fconf-stale-http")
	httpRes := fconfNormalize(t, "http", httpRaw)
	if httpRes.status != "stale" {
		t.Errorf("http status after the advance = %q, want stale (the §2.2 first-class stale answer)", httpRes.status)
	}
	if httpRes.dataJSON == "null" {
		t.Errorf("a stale answer carries its data (spec §2.2: stale is a full answer, never conflated with error)")
	}
}

// TestREMOTE014_SkewFixture_BatteryGoesRed is deliverable 2: the SAME
// battery must go RED when exactly one surface is deliberately skewed.
// Three arms — drop a §2.2 field, reword the stale label, change an error
// code — each on exactly one surface row, each asserted to fail WITH the
// skewed surface named. The control arm proves the mutation (not the
// fixture) is what fails. This is the guard against a future refactor
// that silently unifies surfaces by accident.
func TestREMOTE014_SkewFixture_BatteryGoesRed(t *testing.T) {
	// Arm 1: drop the contract member on the MCP surface.
	{
		st := newFedConfStack(t, true)
		surfaces := fconfBuiltinSurfaces(st)
		for i, s := range surfaces {
			if s.name == "mcp" {
				surfaces[i] = fconfWithSkew(s, func(env map[string]any) { delete(env, "contract") })
			}
		}
		problems := fconfRunBattery(t, surfaces, fconfQuerySet, "skew1", true)
		if len(problems) == 0 {
			t.Fatal("skew arm 1 (drop contract on mcp): the battery stayed GREEN — a green battery that cannot go red proves nothing")
		}
		if !fconfProblemsMention(problems, "surface=mcp") {
			t.Errorf("skew arm 1: the failure does not name the skewed surface:\n  %s", strings.Join(problems, "\n  "))
		}
	}
	// Arm 2: reword the stale label on the bus surface (stale → ok) — the
	// staleness labelling must never be per-surface reworded. The control
	// run (same stack, same clock, no mutation) must be GREEN.
	{
		st := newFedConfStack(t, true)
		st.sim.Advance(fconfStaleAdvance)
		surfaces := fconfBuiltinSurfaces(st)
		if control := fconfRunBattery(t, surfaces, fconfQuerySet, "ctl2", true); len(control) != 0 {
			t.Fatalf("control run (stale clock, no skew) already failed — the skew arms are meaningless:\n  %s", strings.Join(control, "\n  "))
		}
		for i, s := range surfaces {
			if s.name == "bus" {
				surfaces[i] = fconfWithSkew(s, func(env map[string]any) { env["status"] = "ok" })
			}
		}
		problems := fconfRunBattery(t, surfaces, fconfQuerySet, "skew2", true)
		if len(problems) == 0 {
			t.Fatal("skew arm 2 (reword stale label on bus): the battery stayed GREEN")
		}
		if !fconfProblemsMention(problems, "surface=bus") {
			t.Errorf("skew arm 2: the failure does not name the skewed surface:\n  %s", strings.Join(problems, "\n  "))
		}
	}
	// Arm 3: change the error classification on the HTTP surface — the
	// unknown-op refusal must carry the SAME code everywhere.
	{
		st := newFedConfStack(t, true)
		surfaces := fconfBuiltinSurfaces(st)
		for i, s := range surfaces {
			if s.name == "http" {
				surfaces[i] = fconfWithSkew(s, func(env map[string]any) {
					if e, ok := env["error"].(map[string]any); ok {
						e["code"] = "unknown_op_http_flavor"
					}
				})
			}
		}
		problems := fconfRunBattery(t, surfaces, []fconfOp{{name: "not.an.op"}}, "skew3", true)
		if len(problems) == 0 {
			t.Fatal("skew arm 3 (change error code on http): the battery stayed GREEN")
		}
		if !fconfProblemsMention(problems, "surface=http") {
			t.Errorf("skew arm 3: the failure does not name the skewed surface:\n  %s", strings.Join(problems, "\n  "))
		}
	}
}

// fconfProblemsMention reports whether any problem line names the surface.
func fconfProblemsMention(problems []string, needle string) bool {
	for _, p := range problems {
		if strings.Contains(p, needle) {
			return true
		}
	}
	return false
}

// TestREMOTE014_ErrorClassificationUniform is the error-classification
// deliverable: the SAME failure on every surface yields the SAME stable
// code. Against the deny-all posture (no read policy armed — the REMOTE-013
// default), every surface must answer op_not_allowed — never an empty
// answer, never a per-transport respelling. msgExact is relaxed here (the
// refusal's message names its CALLER by design — per-caller, not
// per-surface; the CODE is the classification).
func TestREMOTE014_ErrorClassificationUniform(t *testing.T) {
	st := newFedConfStack(t, false) // zero policy: every read is refused
	surfaces := fconfBuiltinSurfaces(st)
	if problems := fconfRunBattery(t, surfaces, []fconfOp{{name: "queue.get"}}, "deny", false); len(problems) > 0 {
		t.Fatalf("refusal classification is not uniform across surfaces:\n  %s", strings.Join(problems, "\n  "))
	}
	// The code itself, named per surface (equality alone could be uniform
	// garbage): every surface answers the §4 stable code.
	for _, s := range surfaces {
		res := fconfNormalize(t, s.name, s.ask(t, "queue.get", nil, "fconf-deny-direct-"+s.name))
		if res.status != "error" || res.errCode != "op_not_allowed" {
			t.Errorf("surface=%s: status=%q errCode=%q, want error/op_not_allowed (the named refusal, never silence)",
				s.name, res.status, res.errCode)
		}
	}
}
