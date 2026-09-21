package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// modelRate is the per-million-token USD price for a model.
type modelRate struct {
	inPerM  float64 // input $/1M tokens
	outPerM float64 // output $/1M tokens
}

// modelRatesAsOf is the AS-OF date of the builtin modelRates stickers
// (ADV-R09/G8): the month the prices were verified against models.dev /
// provider pricing pages. A sticker refresh changes this constant together
// with the map (or, better, ships a rates file via ApplyModelRatesFile so no
// rebuild is needed). Surfaced through PriceMapAsOf() so every cost figure
// carries its price vintage.
const modelRatesAsOf = "2026-08"

// unknownModelFallbackRate is the DOCUMENTED pricing policy for a model that
// is in NEITHER the task router, providerModelRates, nor modelRates
// (ADV-R09/G8): a flat $2.00/$8.00 per 1M in/out blend — roughly the fleet's
// mid-tier lane mix — so an unknown sticker still yields a non-zero,
// proportional cost instead of $0. This is a POLICY value, not a
// measurement; changing it is a pricing decision, not a bug fix.
var unknownModelFallbackRate = modelRate{inPerM: 2.00, outPerM: 8.00}

// modelRates holds PUBLIC list pricing for models the coding-hermes fleet
// actually uses, keyed by model name — the LAST-RESORT fallback for cost
// reporting (SCHED-GAP-078). Prices are public per-1M-token USD rates as of
// modelRatesAsOf. The task router's per-hop public price is the PRIMARY
// source (provider-aware, tick-accurate); this map only fires when the router
// was unavailable or didn't price the pair. providerModelRates overrides by
// "provider/model" when a provider's public rate differs materially from the
// model-wide default. Both maps can be refreshed without a rebuild via
// ApplyModelRatesFile (SCHEDULER_MODEL_RATES_FILE / --model-rates-file).
var modelRates = map[string]modelRate{
	"deepseek-v4-flash": {0.14, 0.28},
	"deepseek-v4-pro":   {0.27, 1.10},
	"glm-5.2":           {0.15, 0.60},
	"kimi-k3":           {0.60, 2.50},
	"minimax-m3":        {1.00, 3.00},
	"gemma":             {0.10, 0.10},
	"mimo-v2.5":         {0.14, 0.28}, // models.dev sticker (opencode-go lane)
	"gpt-5.6-luna":      {2.50, 10.00},
	"gpt-5.6-sol":       {2.50, 10.00},
	"gpt-5.6-terra":     {2.50, 10.00},
	"grok-4.5":          {2.00, 10.00},
	"step-3.7-flash":    {0.20, 0.80},
}

// providerModelRates keys are "provider/model" — a provider-specific PUBLIC
// rate that differs from the model-wide default (e.g. a reseller lane whose
// sticker is not the underlying provider's list).
var providerModelRates = map[string]modelRate{}

// priceMapMu guards the price maps + their metadata. computeCostUSD is called
// from every completion goroutine; ApplyModelRatesFile swaps the maps wholesale
// under the write lock (startup or test), so readers never see a torn map.
var priceMapMu sync.RWMutex

// priceMapSource records where the ACTIVE stickers came from: "builtin", or
// "file:<path>" after a successful ApplyModelRatesFile. Surfaced next to every
// spend figure so an operator can tell compiled-in stickers from a refreshed
// file (ADV-R09/G8).
var priceMapSource = "builtin"

// PriceMapAsOf returns the as-of date of the ACTIVE price stickers — the
// builtin modelRatesAsOf, or the file's as_of after a rates-file refresh.
func PriceMapAsOf() string {
	priceMapMu.RLock()
	defer priceMapMu.RUnlock()
	if appliedRatesAsOf != "" {
		return appliedRatesAsOf
	}
	return modelRatesAsOf
}

// PriceMapSource returns the provenance of the ACTIVE price stickers
// ("builtin" or "file:<path>").
func PriceMapSource() string {
	priceMapMu.RLock()
	defer priceMapMu.RUnlock()
	return priceMapSource
}

// appliedRatesAsOf is the as_of carried by the last successfully applied
// rates file (empty = builtin vintage).
var appliedRatesAsOf string

// modelRatesDoc is the JSON shape accepted by ApplyModelRatesFile.
type modelRatesDoc struct {
	AsOf      string                   `json:"as_of"`
	Models    map[string]modelRateJSON `json:"models"`
	Providers map[string]modelRateJSON `json:"providers"`
}

// modelRateJSON is the wire form of one rate entry.
type modelRateJSON struct {
	InPerM  float64 `json:"in_per_m"`
	OutPerM float64 `json:"out_per_m"`
}

// ApplyModelRatesFile loads a JSON price-sticker file and merges it OVER the
// builtin maps (per-key override; keys not present keep the builtin sticker),
// adopting the file's as_of date. This is the refresh path (ADV-R09/G8):
// stickers can update without a rebuild via --model-rates-file /
// SCHEDULER_MODEL_RATES_FILE. Negative rates are rejected; a rejected file
// leaves the builtin maps untouched.
func ApplyModelRatesFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("model rates file: %w", err)
	}
	var doc modelRatesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("model rates file %s: %w", path, err)
	}
	models := make(map[string]modelRate, len(doc.Models))
	for name, r := range doc.Models {
		if r.InPerM < 0 || r.OutPerM < 0 {
			return fmt.Errorf("model rates file %s: negative rate for model %q", path, name)
		}
		models[name] = modelRate{inPerM: r.InPerM, outPerM: r.OutPerM}
	}
	providers := make(map[string]modelRate, len(doc.Providers))
	for name, r := range doc.Providers {
		if r.InPerM < 0 || r.OutPerM < 0 {
			return fmt.Errorf("model rates file %s: negative rate for provider lane %q", path, name)
		}
		providers[name] = modelRate{inPerM: r.InPerM, outPerM: r.OutPerM}
	}

	priceMapMu.Lock()
	defer priceMapMu.Unlock()
	for name, r := range models {
		modelRates[name] = r
	}
	for name, r := range providers {
		providerModelRates[name] = r
	}
	appliedRatesAsOf = doc.AsOf
	priceMapSource = "file:" + path
	return nil
}

// computeCostUSD returns the estimated PUBLIC cost in USD for a tick, in
// priority order (SCHED-GAP-078, Bane 2026-08-28):
//
//  1. the task router's public in/out per-1M rates for the resolved
//     (provider, model) lane — provider-aware and tick-accurate;
//  2. the router's blended public usd_1m for the lane;
//  3. the provider-qualified static map, then the model map — last resort
//     when the router did not price the pair;
//  4. the fixed flat per-token estimates so aggregation is never zero.
//
// A routerRate with known=true but all components 0.0 is a FREE lane — cost
// legitimately computes to 0 (never fall through to the maps for a free
// lane: that would bill a $0 lane at the model's sticker).
func computeCostUSD(provider, model string, rr routerRate, tokensIn, tokensOut int) float64 {
	if rr.known {
		if rr.inPerM >= 0 && rr.outPerM >= 0 {
			return float64(tokensIn)/1e6*rr.inPerM + float64(tokensOut)/1e6*rr.outPerM
		}
		if rr.usd1m >= 0 {
			return rr.usd1m * float64(tokensIn+tokensOut) / 1e6
		}
	}
	return stickerCostUSD(provider, model, tokensIn, tokensOut)
}

// stickerCostUSD returns the public listed-price figure without applying lane
// subscription policy. SCHED-GAP-127 deliberately keeps the sticker DERIVED
// from persisted token totals plus the price map instead of adding another
// ticks column: this avoids a second migration/write path that could drift from
// the price_as_of and price_source metadata already carried by /api/v1/status.
// ticks.cost_usd remains the metered/marginal truth used by budget gates, while
// this helper is the one listed-price calculation used by laneMarginalUSD.
func stickerCostUSD(provider, model string, tokensIn, tokensOut int) float64 {
	priceMapMu.RLock()
	defer priceMapMu.RUnlock()
	if provider != "" {
		if rate, ok := providerModelRates[provider+"/"+model]; ok {
			return float64(tokensIn)/1e6*rate.inPerM + float64(tokensOut)/1e6*rate.outPerM
		}
	}
	if rate, ok := modelRates[model]; ok {
		return float64(tokensIn)/1e6*rate.inPerM + float64(tokensOut)/1e6*rate.outPerM
	}
	// Unknown model — the documented fallback policy (ADV-R09/G8):
	// unknownModelFallbackRate, a flat mid-tier blend per 1M tokens so an
	// unpriced model still yields a non-zero, proportional cost. The old
	// behavior billed unknown models at the per-token estCostPer* constants
	// with no declared policy.
	return float64(tokensIn)/1e6*unknownModelFallbackRate.inPerM +
		float64(tokensOut)/1e6*unknownModelFallbackRate.outPerM
}

// gitCommitCountInWindow returns the number of commits in workdir between [since, until].
// Returns 0 for repos without .git or on any git error — it must never block
// the tick completion path. (Named distinctly from gitmetrics.gitCommitCount —
// the spawn-time baseline counter — after the upstream merge.)
func gitCommitCountInWindow(ctx context.Context, workdir, since, until string) int {
	out, err := runGit(ctx, workdir, "rev-list", "--count",
		"--since="+since, "--until="+until, "HEAD")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

// gitFilesChanged returns the number of unique files changed by commits in
// workdir between [since, until]. It diffs the first commit's parent in the
// window against the last commit. Returns 0 for repos without .git, windows
// with zero commits, or any git error.
func gitFilesChanged(ctx context.Context, workdir, since, until string) int {
	// List commits in the window (oldest first).
	oldest := runGitFirst(ctx, workdir, "rev-list",
		"--reverse", "--since="+since, "--until="+until, "HEAD")
	if oldest == "" {
		return 0 // zero commits in window
	}
	newest := runGitFirst(ctx, workdir, "rev-list",
		"--since="+since, "--until="+until, "HEAD")
	if newest == "" || newest == oldest {
		// Single commit: diff against its parent.
		out, err := runGit(ctx, workdir, "diff-tree", "--no-commit-id",
			"--name-only", "-r", oldest)
		if err != nil {
			return 0
		}
		return countUniqueNonEmpty(out)
	}
	// Multiple commits: diff oldest's parent against newest.
	parentOfOldest := runGitFirst(ctx, workdir, "rev-parse", oldest+"^")
	if parentOfOldest == "" {
		parentOfOldest = oldest
	}
	out, err := runGit(ctx, workdir, "diff", "--name-only", parentOfOldest, newest)
	if err != nil {
		return 0
	}
	return countUniqueNonEmpty(out)
}

// runGit executes a git command in workdir and returns stdout as a string.
// Returns "" + error on any failure.
func runGit(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil // suppress — errors are expected for non-repo dirs
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// runGitFirst returns the first line of a git command's stdout, or "" on error.
func runGitFirst(ctx context.Context, workdir string, args ...string) string {
	out, err := runGit(ctx, workdir, args...)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

// countUniqueNonEmpty counts unique non-empty lines in a newline-separated string.
func countUniqueNonEmpty(s string) int {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		seen[line] = struct{}{}
	}
	return len(seen)
}

// countGitChanges runs git commit + file counting over the [start, end] window
// in workdir. Returns (commits, filesChanged). Never panics or blocks — all git
// errors produce (0, 0).
func countGitChanges(workdir string, start, end time.Time) (int, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	since := start.Format(time.RFC3339)
	until := end.Format(time.RFC3339)
	commits := gitCommitCountInWindow(ctx, workdir, since, until)
	files := gitFilesChanged(ctx, workdir, since, until)
	return commits, files
}

// formatCostSummary returns a one-line log-friendly summary string.
func formatCostSummary(provider, model string, tin, tout int, cost float64, commits, files int) string {
	return fmt.Sprintf("provider=%s model=%s tokens=%d/%d cost=$%.4f commits=%d files=%d",
		provider, model, tin, tout, cost, commits, files)
}
