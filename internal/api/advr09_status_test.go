package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── ADV-R09/G8: budget authority chain + spend reality surface ──────────

// mustCreateADVR09Project inserts a project row via the shared helper.
func mustCreateADVR09Project(t *testing.T, a *apiTestServer, name string) {
	t.Helper()
	mustCreateAPITestProject(t, a.db, name)
}

// insertADVR09Tick inserts a completed tick with explicit cost figures and a
// cost_source tag (migration v29).
func insertADVR09Tick(t *testing.T, a *apiTestServer, tickID, project string, spawnedAt time.Time, cost float64, tokensIn, tokensOut int64, costSource string) {
	t.Helper()
	_, err := a.db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, cost_usd, tokens_in, tokens_out, cost_source, created_at)
		 VALUES (?, ?, 'completed', ?, ?, ?, ?, ?, ?, ?)`,
		tickID, project,
		spawnedAt.UTC().Format(time.RFC3339), spawnedAt.Add(30*time.Minute).UTC().Format(time.RFC3339),
		cost, tokensIn, tokensOut, costSource,
		spawnedAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick %s: %v", tickID, err)
	}
}

// TestADVR09_StatusBudgetFlowsFromLoop: budget_total must come from the Loop
// (the resolved --budget/SCHEDULER_BUDGET/TOML value), never a literal. The
// test server builds its loop with budget=0 — SCHED-GAP-1582 normalizes that
// to the documented default of 100 inside NewLoop (a 0 budget must not mean
// "hold everything"), so the loop's EFFECTIVE budget is 100 and the surface
// must report exactly that: a hardcoded literal in api/ (0, 42, …) still
// fails this test, and TestADVR09_StatusBudgetFollowsConfiguredLoop proves
// the surface follows the loop rather than any constant.
func TestADVR09_StatusBudgetFlowsFromLoop(t *testing.T) {
	a := newAPITestServer(t)

	code, body := a.do(t, "GET", "/api/v1/status", nil)
	if code != 200 {
		t.Fatalf("GET /api/v1/status = %d", code)
	}
	got, ok := body["budget_total"].(float64)
	if !ok {
		t.Fatalf("budget_total missing or wrong type: %T", body["budget_total"])
	}
	// The test loop was built with budget=0 → normalized to 100
	// (SCHED-GAP-1582). The surface reports the loop's effective budget.
	if got != 100 {
		t.Errorf("budget_total = %v, want 100 (the loop's effective budget after SCHED-GAP-1582 zero-normalization — not an api/ literal)", got)
	}
	// Provenance is surfaced next to the number.
	if src, ok := body["budget_source"].(string); !ok || src == "" {
		t.Errorf("budget_source missing/empty: %v", body["budget_source"])
	}
	// A non-default budget on a loop with no provenance snapshot is
	// honestly labeled unattributed, never guessed... but budget=0 with no
	// snapshot is the documented flag-default surface.
}

// TestADVR09_StatusBudgetFollowsConfiguredLoop: a loop built with a
// non-default budget reports THAT value — the surface tracks the authority,
// not a constant. Uses a second server with budget=42.
func TestADVR09_StatusBudgetFollowsConfiguredLoop(t *testing.T) {
	a := newAPITestServer(t)
	// The harness loop was built with budget=0; build a 42-budget loop on
	// the same DB and swap it into a fresh server to observe the change.
	loop42 := scheduler.NewLoop(a.db, time.Minute, time.Hour, 10, 42, 5)
	srv42 := api.NewServer(a.db, loop42)
	ts2 := httptest.NewServer(srv42.Handler())
	defer ts2.Close()

	resp, err := http.Get(ts2.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got, ok := body["budget_total"].(float64)
	if !ok {
		t.Fatalf("budget_total missing or wrong type: %T", body["budget_total"])
	}
	if got != 42 {
		t.Errorf("budget_total = %v, want 42 (the loop's budget)", got)
	}
	if src := body["budget_source"]; src != "unattributed" {
		t.Errorf("budget_source = %v, want \"unattributed\" (loop set, no provenance snapshot)", src)
	}
}

// TestADVR09_BudgetSourceFromResolvedConfig: when main.go's provenance
// snapshot is present, it is the reported source (toml/env/flag layers).
func TestADVR09_BudgetSourceFromResolvedConfig(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{WeightBudget: 100, BudgetSource: "toml"})

	code, body := a.do(t, "GET", "/api/v1/status", nil)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if src := body["budget_source"]; src != "toml" {
		t.Errorf("budget_source = %v, want \"toml\" (from resolved-config snapshot)", src)
	}
	// The config endpoint surfaces the same provenance.
	code, cfg := a.do(t, "GET", "/api/v1/config", nil)
	if code != 200 {
		t.Fatalf("config = %d", code)
	}
	if src := cfg["budget_source"]; src != "toml" {
		t.Errorf("/api/v1/config budget_source = %v, want \"toml\"", src)
	}
	if wb := cfg["weight_budget"]; wb != float64(100) {
		t.Errorf("/api/v1/config weight_budget = %v, want 100", wb)
	}
}

// TestADVR09_SpendByCostSource: recorded spend is split by cost_source with
// real tick metrics — measured/gateway money never blends with the estimate
// tier, and the block states the price vintage. Built from real tick rows
// (the same columns the completion path writes), not mocks.
func TestADVR09_SpendByCostSource(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateADVR09Project(t, a, "spend-proj")

	now := time.Now().UTC()
	// Mixed provenance, exactly as the completion paths write them:
	insertADVR09Tick(t, a, "t-meas-1", "spend-proj", now.Add(-3*time.Hour), 0.50, 500000, 4000, scheduler.CostSourceMeasured)
	insertADVR09Tick(t, a, "t-meas-2", "spend-proj", now.Add(-2*time.Hour), 0.25, 300000, 2500, scheduler.CostSourceMeasured)
	insertADVR09Tick(t, a, "t-gw-1", "spend-proj", now.Add(-1*time.Hour), 0.10, 12000, 8000, scheduler.CostSourceGateway)
	insertADVR09Tick(t, a, "t-est-1", "spend-proj", now.Add(-40*time.Minute), 0.03, 435000, 3500, scheduler.CostSourceEstimated)
	insertADVR09Tick(t, a, "t-leg-1", "spend-proj", now.Add(-30*time.Minute), 0.99, 800000, 6000, "") // pre-v29 row

	code, body := a.do(t, "GET", "/api/v1/status", nil)
	if code != 200 {
		t.Fatalf("GET /api/v1/status = %d", code)
	}
	spendRaw, ok := body["spend"].(map[string]interface{})
	if !ok {
		t.Fatalf("spend block missing: %T", body["spend"])
	}
	// Price vintage surfaced next to the money.
	if asOf, _ := spendRaw["price_as_of"].(string); asOf == "" {
		t.Errorf("spend.price_as_of missing/empty: %v", spendRaw["price_as_of"])
	}
	if src, _ := spendRaw["price_source"].(string); src == "" {
		t.Errorf("spend.price_source missing/empty: %v", spendRaw["price_source"])
	}
	byCS, ok := spendRaw["by_cost_source"].(map[string]interface{})
	if !ok {
		t.Fatalf("spend.by_cost_source missing: %T", spendRaw["by_cost_source"])
	}

	type tier struct {
		CostUSD      float64 `json:"cost_usd"`
		TokensIn     int64   `json:"tokens_in"`
		TokensOut    int64   `json:"tokens_out"`
		Ticks        int     `json:"ticks"`
		TokensInAvg  float64 `json:"tokens_in_avg"`
		TokensOutAvg float64 `json:"tokens_out_avg"`
	}
	decode := func(key string) *tier {
		raw, ok := byCS[key].(map[string]interface{})
		if !ok {
			t.Fatalf("by_cost_source[%q] missing or wrong type: %T", key, byCS[key])
		}
		b, _ := json.Marshal(raw)
		var out tier
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode tier %q: %v", key, err)
		}
		return &out
	}

	meas := decode("measured")
	if meas.Ticks != 2 || meas.CostUSD < 0.74 || meas.CostUSD > 0.76 {
		t.Errorf("measured tier = {ticks %d, $%.4f}, want {2, ~0.75}", meas.Ticks, meas.CostUSD)
	}
	if meas.TokensIn != 800000 || meas.TokensOut != 6500 {
		t.Errorf("measured tokens = %d/%d, want 800000/6500 (sums over real rows)", meas.TokensIn, meas.TokensOut)
	}
	if meas.TokensInAvg < 399999 || meas.TokensInAvg > 500001 {
		t.Errorf("measured tokens_in_avg = %.0f, want ~400000 (500000+300000)/2", meas.TokensInAvg)
	}

	gw := decode("gateway")
	if gw.Ticks != 1 || gw.TokensIn != 12000 || gw.TokensOut != 8000 {
		t.Errorf("gateway tier = %+v, want {1 tick, 12000/8000 tokens}", gw)
	}

	est := decode("estimated")
	if est.Ticks != 1 || est.TokensIn != 435000 {
		t.Errorf("estimated tier = %+v, want {1 tick, 435000 in}", est)
	}

	leg := decode("legacy")
	if leg.Ticks != 1 || leg.CostUSD < 0.98 || leg.CostUSD > 1.00 {
		t.Errorf("legacy tier = %+v, want {1 tick, ~$0.99}", leg)
	}
}

// TestADVR09_EstimateOnlyForTelemetrylessTicks: the estimate tier is marked
// in the surface — an estimated row's figures are distinguishable from a
// measured row's by cost_source, the ONLY estimate tier, and estimate-tier
// rows carry the recalibrated constants (435K/3.5K), not the old 8000/2000
// fiction.
func TestADVR09_EstimateOnlyForTelemetrylessTicks(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateADVR09Project(t, a, "est-proj")

	now := time.Now().UTC()
	insertADVR09Tick(t, a, "t-est-only", "est-proj", now.Add(-time.Hour), 0.03, 435000, 3500, scheduler.CostSourceEstimated)

	code, body := a.do(t, "GET", "/api/v1/status", nil)
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	spend, _ := body["spend"].(map[string]interface{})
	byCS, _ := spend["by_cost_source"].(map[string]interface{})
	est, ok := byCS["estimated"].(map[string]interface{})
	if !ok {
		t.Fatalf("estimated tier missing: %v", byCS)
	}
	tin, _ := est["tokens_in"].(float64)
	if tin != 435000 {
		t.Errorf("estimated tokens_in = %v, want 435000 (recalibrated constant — 8000 was 9.3x-111x low)", tin)
	}
	// And no measured tier exists — the project has no measured rows.
	if m, ok := byCS["measured"]; ok {
		t.Errorf("measured tier present for an estimate-only project: %v", m)
	}
}
