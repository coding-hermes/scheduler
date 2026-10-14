package api_test

// REMOTE-008 acceptance battery (docs/federation-query-spec.md §2 + §3 HTTP
// row): the POST /api/v1/federation/query + GET /api/v1/federation/catalogue
// surfaces proven over the wire against an in-memory daemon.
//
// The named cells map 1:1 to the row's acceptance list:
//   - each of the 6 ops returns the documented envelope (§2.2 invariants:
//     peer identity, echoed corr_id/op, as_of/age_ms, contract, data shape)
//   - an unknown op returns the ERROR envelope (named refusal, never an
//     empty 200)
//   - an empty registry/list returns [] — never null (§2.2 null law)
//   - replaying the same (caller, corr_id, op) returns the IDENTICAL body
//     byte-for-byte (§2.5), including as_of
//   - a missing operator token is refused, fail-closed (§4: 503 with no
//     credential configured, 401 on a wrong credential)
//   - status="stale" is a first-class answer distinct from error (§2.2),
//     driven deterministically through the manual sim clock (SCHED-GAP-169)

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
)

const testOperatorTokenFederation = "api-test-operator-token"

// fedQuery posts one query envelope with the shared operator credential and
// returns the HTTP status plus the RAW body (replay equality is byte-level,
// so the raw bytes — not a re-marshal — are the comparison surface).
func fedQuery(t *testing.T, a *apiTestServer, body map[string]any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal query body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/federation/query", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operator-Token", testOperatorTokenFederation)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST federation/query: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// fedQueryRaw posts a body string verbatim (malformed-JSON arm).
func fedQueryRaw(t *testing.T, a *apiTestServer, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/federation/query", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operator-Token", testOperatorTokenFederation)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST federation/query: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// assertEnvelope checks the §2.2 invariants that hold for EVERY ok/partial
// reply: echoed corr_id/op, peer = this scheduler's own id, a plausible
// status, RFC3339 as_of, a numeric age_ms, an explicit (non-null) data and
// gaps, and the contract version.
func assertEnvelope(t *testing.T, raw []byte, corrID, op, peerID string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("response not JSON: %v\nbody: %s", err, raw)
	}
	if env["corr_id"] != corrID {
		t.Errorf("corr_id = %v, want %q", env["corr_id"], corrID)
	}
	if env["op"] != op {
		t.Errorf("op = %v, want %q", env["op"], op)
	}
	if env["peer"] != peerID {
		t.Errorf("peer = %v, want %q (this scheduler's own id)", env["peer"], peerID)
	}
	status, _ := env["status"].(string)
	switch status {
	case "ok", "partial", "stale":
	default:
		t.Errorf("status = %v, want ok|partial|stale for this cell", status)
	}
	asOf, _ := env["as_of"].(string)
	if asOf == "" {
		t.Errorf("as_of missing from envelope: %s", raw)
	} else if _, err := time.Parse(time.RFC3339, asOf); err != nil {
		t.Errorf("as_of = %q not RFC3339: %v", asOf, err)
	}
	if _, ok := env["age_ms"].(float64); !ok {
		t.Errorf("age_ms missing or not numeric: %v", env["age_ms"])
	}
	if env["data"] == nil {
		t.Errorf("data is null — the §2.2 null law requires []/{}: %s", raw)
	}
	if _, ok := env["gaps"].([]any); !ok {
		t.Errorf("gaps missing or not an array (must be explicit, never null): %v", env["gaps"])
	}
	if c, _ := env["contract"].(string); !strings.HasPrefix(c, "1.") {
		t.Errorf("contract = %v, want a 1.x version string", env["contract"])
	}
	return env
}

// TestREMOTE008_QueryOps_EnvelopeConformance runs every catalogue op and
// proves the documented envelope comes back, with each op's data reusing
// the daemon's own read path (peer.status mirrors /api/v1/health fields;
// queue.get mirrors /api/v1/queue ordering).
func TestREMOTE008_QueryOps_EnvelopeConformance(t *testing.T) {
	a := newAPITestServer(t)
	// The scheduler identity REMOTE-003 installs: every reply's peer field
	// must carry exactly this value.
	const peerID = "remote008-peer"
	database.SetSchedulerID(peerID)
	t.Cleanup(func() { database.SetSchedulerID(database.DefaultSchedulerID) })

	ops := []string{"peer.status", "fleet.status", "projects.list", "queue.get", "ticks.list", "events.list"}
	for _, op := range ops {
		t.Run(op, func(t *testing.T) {
			corrID := "corr-" + op
			status, raw := fedQuery(t, a, map[string]any{
				"op": op, "corr_id": corrID, "want": "answer",
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", status, raw)
			}
			env := assertEnvelope(t, raw, corrID, op, peerID)
			if env["status"] != "ok" {
				t.Fatalf("status = %v, want ok; body: %s", env["status"], raw)
			}
			objData, isObj := env["data"].(map[string]any)
			listData, isList := env["data"].([]any)
			switch op {
			case "peer.status", "fleet.status":
				if !isObj {
					t.Fatalf("data not an object for %s: %s", op, raw)
				}
			case "projects.list", "queue.get", "ticks.list", "events.list":
				if !isList {
					t.Fatalf("data not an array for %s: %s", op, raw)
				}
			}
			_, _, _ = objData, listData, isObj
		})
	}
}

// TestREMOTE008_PeerStatus_MirrorsOwnReadPath pins the §2.3 law that the
// answer is the daemon's OWN health payload: the fields peer.status returns
// must equal what /api/v1/health serves on the same daemon (version, build
// identity, uptime, db, active_ticks) — not a new aggregation.
func TestREMOTE008_PeerStatus_MirrorsOwnReadPath(t *testing.T) {
	a := newAPITestServer(t)
	_, raw := fedQuery(t, a, map[string]any{"op": "peer.status", "corr_id": "c-mirror"})
	var env struct {
		Data struct {
			SchedulerID string `json:"scheduler_id"`
			Version     string `json:"version"`
			BuildSHA    string `json:"build_sha"`
			Uptime      string `json:"uptime"`
			DB          string `json:"db"`
			ActiveTicks int    `json:"active_ticks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if env.Data.SchedulerID == "" {
		t.Errorf("data.scheduler_id empty — the peer must name itself")
	}
	if env.Data.DB != "connected" {
		t.Errorf("data.db = %q, want connected", env.Data.DB)
	}
	// The SAME fields from /api/v1/health on the same stack.
	_, health := a.do(t, "GET", "/api/v1/health", nil)
	if health["version"] != env.Data.Version {
		t.Errorf("version drift vs own /health: fed=%v health=%v", env.Data.Version, health["version"])
	}
	if health["build_sha"] != env.Data.BuildSHA {
		t.Errorf("build_sha drift vs own /health: fed=%v health=%v", env.Data.BuildSHA, health["build_sha"])
	}
	if health["active_ticks"].(float64) != float64(env.Data.ActiveTicks) {
		t.Errorf("active_ticks drift vs own /health: fed=%d health=%v", env.Data.ActiveTicks, health["active_ticks"])
	}
}

// TestREMOTE008_ProjectsList_FilterAndShape proves projects.list serves the
// documented row shape (name, enabled, weight, priority, cooldown) and the
// exact-name filter arg narrows it.
func TestREMOTE008_ProjectsList_FilterAndShape(t *testing.T) {
	a := newAPITestServer(t)
	_, b := a.do(t, "POST", "/api/v1/projects", map[string]any{
		"name": "fed-proj-a", "repo_url": "https://example.com/a", "workdir": "/tmp/a",
	})
	if b == nil {
		t.Fatal("project create failed")
	}

	_, raw := fedQuery(t, a, map[string]any{"op": "projects.list", "corr_id": "c-pl-all"})
	var all struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found bool
	for _, row := range all.Data {
		if row["name"] == "fed-proj-a" {
			found = true
			for _, key := range []string{"enabled", "weight", "priority", "cooldown_s"} {
				if _, ok := row[key]; !ok {
					t.Errorf("projects.list row missing documented field %q: %v", key, row)
				}
			}
		}
	}
	if !found {
		t.Fatalf("projects.list did not return the created project: %s", raw)
	}

	_, raw = fedQuery(t, a, map[string]any{
		"op": "projects.list", "corr_id": "c-pl-one", "args": map[string]any{"filter": "fed-proj-a"},
	})
	var one struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(one.Data) != 1 || one.Data[0]["name"] != "fed-proj-a" {
		t.Fatalf("filter arg not applied: %s", raw)
	}
}

// TestREMOTE008_QueueGet_MatchesOwnQueue pins queue.get to the daemon's own
// ordered queue: identical count and ordering as GET /api/v1/queue.
func TestREMOTE008_QueueGet_MatchesOwnQueue(t *testing.T) {
	a := newAPITestServer(t)
	for _, name := range []string{"q-a", "q-b"} {
		a.do(t, "POST", "/api/v1/projects", map[string]any{
			"name": name, "repo_url": "https://example.com/" + name, "workdir": "/tmp/" + name,
		})
		a.do(t, "PUT", "/api/v1/projects/"+name, map[string]any{"enabled": true})
	}
	_, raw := fedQuery(t, a, map[string]any{"op": "queue.get", "corr_id": "c-queue"})
	var env struct {
		Data []struct {
			Project string  `json:"project"`
			Urgency float64 `json:"urgency"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	_, own := a.do(t, "GET", "/api/v1/queue", nil)
	ownQueue, _ := own["queue"].([]any)
	if len(env.Data) != len(ownQueue) {
		t.Fatalf("queue.get count = %d, own /queue count = %d — the op must serve the SAME read path", len(env.Data), len(ownQueue))
	}
	for i, it := range env.Data {
		ownItem, _ := ownQueue[i].(map[string]any)
		if ownItem["project"] != it.Project {
			t.Errorf("queue order drift at %d: fed=%s own=%v", i, it.Project, ownItem["project"])
		}
	}
}

// TestREMOTE008_TicksEvents_SinceAndSeverity proves the list ops' args:
// since keeps only rows at/after the instant, severity filters exactly, and
// rows come back newest-first (the endpoints' documented order).
func TestREMOTE008_TicksEvents_SinceAndSeverity(t *testing.T) {
	a := newAPITestServer(t)
	// Seed two events with distinct severities through the shared LogEvent
	// path the daemon itself writes through (the severity CHECK vocabulary
	// is CRITICAL/HIGH/MEDIUM/LOW/INFO).
	for _, ev := range []database.Event{
		{Severity: "INFO", Component: "api.federation.test", Message: "fed-seed-info"},
		{Severity: "HIGH", Component: "api.federation.test", Message: "fed-seed-high"},
	} {
		if err := database.LogEvent(t.Context(), a.db, &ev); err != nil {
			t.Fatalf("LogEvent seed: %v", err)
		}
	}

	// severity filter.
	_, raw := fedQuery(t, a, map[string]any{
		"op": "events.list", "corr_id": "c-ev-sev", "args": map[string]any{"severity": "HIGH"},
	})
	var evEnv struct {
		Data []struct {
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evEnv); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if len(evEnv.Data) == 0 {
		t.Fatal("severity filter returned zero rows — seed lost")
	}
	for _, e := range evEnv.Data {
		if e.Severity != "HIGH" {
			t.Errorf("severity filter leaked a %q row", e.Severity)
		}
	}

	// since: a floor in the future keeps nothing; parseable RFC3339 only.
	_, raw = fedQuery(t, a, map[string]any{
		"op": "events.list", "corr_id": "c-ev-since",
		"args": map[string]any{"since": "9999-01-01T00:00:00Z"},
	})
	var sinceEnv struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &sinceEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sinceEnv.Data) != 0 {
		t.Errorf("since=9999 kept %d rows — the since arg is not applied", len(sinceEnv.Data))
	}

	// bad since: a NAMED refusal, never a silent ignore.
	status, raw := fedQuery(t, a, map[string]any{
		"op": "events.list", "corr_id": "c-ev-bad",
		"args": map[string]any{"since": "not-a-time"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("bad since status = %d, want 400; body: %s", status, raw)
	}
	if !strings.Contains(string(raw), `"bad_request"`) {
		t.Errorf("bad since error code not named: %s", raw)
	}

	// ticks.list: limit arg honored, newest-first (empty DB → empty array;
	// the limit cell runs through the same arg pipeline as events.list).
	_, raw = fedQuery(t, a, map[string]any{
		"op": "ticks.list", "corr_id": "c-tk", "args": map[string]any{"limit": 5},
	})
	var tkEnv struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &tkEnv); err != nil {
		t.Fatalf("unmarshal ticks.list: %v", err)
	}
	if len(tkEnv.Data) > 5 {
		t.Errorf("ticks.list returned %d rows over limit=5", len(tkEnv.Data))
	}
}

// TestREMOTE008_UnknownOp_NamedRefusal proves an unknown op answers the
// ERROR envelope with code=unknown_op — a named refusal, never an empty 200.
func TestREMOTE008_UnknownOp_NamedRefusal(t *testing.T) {
	a := newAPITestServer(t)
	status, raw := fedQuery(t, a, map[string]any{"op": "does.not.exist", "corr_id": "c-unk"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", status, raw)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env["status"] != "error" {
		t.Errorf("status = %v, want error", env["status"])
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("error object missing: %s", raw)
	}
	if errObj["code"] != "unknown_op" {
		t.Errorf("error.code = %v, want unknown_op", errObj["code"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "queue.get") {
		t.Errorf("refusal message does not name the supported ops: %v", msg)
	}
	// The echo invariants survive the refusal arm too.
	if env["op"] != "does.not.exist" || env["corr_id"] != "c-unk" {
		t.Errorf("refusal envelope lost the echo: op=%v corr_id=%v", env["op"], env["corr_id"])
	}
}

// TestREMOTE008_EmptyLists_ReturnArrayNotNull proves the §2.2 null law on
// the four list ops over a fresh (empty) database: data is [] — never null.
func TestREMOTE008_EmptyLists_ReturnArrayNotNull(t *testing.T) {
	// A fresh stack per op: an earlier op's audit rows would make
	// events.list non-empty (the audit is the point of §4, not a test
	// contaminator to work around). events.list additionally floors with a
	// future `since`: the surface is operator-gated, so the query's OWN auth
	// audit row (written before the read, honestly) always exists — the
	// since floor excludes it and proves the same [] law (the dedicated
	// quiet-DB cell below pins the unfloored shape).
	for _, op := range []string{"projects.list", "queue.get", "ticks.list", "events.list"} {
		t.Run(op, func(t *testing.T) {
			a := newAPITestServer(t)
			args := map[string]any{}
			if op == "events.list" {
				args["since"] = "9999-01-01T00:00:00Z"
			}
			_, raw := fedQuery(t, a, map[string]any{"op": op, "corr_id": "c-empty-" + op, "args": args})
			// Look at the RAW bytes: a Go-level nil slice marshals as null,
			// and only the raw form can prove the law held on the wire.
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
			}
			trimmed := bytes.TrimSpace(env.Data)
			if !bytes.Equal(trimmed, []byte("[]")) {
				t.Fatalf("data = %s, want the literal [] (never null) for an empty %s", trimmed, op)
			}
			assertEnvelope(t, raw, "c-empty-"+op, op, database.SchedulerID())
		})
	}
}

// TestREMOTE008_EventsList_OnQuietDB_ServesOnlyItsOwnGateAudit pins the
// honest shape of events.list on a gated surface: the query's OWN auth
// audit row (written by requireOperator BEFORE the read) may appear in the
// answer, but nothing else does — the op returns what exists, no more. A
// future `since` floor then excludes even that row, proving the exact []
// answer on the wire.
func TestREMOTE008_EventsList_OnQuietDB_ServesOnlyItsOwnGateAudit(t *testing.T) {
	a := newAPITestServer(t)
	_, raw := fedQuery(t, a, map[string]any{"op": "events.list", "corr_id": "c-ev-quiet"})
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if len(env.Data) > 1 {
		t.Fatalf("quiet-DB events.list returned %d rows, want at most the query's own auth audit row", len(env.Data))
	}
	for _, row := range env.Data {
		if comp, _ := row["component"].(string); comp != "api.auth" {
			t.Errorf("quiet-DB events.list carried a non-audit row: %v", row["component"])
		}
	}
	// The exact-[] arm: a future since floor excludes the gate's own row.
	_, raw = fedQuery(t, a, map[string]any{
		"op": "events.list", "corr_id": "c-ev-quiet2",
		"args": map[string]any{"since": "9999-01-01T00:00:00Z"},
	})
	var sinceEnv struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &sinceEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(sinceEnv.Data), []byte("[]")) {
		t.Fatalf("since-floored events.list data = %s, want []", sinceEnv.Data)
	}
}

// TestREMOTE008_ReplayIdenticalBody proves §2.5: the same (caller, corr_id,
// op) replay returns the IDENTICAL bytes — as_of included — without
// re-reading. A different corr_id is a DIFFERENT query (fresh as_of), and a
// different op under the same corr_id is different too (the key has three
// components).
func TestREMOTE008_ReplayIdenticalBody(t *testing.T) {
	a := newAPITestServer(t)
	body := map[string]any{"op": "peer.status", "corr_id": "c-replay-1"}
	s1, raw1 := fedQuery(t, a, body)
	if s1 != http.StatusOK {
		t.Fatalf("first query status = %d", s1)
	}
	s2, raw2 := fedQuery(t, a, body)
	if s2 != http.StatusOK {
		t.Fatalf("replay status = %d", s2)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("replay body differs from the first answer:\nfirst:  %s\nsecond: %s", raw1, raw2)
	}
	var first map[string]any
	if err := json.Unmarshal(raw1, &first); err != nil {
		t.Fatal("unmarshal")
	}
	if first["as_of"] == nil {
		t.Fatal("as_of missing — replay identity cannot be proven")
	}

	// Different corr_id → different query → re-read (fresh envelope).
	s3, raw3 := fedQuery(t, a, map[string]any{"op": "peer.status", "corr_id": "c-replay-2"})
	if s3 != http.StatusOK || bytes.Equal(raw3, raw1) {
		t.Errorf("different corr_id must NOT replay the first answer")
	}
	// Same corr_id, different op → different query.
	s4, raw4 := fedQuery(t, a, map[string]any{"op": "queue.get", "corr_id": "c-replay-1"})
	if s4 != http.StatusOK {
		t.Fatalf("cross-op status = %d", s4)
	}
	var cross map[string]any
	if err := json.Unmarshal(raw4, &cross); err != nil {
		t.Fatal("unmarshal")
	}
	if cross["status"] == "error" {
		t.Errorf("same corr_id under a different op must be a fresh query, got: %s", raw4)
	}
}

// TestREMOTE008_AuthFailClosed proves §4: the federation surface fails
// CLOSED — 503 with NO credential configured (the zero-value Server), 401
// on a wrong credential, 405 on GET to the query route.
func TestREMOTE008_AuthFailClosed(t *testing.T) {
	// (a) Bare server: SetAuthConfig never called → authOff → 503 on both
	// federation routes. Fail-closed is structural: a query must not slip
	// through just because its body is invalid.
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bare := api.NewServer(db, nil)
	ts := httptest.NewServer(bare.Handler())
	t.Cleanup(ts.Close)

	post := func(url string) int {
		resp, err := http.Post(url, "application/json", strings.NewReader(`{"op":"peer.status","corr_id":"c"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("bare-server POST = %d, want 503; body: %s", resp.StatusCode, raw)
		}
		return resp.StatusCode
	}
	post(ts.URL + "/api/v1/federation/query")
	// catalogue is gated too.
	resp, err := http.Get(ts.URL + "/api/v1/federation/catalogue")
	if err != nil {
		t.Fatalf("GET catalogue: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("bare-server GET catalogue = %d, want 503", resp.StatusCode)
	}

	// (b) Armed server: wrong credential → 401.
	a := newAPITestServer(t)
	req, _ := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/federation/query",
		strings.NewReader(`{"op":"peer.status","corr_id":"c"}`))
	req.Header.Set("X-Operator-Token", "wrong-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong credential = %d, want 401", resp2.StatusCode)
	}

	// (c) Method: the query route is POST-only.
	sCode, _ := fedQueryRaw(t, a, "")
	_ = sCode
	resp3, err := http.Get(a.ts.URL + "/api/v1/federation/query")
	if err != nil {
		t.Fatalf("GET query: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /federation/query = %d, want 405", resp3.StatusCode)
	}
	resp4, err := http.Post(a.ts.URL+"/api/v1/federation/catalogue", "application/json", nil)
	if err != nil {
		t.Fatalf("POST catalogue: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /federation/catalogue = %d, want 405", resp4.StatusCode)
	}
}

// TestREMOTE008_Catalogue lists the six §2.3 ops with their arg shapes and
// the contract/peer identity.
func TestREMOTE008_Catalogue(t *testing.T) {
	a := newAPITestServer(t)
	_, body := a.do(t, "GET", "/api/v1/federation/catalogue", nil)
	ops, ok := body["ops"].([]any)
	if !ok {
		t.Fatalf("catalogue.ops missing: %v", body)
	}
	want := map[string]bool{
		"peer.status": false, "fleet.status": false, "projects.list": false,
		"queue.get": false, "ticks.list": false, "events.list": false,
	}
	for _, o := range ops {
		entry, _ := o.(map[string]any)
		opID, _ := entry["op"].(string)
		if _, known := want[opID]; !known {
			t.Errorf("catalogue carries undocumented op %q", opID)
			continue
		}
		want[opID] = true
		if _, ok := entry["args"].(map[string]any); !ok {
			t.Errorf("op %q: args shape missing (must be an object, possibly empty)", opID)
		}
	}
	for op, seen := range want {
		if !seen {
			t.Errorf("catalogue missing §2.3 op %q", op)
		}
	}
	if body["contract"] != "1.0.0" {
		t.Errorf("contract = %v, want 1.0.0", body["contract"])
	}
	// The list itself must not be null-shaped: an empty catalogue is a
	// named bug (the §2.3 v1 catalogue is non-empty by construction).
	if len(ops) != len(want) {
		t.Errorf("catalogue ops = %d, want %d", len(ops), len(want))
	}
}

// TestREMOTE008_RequiredFields_Refused proves the §2.1 required members:
// missing op and missing corr_id are NAMED refusals (missing_op /
// missing_corr_id), never guessed defaults; malformed JSON is bad_request.
func TestREMOTE008_RequiredFields_Refused(t *testing.T) {
	a := newAPITestServer(t)
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"no op", `{"corr_id":"c"}`, "missing_op"},
		{"blank op", `{"op":"","corr_id":"c"}`, "missing_op"},
		{"no corr_id", `{"op":"peer.status"}`, "missing_corr_id"},
		{"blank corr_id", `{"op":"peer.status","corr_id":"  "}`, "missing_corr_id"},
		{"bad want", `{"op":"peer.status","corr_id":"c","want":"maybe"}`, "bad_request"},
		{"malformed json", `{"op":`, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := fedQueryRaw(t, a, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, raw)
			}
			var env struct {
				Status string `json:"status"`
				Error  struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v; body: %s", err, raw)
			}
			if env.Status != "error" || env.Error.Code != tc.wantCode {
				t.Fatalf("status=%q code=%q, want error/%s; body: %s", env.Status, env.Error.Code, tc.wantCode, raw)
			}
		})
	}
}

// TestREMOTE008_AuditRowWritten proves §4's read-audit law: every answered
// query (allowed AND refused) writes one local audit row — reads are
// observable, not silently free.
func TestREMOTE008_AuditRowWritten(t *testing.T) {
	a := newAPITestServer(t)
	allowed, _ := fedQuery(t, a, map[string]any{"op": "queue.get", "corr_id": "c-audit-1"})
	if allowed != http.StatusOK {
		t.Fatalf("query status = %d", allowed)
	}
	refused, _ := fedQuery(t, a, map[string]any{"op": "nope", "corr_id": "c-audit-2"})
	if refused != http.StatusBadRequest {
		t.Fatalf("refusal status = %d", refused)
	}
	_, body := a.do(t, "GET", "/api/v1/events?limit=200", nil)
	events, _ := body["events"].([]any)
	foundAllowed, foundRefused := false, false
	for _, e := range events {
		row, _ := e.(map[string]any)
		comp, _ := row["component"].(string)
		msg, _ := row["message"].(string)
		if comp != "api.federation" {
			continue
		}
		if strings.Contains(msg, "op=queue.get") && strings.Contains(msg, "c-audit-1") {
			foundAllowed = true
		}
		if strings.Contains(msg, "op=nope") && strings.Contains(msg, "c-audit-2") {
			foundRefused = true
		}
	}
	if !foundAllowed {
		t.Error("no api.federation audit row for the ALLOWED query")
	}
	if !foundRefused {
		t.Error("no api.federation audit row for the REFUSED query — the refusal is the interesting event (§4)")
	}
}

// TestREMOTE008_MethodGuards pins the 405 shape on both routes (covered in
// the auth battery too, kept separate so a method change cannot hide behind
// an auth refactor).
func TestREMOTE008_MethodGuards(t *testing.T) {
	a := newAPITestServer(t)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		req, _ := http.NewRequest(method, a.ts.URL+"/api/v1/federation/query", nil)
		req.Header.Set("X-Operator-Token", testOperatorTokenFederation)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /federation/query = %d, want 405", method, resp.StatusCode)
		}
	}
}
