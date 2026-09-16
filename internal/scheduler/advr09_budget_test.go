package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── ADV-R09/G8: price-map as-of, fallback policy, refresh path ──────────

// TestADVR09_PriceMapAsOfIsDeclaredInCode: the builtin sticker map carries a
// real as-of date (YYYY-MM) in CODE, not just a comment — the surfaced value
// must parse and must not be empty. RED against the pre-R09 state (no
// modelRatesAsOf constant / no accessor).
func TestADVR09_PriceMapAsOfIsDeclaredInCode(t *testing.T) {
	asOf := PriceMapAsOf()
	if asOf == "" {
		t.Fatal("PriceMapAsOf() = empty — the builtin stickers declare no as-of date")
	}
	if _, err := time.Parse("2006-01", asOf); err != nil {
		t.Errorf("PriceMapAsOf() = %q, want YYYY-MM: %v", asOf, err)
	}
}

// TestADVR09_UnknownModelFallbackPolicyDocumented: an unpriced model bills at
// the DECLARED fallback rate ($2.00 in / $8.00 out per 1M — pinned literals,
// not read back from the variable, so a silent policy change FAILS here),
// with the policy properties: non-zero and proportional.
func TestADVR09_UnknownModelFallbackPolicyDocumented(t *testing.T) {
	if unknownModelFallbackRate.inPerM <= 0 || unknownModelFallbackRate.outPerM <= 0 {
		t.Fatalf("unknownModelFallbackRate = %+v, want positive documented policy rates", unknownModelFallbackRate)
	}
	// Pin the documented policy VALUE (2026-08 blend). A change here is a
	// pricing decision that must update this test + AGENTS.md, never silent.
	if unknownModelFallbackRate.inPerM != 2.00 || unknownModelFallbackRate.outPerM != 8.00 {
		t.Errorf("unknownModelFallbackRate = %+v, want the documented $2.00/$8.00 per-1M policy", unknownModelFallbackRate)
	}
	// The value the fallback bills 1M/1M at:
	cost := computeCostUSD("", "model-that-does-not-exist", routerRate{}, 1_000_000, 1_000_000)
	const want = 2.00 + 8.00
	if cost < want*0.99 || cost > want*1.01 {
		t.Errorf("unknown-model 1M/1M cost = %.4f, want ~%.4f (the declared fallback policy)", cost, want)
	}
	// Proportional: half the tokens → half the cost.
	half := computeCostUSD("", "model-that-does-not-exist", routerRate{}, 500_000, 500_000)
	if half < want*0.49 || half > want*0.51 {
		t.Errorf("unknown-model 0.5M/0.5M cost = %.4f, want ~%.4f (proportional)", half, want/2)
	}
}

// TestADVR09_ModelRatesFileRefreshPath: the refresh path — a JSON sticker file
// applied over the builtin maps updates pricing WITHOUT a rebuild, adopts the
// file's as_of, and reports file provenance. A file with a negative rate is
// rejected and leaves the builtin maps untouched.
func TestADVR09_ModelRatesFileRefreshPath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "rates.json")
	doc := map[string]any{
		"as_of": "2026-10",
		"models": map[string]any{
			"deepseek-v4-flash": map[string]any{"in_per_m": 0.20, "out_per_m": 0.40},
			"brand-new-model":   map[string]any{"in_per_m": 1.00, "out_per_m": 4.00},
		},
		"providers": map[string]any{
			"opencode-go/mimo-v2.5": map[string]any{"in_per_m": 0.11, "out_per_m": 0.22},
		},
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(good, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyModelRatesFile(good); err != nil {
		t.Fatalf("ApplyModelRatesFile: %v", err)
	}
	t.Cleanup(func() {
		// Restore builtin stickers for other tests in the package.
		priceMapMu.Lock()
		modelRates["deepseek-v4-flash"] = modelRate{0.14, 0.28}
		delete(modelRates, "brand-new-model")
		delete(providerModelRates, "opencode-go/mimo-v2.5")
		appliedRatesAsOf = ""
		priceMapSource = "builtin"
		priceMapMu.Unlock()
	})

	if got := PriceMapAsOf(); got != "2026-10" {
		t.Errorf("PriceMapAsOf() = %q, want %q (file as_of adopted)", got, "2026-10")
	}
	if src := PriceMapSource(); src != "file:"+good {
		t.Errorf("PriceMapSource() = %q, want %q", src, "file:"+good)
	}
	// Refreshed sticker prices the model — a cost computed now reflects the
	// FILE, not the builtin map.
	cost := computeCostUSD("", "brand-new-model", routerRate{}, 1_000_000, 0)
	if cost < 0.99 || cost > 1.01 {
		t.Errorf("brand-new-model 1M in cost = %.4f, want ~1.00 (file sticker)", cost)
	}
	cost = computeCostUSD("", "deepseek-v4-flash", routerRate{}, 1_000_000, 0)
	if cost < 0.19 || cost > 0.21 {
		t.Errorf("deepseek-v4-flash 1M in cost = %.4f, want ~0.20 (file override)", cost)
	}
	// Provider-qualified override wins over the model map.
	cost = computeCostUSD("opencode-go", "mimo-v2.5", routerRate{}, 1_000_000, 0)
	if cost < 0.10 || cost > 0.12 {
		t.Errorf("opencode-go/mimo-v2.5 1M in cost = %.4f, want ~0.11 (file provider override)", cost)
	}

	// Negative rate → rejected, builtin state intact.
	bad := filepath.Join(dir, "bad.json")
	badDoc := `{"as_of": "2026-11", "models": {"x": {"in_per_m": -1, "out_per_m": 1}}}`
	if err := os.WriteFile(bad, []byte(badDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyModelRatesFile(bad); err == nil {
		t.Error("ApplyModelRatesFile(negative rate) = nil error, want rejection")
	}
	if got := PriceMapAsOf(); got != "2026-10" {
		t.Errorf("PriceMapAsOf() after rejected file = %q, want 2026-10 (unchanged)", got)
	}
}

// TestADVR09_FreeLaneStaysFree: a router-known FREE lane (known=true, all
// components 0) still prices at $0 — the fallback policy must never bill a
// free lane at the unknown-model blend.
func TestADVR09_FreeLaneStaysFree(t *testing.T) {
	cost := computeCostUSD("ollama-cloud", "whatever", routerRate{known: true, inPerM: 0, outPerM: 0}, 1_000_000, 1_000_000)
	if cost != 0 {
		t.Errorf("free-lane cost = %.4f, want 0", cost)
	}
}
