package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── SCHED-GAP-078: tick cost_usd must use the router's PUBLIC per-hop
// price (provider-aware), not the hardcoded modelRates map / flat
// fallback. Verified 2026-08-27: P8_SYNC mimo-v2.5 tick (1.28M tokens)
// billed $2.64 via the GPT-5.6-class flat fallback; router public sticker
// prices it at ~$0.18. Bane 2026-08-28: cost reporting quotes the
// providers' PUBLIC list prices, not internal normalized rates.

func f64p(v float64) *float64 { return &v }

// The exact reproduction from the gap: mimo-v2.5 @ opencode-go,
// 1,285,095 input + 8,356 output tokens. Router public sticker:
// $0.14/$0.28 per M → ~$0.18, within 2× of blended usd_1m × tokens.
func TestSCHEDGAP078_MimoTick_PublicPrice(t *testing.T) {
	rr := routerRate{known: true, usd1m: 0.1456, inPerM: 0.14, outPerM: 0.28}
	cost := computeCostUSD("opencode-go", "mimo-v2.5", rr, 1285095, 8356)

	// Exact public computation: 1.285095M×0.14 + 0.008356M×0.28
	wantExact := 1.285095*0.14 + 0.008356*0.28
	if cost < wantExact*0.999 || cost > wantExact*1.001 {
		t.Errorf("cost = %.6f, want ~%.6f (router public in/out)", cost, wantExact)
	}

	// Pass criterion: within 2× of router usd_1m × total tokens.
	wantBlended := 0.1456 * float64(1285095+8356) / 1e6
	if cost < wantBlended/2 || cost > wantBlended*2 {
		t.Errorf("cost = %.6f outside 2× of router blended %.4f", cost, wantBlended)
	}

	// And the whole point: nowhere near the old $2.64 flat-fallback bill.
	if cost > 1.0 {
		t.Errorf("cost = %.6f, want < $1 (old flat fallback billed ~$2.64)", cost)
	}
}

// Same model, different providers → different costs (provider-aware).
func TestSCHEDGAP078_ProviderAware(t *testing.T) {
	opencode := routerRate{known: true, inPerM: 0.14, outPerM: 0.28}
	clinepass := routerRate{known: true, inPerM: 0.44, outPerM: 1.32}
	c1 := computeCostUSD("opencode-go", "deepseek-v4-flash", opencode, 1e6, 1e6)
	c2 := computeCostUSD("clinepass", "deepseek-v4-flash", clinepass, 1e6, 1e6)
	if c1 == c2 {
		t.Errorf("costs must differ by provider lane: opencode=%f clinepass=%f", c1, c2)
	}
	if c2 <= c1 {
		t.Errorf("clinepass lane must cost more than opencode lane: %f vs %f", c2, c1)
	}
}

// Blended-only rate (in/out unknown → -1) uses usd_1m × total tokens.
func TestSCHEDGAP078_BlendedOnly(t *testing.T) {
	rr := routerRate{known: true, usd1m: 0.033, inPerM: -1, outPerM: -1}
	cost := computeCostUSD("ollama-cloud", "deepseek-v4-flash", rr, 1_000_000, 0)
	want := 0.033
	if cost < want*0.999 || cost > want*1.001 {
		t.Errorf("blended cost = %.6f, want %.6f", cost, want)
	}
}

// A FREE lane (public price $0) must compute to $0 — never fall through
// to the static map and bill a free lane at the model's sticker.
func TestSCHEDGAP078_FreeLane(t *testing.T) {
	rr := routerRate{known: true, usd1m: 0, inPerM: 0, outPerM: 0}
	cost := computeCostUSD("zai-glm", "glm-5.3-flash", rr, 1_000_000, 1_000_000)
	if cost != 0 {
		t.Errorf("free lane cost = %f, want 0", cost)
	}
}

// Router unavailable / pair not priced → static map fallback (last resort),
// then flat estimate for truly unknown models.
func TestSCHEDGAP078_MapFallback(t *testing.T) {
	// known=false (no router): deepseek-v4-flash → public map $0.14/$0.28.
	cost := computeCostUSD("deepseek", "deepseek-v4-flash", routerRate{}, 1_000_000, 1_000_000)
	if cost < 0.41 || cost > 0.43 {
		t.Errorf("map fallback cost = %.6f, want ~0.42", cost)
	}
	// Unknown model → flat estimate, still non-zero.
	cost = computeCostUSD("", "nobody-knows-this-model", routerRate{}, 1000, 500)
	want := float64(1000)*estCostPerIn + float64(500)*estCostPerOut
	if cost != want {
		t.Errorf("flat fallback cost = %.6f, want %.6f", cost, want)
	}
}

// RouterResult.HopRate resolves the price for the pair that actually ran
// (head and chain hops), and JSON null prices decode as unknown (-1), not 0.
func TestSCHEDGAP078_HopRate(t *testing.T) {
	var res RouterResult
	err := json.Unmarshal([]byte(`{
		"project": "p8-sync",
		"gate": "OPEN",
		"head": {"hop": 1, "provider": "opencode-go", "model": "mimo-v2.5",
		         "usd_1m": 0.1456, "in_per_m": 0.14, "out_per_m": 0.28},
		"chain": [
			{"hop": 1, "provider": "opencode-go", "model": "mimo-v2.5",
			 "usd_1m": 0.1456, "in_per_m": 0.14, "out_per_m": 0.28},
			{"hop": 2, "provider": "ollama-cloud", "model": "deepseek-v4-flash",
			 "usd_1m": 0.033}
		]
	}`), &res)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rr, ok := res.HopRate("opencode-go", "mimo-v2.5")
	if !ok || !rr.known || rr.inPerM != 0.14 || rr.outPerM != 0.28 || rr.usd1m != 0.1456 {
		t.Errorf("head rate = %+v ok=%v, want public 0.14/0.28 blended 0.1456", rr, ok)
	}

	// Blended-only hop: null in/out → -1 (unknown), usd_1m still known.
	rr, ok = res.HopRate("ollama-cloud", "deepseek-v4-flash")
	if !ok || !rr.known || rr.inPerM != -1 || rr.outPerM != -1 || rr.usd1m != 0.033 {
		t.Errorf("blended hop rate = %+v ok=%v, want usd1m=0.033 in/out=-1", rr, ok)
	}

	// Pair not in the result at all → not known (map fallback path).
	if _, ok := res.HopRate("deepseek-foreman", "deepseek-v4-flash"); ok {
		t.Errorf("HopRate for unlisted pair must be !ok")
	}

	// Nil result is safe.
	if _, ok := (*RouterResult)(nil).HopRate("a", "b"); ok {
		t.Errorf("HopRate on nil result must be !ok")
	}
}

// formatCostSummary carries the provider so log greps can break costs down
// by lane.
func TestSCHEDGAP078_FormatCostSummary(t *testing.T) {
	s := formatCostSummary("opencode-go", "mimo-v2.5", 1285095, 8356, 0.182, 3, 4)
	if !strings.Contains(s, "provider=opencode-go") || !strings.Contains(s, "model=mimo-v2.5") {
		t.Errorf("summary missing provider/model: %s", s)
	}
	if !strings.Contains(s, "cost=$0.1820") {
		t.Errorf("summary missing cost: %s", s)
	}
}
