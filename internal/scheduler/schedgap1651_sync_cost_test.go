package scheduler

// SCHED-GAP-1651 tests — sync lane cost optimization:
//
//  1. A router-priced FREE head must cost $0. The live defect (measured
//     2026-10-10): the primary dispatch pair never carried the router's
//     price (only the 401/403 retry hops did), so every qwen3.7-plus:free
//     duckbrain-sync tick fell through to the unknown-model fallback
//     ($2/M in, $8/M out) — 348 ticks in 7 days carried $828.77 of
//     phantom cost for a $0 lane.
//  2. Sync-family tick reports carry a COST/keys footer so cost-per-key is
//     visible on the delivered message; non-sync reports stay byte-identical.
//  3. memory_keys counts the DuckBrain HTTP write protocol (curl -X POST
//     …/api/memories through `terminal`) the SCHED-GAP-1680 tool-name rule
//     can never see — all 4625 sync ticks read 0 before this.
//  4. The canonical sync prompt carries the explicit scope/read budget.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// syncFreeRouterJSON is the live duckbrain-sync router shape (2026-10-10):
// head = a :free xkiro lane priced at $0, chain carries the same hop.
const syncFreeRouterJSON = `{
  "project": "test-sync-lane",
  "profile": "P8_SYNC",
  "resolved_at": "2026-10-10T18:00:00+00:00",
  "head": {
    "hop": 2,
    "provider": "xkiro-2",
    "model": "qwen/qwen3.7-plus:free",
    "usd_1m": 0.0,
    "in_per_m": 0.0,
    "out_per_m": 0.0,
    "data_class": "community"
  },
  "chain": [
    {"provider": "xkiro-2", "model": "qwen/qwen3.7-plus:free", "usd_1m": 0.0, "in_per_m": 0.0, "out_per_m": 0.0}
  ],
  "gate": "OPEN"
}`

// syncPricedRouterJSON is the same shape with a real in/out price split —
// proves the wiring carries NON-zero router prices through the head too.
const syncPricedRouterJSON = `{
  "project": "test-sync-lane",
  "profile": "P8_SYNC",
  "resolved_at": "2026-10-10T18:00:00+00:00",
  "head": {
    "hop": 1,
    "provider": "some-sub",
    "model": "qwen3.7-plus",
    "usd_1m": 0.5,
    "in_per_m": 0.4,
    "out_per_m": 1.6,
    "data_class": "zdr"
  },
  "chain": [
    {"provider": "some-sub", "model": "qwen3.7-plus", "usd_1m": 0.5, "in_per_m": 0.4, "out_per_m": 1.6}
  ],
  "gate": "OPEN"
}`

// spawnWithRouter runs one gateway spawn against the OK stub with the given
// canned router output and returns the outcome plus the model the gateway
// actually received in its POST body.
func spawnWithRouter(t *testing.T, tickID, routerJSON string) (TickOutcome, string) {
	t.Helper()
	db := newTestDB(t)

	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		capturedModel = payload.Model
		gatewaySpawnOKHandler(new(string))(w, r)
	}))
	defer srv.Close()

	s := NewSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "fk-test", 5*time.Second))
	s.SetNoExecFallback(true)
	s.SetRouterClient(NewRouterClient([]string{writeFakeRouter(t, routerJSON)}, 0))

	project := PackedProject{Name: "test-sync-lane", Workdir: t.TempDir()}
	tick, err := s.Spawn(project, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return tick.Wait(), capturedModel
}

// TestSCHEDGAP1651_FreeHeadCostsZero proves the spawn path wires the
// router's price for the PRIMARY dispatch pair: a gateway tick resolved to
// a router-priced $0 head must complete at cost 0 — never the unknown-model
// fallback (which for this token volume would read ~$2.40).
func TestSCHEDGAP1651_FreeHeadCostsZero(t *testing.T) {
	outcome, model := spawnWithRouter(t, "test-sync-lane-2026-10-10-18-00-00", syncFreeRouterJSON)

	if outcome.Status != TickCompleted {
		t.Fatalf("status = %v, want completed (err=%v)", outcome.Status, outcome.Error)
	}
	if model != "qwen/qwen3.7-plus:free" {
		t.Fatalf("gateway received model %q, want the router head (precondition)", model)
	}
	if outcome.TokensIn == 0 {
		t.Fatal("gateway usage missing — the test fixture must carry real tokens")
	}
	if outcome.CostUSD != 0 {
		t.Fatalf("cost = %f, want 0 — a router-priced free lane must never fall back to the sticker maps (pre-fix this tick billed ~$%.2f at the unknown-model fallback)",
			outcome.CostUSD, float64(outcome.TokensIn)/1e6*2.0+float64(outcome.TokensOut)/1e6*8.0)
	}
	if outcome.CostSource != CostSourceGateway {
		t.Fatalf("cost_source = %q, want gateway", outcome.CostSource)
	}
}

// TestSCHEDGAP1651_PricedHeadUsesRouterPrice proves the same wiring carries
// a NON-zero router price: the tick cost follows the router's in/out split,
// not the static sticker map.
func TestSCHEDGAP1651_PricedHeadUsesRouterPrice(t *testing.T) {
	outcome, model := spawnWithRouter(t, "test-sync-lane-2026-10-10-19-00-00", syncPricedRouterJSON)

	if outcome.Status != TickCompleted {
		t.Fatalf("status = %v err=%v", outcome.Status, outcome.Error)
	}
	if model != "qwen3.7-plus" {
		t.Fatalf("gateway received model %q, want the router head (precondition)", model)
	}
	// Router head price 0.4 in / 1.6 out; the OK stub reports 800 in / 20 out.
	want := 800.0/1e6*0.4 + 20.0/1e6*1.6
	if outcome.CostUSD < want*0.99 || outcome.CostUSD > want*1.01 {
		t.Fatalf("cost = %f, want ~%f (router in/out split)", outcome.CostUSD, want)
	}
}

// TestSCHEDGAP1651_SyncFooterAppendedAndScoped proves sync-family reports
// carry the COST/keys footer and non-sync reports stay byte-identical.
func TestSCHEDGAP1651_SyncFooterAppendedAndScoped(t *testing.T) {
	syncOutcome := TickOutcome{
		Project:    "blog-sync",
		TokensIn:   986757,
		TokensOut:  5286,
		CostUSD:    0,
		CostSource: CostSourceGateway,
		MemoryKeys: 5,
		PriceAsOf:  "2026-10-01",
	}
	line := syncTickCostFooter(syncOutcome)
	if line == "" {
		t.Fatal("sync lane must produce a cost footer")
	}
	for _, want := range []string{"COST:", "986757 in / 5286 out", "$0 (gateway)", "5 keys", "prices as of 2026-10-01"} {
		if !strings.Contains(line, want) {
			t.Errorf("footer %q missing %q", line, want)
		}
	}

	unmeasured := syncTickCostFooter(TickOutcome{Project: "blog-sync", TokensIn: 10, TokensOut: 1, CostSource: CostSourceGateway})
	if !strings.Contains(unmeasured, "0 keys (unmeasured)") {
		t.Errorf("an unmeasured zero must say WHY: %q", unmeasured)
	}

	if got := syncTickCostFooter(TickOutcome{Project: "blog-qa", MemoryKeys: 9}); got != "" {
		t.Errorf("non-sync lane must have no footer, got %q", got)
	}
	if got := syncTickCostFooter(TickOutcome{Project: "sync", MemoryKeys: 9}); got != "" {
		t.Errorf("a lane merely CONTAINING sync (no -sync suffix) must have no footer, got %q", got)
	}
}

// TestSCHEDGAP1651_DeliveryIncludesFooter proves the delivered message
// itself carries the footer (slot_pool wiring, not just the formatter).
func TestSCHEDGAP1651_DeliveryIncludesFooter(t *testing.T) {
	resetDeliverWarnState(t)
	capture, keepDir := setupFakeHermesB64(t)

	clk := clock.NewFixed(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	var body bytes.Buffer
	body.WriteString("namespace synced, 5 keys verified\n")
	body.WriteString("\n\n" + syncTickCostFooter(TickOutcome{
		Project: "blog-sync", TokensIn: 1200, TokensOut: 40, CostUSD: 0, CostSource: CostSourceGateway, MemoryKeys: 5,
	}))
	deliverOutputWithMode(clk, "blog-sync", "tick-1651", "telegram:12345", "prompt", &body, "full")

	sent, err := os.ReadFile(filepath.Join(keepDir, "body"))
	if err != nil || len(sent) == 0 {
		t.Fatalf("send-time body snapshot missing: %v", err)
	}
	if !strings.Contains(string(sent), "COST: 1200 in / 40 out") {
		t.Errorf("delivered report missing cost footer; got:\n%s", string(sent))
	}
	if !strings.Contains(string(sent), "namespace synced") {
		t.Errorf("send-time body lost the lane's own report; got:\n%s", string(sent))
	}
	_ = capture
}

// curlArgsFixture mirrors the LIVE transcript shape (guard-sync tick
// 2026-10-10): the arguments JSON string escapes newlines and quotes, and
// the header key arrives masked (`x-api-key: ***`) — the detector must
// still count the POST.
const curlArgsFixture = `{\"command\":\"KEY=$(tr -d '[:space:]' < /home/kara/.duckbrain/token-x.token); curl -sm25 -H \\\"x-api-key: $KEY\\\" -o /tmp/wt.json -w '%{http_code}' -X POST \\\"http://localhost:3000/api/memories?namespace=guard\\\" -H 'content-type: application/json' --data-binary @- <<'EOF'\\n{\\\"key\\\":\\\"/sync/write-test\\\",\\\"domain\\\":\\\"raw_note\\\",\\\"content\\\":\\\"x\\\"}\\nEOF\"}`

// TestSCHEDGAP1651_CurlMemoryPostClassification pins the HTTP write rule:
// POSTs to the DuckBrain memories API count; GETs, other hosts and other
// paths never do.
func TestSCHEDGAP1651_CurlMemoryPostClassification(t *testing.T) {
	cases := []struct {
		name string
		args string
		want int
	}{
		{"live POST shape counts", curlArgsFixture, 1},
		{"two POSTs count twice", curlArgsFixture + curlArgsFixture, 2},
		{"GET recall never counts", `{\"command\":\"curl -sm25 -G 'http://localhost:3000/api/memories?prefix=%2Fsync&namespace=guard'\"}`, 0},
		{"memories path on foreign host", `{\"command\":\"curl -X POST 'http://evil.example.com/api/memories'\"}`, 0},
		{"localhost:3000 other path", `{\"command\":\"curl -X POST 'http://localhost:3000/api/v1/groups'\"}`, 0},
		{"duckbrain host other path", `{\"command\":\"curl -X POST 'https://duckbrain.example.com/api/other'\"}`, 0},
		{"scheduler API is not memory", `{\"command\":\"curl -X POST 'http://localhost:9090/api/v1/projects'\"}`, 0},
		{"empty args", ``, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countCurlMemoryPostsInArgs(tc.args); got != tc.want {
				t.Fatalf("countCurlMemoryPostsInArgs = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSCHEDGAP1651_CountsMemoryKeysFromHTTPWrites drives the real transcript
// scan with the live curl-POST shape and asserts the mixed-rule count.
func TestSCHEDGAP1651_CountsMemoryKeysFromHTTPWrites(t *testing.T) {
	tickID := "blog-sync-2026-10-10-18-00-00"
	memoryCallsFixture(t, tickID, []memoryToolCall{
		{name: "terminal", args: curlArgsFixture}, // HTTP write path
		{name: "terminal", args: `{\"command\":\"curl -s -G 'http://localhost:3000/api/memories?prefix=/sync/last-run'\"}`}, // read: not counted
		{name: "mcp__duckbrain__remember", args: `{}`}, // MCP name rule still counts
		{name: "write_file", args: `{}`},               // file tool: never counted
	})
	if got := countMemoryKeysInSession(tickID); got != 2 {
		t.Fatalf("countMemoryKeysInSession = %d, want 2 (curl POST + MCP remember)", got)
	}
}

// TestSCHEDGAP1651_SyncPromptCarriesReadBudget asserts acceptance criterion 2
// structurally: the canonical sync prompt block carries the scope + read
// budget the scheduler appends to every duckbrain-sync tick prompt.
func TestSCHEDGAP1651_SyncPromptCarriesReadBudget(t *testing.T) {
	prompt := SyncScopeBudgetPrompt
	for _, want := range []string{
		"Read only the namespace specified",
		"TOKEN/READ BUDGET",
		"Do not sweep",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("canonical sync scope block missing %q", want)
		}
	}
}

// memoryCallsFixture seeds a fixture state.db whose assistant rows carry
// full name+arguments tool_calls (the shape callsFromAssistantRow parses).
func memoryCallsFixture(t *testing.T, tickID string, calls []memoryToolCall) {
	t.Helper()
	stateDB := filepath.Join(t.TempDir(), "state.db")
	sdb, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("open fixture state db: %v", err)
	}
	defer sdb.Close()
	if _, err := sdb.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, source TEXT NOT NULL, session_key TEXT,
		started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0,
		tool_call_count INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create fixture sessions: %v", err)
	}
	if _, err := sdb.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
		role TEXT NOT NULL, content TEXT, tool_calls TEXT, tool_name TEXT,
		timestamp REAL NOT NULL)`); err != nil {
		t.Fatalf("create fixture messages: %v", err)
	}
	start := float64(time.Now().UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, message_count, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?, ?)`,
		"sess-"+tickID, tickID, start, len(calls), len(calls)); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i, c := range calls {
		raw, err := json.Marshal([]map[string]any{
			{"function": map[string]any{"name": c.name, "arguments": c.args}},
		})
		if err != nil {
			t.Fatalf("marshal call: %v", err)
		}
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_calls, timestamp)
			 VALUES (?, 'assistant', ?, ?)`,
			"sess-"+tickID, string(raw), start+float64(i)); err != nil {
			t.Fatalf("seed assistant message: %v", err)
		}
	}
	t.Setenv(hermesStateDBPathEnv, stateDB)
}

// TestSCHEDGAP1651_PromptAppendIdempotent proves no duplicate block when the
// namespace prompt already carries it (the live duckbrain-sync default_prompt
// has the block baked in; a second append would ship it twice per tick).
func TestSCHEDGAP1651_PromptAppendIdempotent(t *testing.T) {
	withBlock := PackedProject{Name: "blog-sync", NamespacePrompt: "base " + SyncScopeBudgetPrompt}
	if got := buildForemanPrompt(withBlock, "t1"); strings.Count(got, "SCOPE AND READ BUDGET") != 1 {
		t.Errorf("prompt with block already present must carry it exactly once, got %d",
			strings.Count(got, "SCOPE AND READ BUDGET"))
	}
	without := PackedProject{Name: "blog-sync", NamespacePrompt: "plain base"}
	got := buildForemanPrompt(without, "t2")
	if strings.Count(got, "SCOPE AND READ BUDGET") != 1 {
		t.Errorf("plain namespace prompt must get exactly one appended block")
	}
	qa := PackedProject{Name: "blog-qa", NamespacePrompt: "plain base"}
	if strings.Contains(buildForemanPrompt(qa, "t3"), "SCOPE AND READ BUDGET") {
		t.Errorf("non-sync lane must not receive the block")
	}
}
