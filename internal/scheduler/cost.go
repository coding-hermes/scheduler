package scheduler

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"

	// modernc.org/sqlite is the pure-Go driver (no CGO); registered under the
	// driver name "sqlite". Same as internal/database/schema.go.
	_ "modernc.org/sqlite"
)

// Real per-tick cost from Hermes' own telemetry.
//
// The scheduler runs each foreman with an isolated HERMES_HOME (see spawn.go:
// "HERMES_HOME="+s.foremanHome). Hermes writes real token/cost usage to that
// home's state.db (session_model_usage.estimated_cost_usd / actual_cost_usd).
// Instead of the hardcoded $0.032 flat estimate, we look up the real
// estimated_cost_usd for sessions that overlap the tick's [start, finish]
// window. This replaces the "0.032 or 0" dashboard cost with genuine numbers.
//
// The cost lookup is best-effort: on any error (state.db missing, sqlite not
// available, empty result) we return 0 and the caller falls back to the flat
// estimate so aggregation still works.

// sumSessionCostInWindow queries a Hermes state.db for the total estimated
// cost AND the real input/output token totals of sessions whose activity
// window overlaps [start, end]. Tokens come from the same rows the cost does
// (session_model_usage.input_tokens/output_tokens) — before ADV-R09 the exec
// path recorded real USD but wrote 0/0 tokens, hiding the measured per-tick
// usage the estimate should have been calibrated against.
func sumSessionCostInWindow(stateDB string, start, end time.Time, clk clock.Clock) (cost float64, tokensIn, tokensOut int, n int, err error) {
	if stateDB == "" || start.IsZero() {
		return 0, 0, 0, 0, fmt.Errorf("no state db / start time")
	}
	if _, serr := os.Stat(stateDB); serr != nil {
		return 0, 0, 0, 0, fmt.Errorf("state db %s: %w", stateDB, serr)
	}
	if end.IsZero() {
		end = clk.Now()
	}

	// session_model_usage.first_seen / last_seen are unix-epoch floats.
	// Match sessions whose activity overlaps the tick window:
	//   first_seen <= end  AND  last_seen >= start
	db, oerr := sql.Open("sqlite", stateDB)
	if oerr != nil {
		return 0, 0, 0, 0, oerr
	}
	defer db.Close()

	// Prefer real actual cost when present (actual_cost_usd > 0); otherwise
	// fall back to estimated. Sum over overlapping sessions.
	q := `
SELECT COALESCE(SUM(CASE WHEN actual_cost_usd > 0 THEN actual_cost_usd ELSE estimated_cost_usd END), 0),
       COALESCE(SUM(input_tokens), 0),
       COALESCE(SUM(output_tokens), 0),
       COUNT(*)
  FROM session_model_usage
 WHERE first_seen <= ? AND last_seen >= ?`
	err = db.QueryRow(q, end.Unix(), start.Unix()).Scan(&cost, &tokensIn, &tokensOut, &n)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return cost, tokensIn, tokensOut, n, nil
}

// sumSessionMeteredUSDInWindow is the budget-only cash meter. Unlike
// sumSessionCostInWindow (which prefers actual over estimated for one tick),
// the SCHED-GAP-127 budget contract explicitly sums both columns for every
// fleet session overlapping [start,end]. Keeping this query separate preserves
// the existing per-tick telemetry semantics while making the opt-in budget mode
// auditable against Hermes state.db directly.
func sumSessionMeteredUSDInWindow(ctx context.Context, stateDB string, start, end time.Time) (float64, error) {
	if stateDB == "" || start.IsZero() || end.IsZero() {
		return 0, fmt.Errorf("metered budget: no state db / window")
	}
	if _, err := os.Stat(stateDB); err != nil {
		return 0, fmt.Errorf("metered budget state db %s: %w", stateDB, err)
	}
	db, err := sql.Open("sqlite", stateDB)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var cost float64
	err = db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(COALESCE(estimated_cost_usd, 0) + COALESCE(actual_cost_usd, 0)), 0)
  FROM session_model_usage
 WHERE first_seen <= ? AND last_seen >= ?`, end.Unix(), start.Unix()).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost, nil
}

// resolveRealTickCost returns the real cost AND real token totals of a tick,
// falling back to the flat estimate when real telemetry is unavailable. It
// sums:
//   - foreman + worker sessions in the foreman's Hermes state.db overlapping
//     the tick window (the dominant cost), and
//   - GitReins judge usage recorded in <workdir>/.gitreins/usage.jsonl within
//     the same window (gitreins uses its own LLM client, so it never appears
//     in Hermes telemetry).
//
// Returns (cost, tokensIn, tokensOut, isReal). Token totals cover the Hermes
// telemetry rows only (the judge usage.jsonl rows carry tokens too but are
// priced inline; folding them in would double-report usage across surfaces).
func resolveRealTickCost(foremanHome, workdir, project string, start, end time.Time, clk clock.Clock) (float64, int, int, bool) {
	total := 0.0
	tin, tout := 0, 0
	real := false

	// 1) Foreman + worker cost + tokens from Hermes telemetry.
	stateDB := filepath.Join(foremanHome, "state.db")
	if sessionCost, sTin, sTout, n, err := sumSessionCostInWindow(stateDB, start.Add(-2*time.Minute), end, clk); err == nil {
		total += sessionCost
		tin += sTin
		tout += sTout
		if n > 0 && sessionCost > 0 {
			real = true
		}
	}

	// 2) GitReins judge cost from usage.jsonl (per-project).
	if workdir != "" {
		if jCost, n, err := sumGitreinsUsageInWindow(filepath.Join(workdir, ".gitreins", "usage.jsonl"), start, end); err == nil {
			total += jCost
			if n > 0 && jCost > 0 {
				real = true
			}
		}
	}

	if !real || total <= 0 {
		_, _, est := estimateTickCost()
		return est, 0, 0, false
	}
	return total, tin, tout, true
}

// subIncludedLaneProviders is an explicit pricing-policy allowlist. Membership
// means the lane is prepaid/subscription-backed and therefore has $0 marginal
// model cost when telemetry is absent. This is intentionally code-owned rather
// than inferred from model names or router prices: adding a provider here is a
// human-reviewed pricing decision.
var subIncludedLaneProviders = map[string]string{
	"zai-glm":         "Z.AI coding-plan subscription",
	"kimi-for-coding": "Kimi fixed-price coding subscription",
	"ollama-cloud":    "Ollama Cloud prepaid Max subscription",
	"opencode-go":     "OpenCode Go subscription seat",
	"commandcode":     "CommandCode subscription seat",
	"xkiro":           "xKiro plan allowance",
	"neuralwatt":      "NeuralWatt prepaid plan credits",
	"synthetic":       "Synthetic subscription allowance",
	"openai-codex":    "OpenAI Codex OAuth subscription seat",
}

// laneMarginalUSD returns the estimated-tick marginal USD for a resolved lane.
// Sub-included providers are $0 regardless of model; every other provider gets
// the model/provider's public listed-price sticker. Model identity alone never
// makes a lane free: the same model routed through a PAYG provider still bills.
func laneMarginalUSD(model, provider string) float64 {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if _, included := subIncludedLaneProviders[provider]; included {
		return 0
	}
	return stickerCostUSD(provider, model, estTokensIn, estTokensOut)
}

type resolvedTickCost struct {
	costUSD    float64
	stickerUSD float64
	tokensIn   int
	tokensOut  int
	source     string
}

// resolveTickCost enforces the SCHED-GAP-127 provenance order: real telemetry
// wins first; only when it is absent does lane policy apply. The listed-price
// sticker is kept separately from the marginal truth so a sub-included lane can
// report a real $0 without erasing what the same tokens list for publicly.
func resolveTickCost(foremanHome, workdir, project, provider, model string, rate routerRate, start, end time.Time, clk clock.Clock) resolvedTickCost {
	cost, tin, tout, isReal := resolveRealTickCost(foremanHome, workdir, project, start, end, clk)
	if isReal {
		return resolvedTickCost{
			costUSD:    cost,
			stickerUSD: stickerCostUSD(provider, model, tin, tout),
			tokensIn:   tin,
			tokensOut:  tout,
			source:     CostSourceMeasured,
		}
	}

	tin, tout, _ = estimateTickCost()
	sticker := computeCostUSD(provider, model, rate, tin, tout)
	if laneMarginalUSD(model, provider) == 0 {
		return resolvedTickCost{
			costUSD:    0,
			stickerUSD: stickerCostUSD(provider, model, tin, tout),
			tokensIn:   tin,
			tokensOut:  tout,
			source:     CostSourceEstimatedSubincluded,
		}
	}
	return resolvedTickCost{
		costUSD:    sticker,
		stickerUSD: sticker,
		tokensIn:   tin,
		tokensOut:  tout,
		source:     CostSourceEstimated,
	}
}

// sumGitreinsUsageInWindow reads .gitreins/usage.jsonl lines whose ts falls in
// [start, end] and converts token counts to USD using the same per-token rates
// as the foreman estimate. Returns (costUSD, lineCount).
func sumGitreinsUsageInWindow(path string, start, end time.Time) (float64, int, error) {
	if path == "" {
		return 0, 0, fmt.Errorf("no gitreins usage path")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	total := 0.0
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Ts        float64 `json:"ts"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		t := time.Unix(int64(rec.Ts), 0)
		if t.Before(start) || t.After(end) {
			continue
		}
		total += float64(rec.TokensIn)*estCostPerIn + float64(rec.TokensOut)*estCostPerOut
		count++
	}
	return total, count, sc.Err()
}

// ── S12 §11 cost attribution (SCHED-GAP-115): per-worker wave split ──────
//
// DECISION 2 / invariant W4: ticks.cost_usd is the tick's SINGLE money
// figure and ALREADY contains the wave's model spend — resolveRealTickCost
// sums every session in the foreman's HERMES_HOME state.db overlapping the
// tick window, worker sessions included, so adding tick_workers.cost_usd on
// top would double-count. Per-worker cost is therefore ATTRIBUTION ONLY: it
// splits the already-counted tick cost, never adds to it (spec §16 row 7:
// "non-additive for model cost").
//
// The ONE additive piece is worktree-side GitReins judge cost: a worktree
// created by `git worktree add` does not receive gitignored files, so each
// worker's judge usage lands in <worktree>/.gitreins/usage.jsonl — a path
// resolveRealTickCost never reads (it reads only the MAIN workdir's copy).
// That money is invisible today; rolling it into ticks.cost_usd is NEW
// money, not a double count (spec §5.1: "judge cost from a worktree is new
// money and should be rolled in explicitly once row 7 lands").
//
// Estimated vs realized per worker: the manifest's per-worker cost_usd is
// the foreman's REALIZED figure when > 0 (its worker sessions report token
// usage through the same gateway telemetry the foreman itself uses). When a
// worker reports 0 — foreman never filled it, telemetry gap — the row gets
// the tick's per-worker AVERAGE estimate (tick cost ÷ worker_count) as a
// manifest-side estimate placeholder, marked estimated. Realized wins per
// worker; a 0-cost row never drags the attribution split below zero.

// estimateWorkerCost splits the tick's single cost figure evenly across its
// dispatched workers — the fallback attribution when the manifest carries
// no per-worker figure. workerCount <= 0 (serial tick) or a non-positive
// cost yields 0: there is nothing to split.
func estimateWorkerCost(tickCost float64, workerCount int) float64 {
	if workerCount <= 0 || tickCost <= 0 {
		return 0
	}
	return tickCost / float64(workerCount)
}

// attributedWorkerCost is one worker's resolved attribution figure.
type attributedWorkerCost struct {
	TaskID   string
	Cost     float64 // USD
	Realized bool    // true = the manifest's own figure; false = estimated split
}

// attributeWaveCost resolves the per-worker attribution set for a
// just-completed wave tick (SCHED-GAP-115). Inputs:
//
//   - workers: the manifest's tick_workers rows (already ingested by
//     SCHED-GAP-110; task_id + the foreman's per-worker cost_usd);
//   - tickCost: the tick's single cost figure (ticks.cost_usd) — already
//     contains the wave's model spend (W4), never re-added;
//   - worktreeJudgeCost: NEW money from worktree-side GitReins judge usage
//     (sumGitreinsUsageInWindow over each worker's worktree path), additive
//     to the TICK total but attributed per worker by the same split.
//
// Semantics: a worker with manifest cost_usd > 0 keeps it (realized wins);
// a worker with 0 gets the even split of (tickCost + worktreeJudgeCost) as
// an estimate — divided over the DEDUPED worker set, so a duplicated task
// id never dilutes the split. The returned set is per-worker attribution —
// callers may sum it for a display split, but MUST NOT add it to
// ticks.cost_usd or any fleet total (W4). No worker is counted twice:
// exactly one output entry per unique task_id (first wins — the same
// no-double-count discipline as manifest ingest).
func attributeWaveCost(workers []database.TickWorker, tickCost, worktreeJudgeCost float64) []attributedWorkerCost {
	if len(workers) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(workers))
	out := make([]attributedWorkerCost, 0, len(workers))
	for _, w := range workers {
		if seen[w.TaskID] {
			continue // no double count for a duplicated task_id
		}
		seen[w.TaskID] = true
		ac := attributedWorkerCost{TaskID: w.TaskID}
		if w.CostUSD > 0 {
			ac.Cost = w.CostUSD
			ac.Realized = true
		}
		out = append(out, ac)
	}
	// The estimate divisor is the DEDUPED count: fill only the rows that
	// still need a figure.
	est := estimateWorkerCost(tickCost+worktreeJudgeCost, len(out))
	for i := range out {
		if !out[i].Realized {
			out[i].Cost = est
		}
	}
	return out
}

// sumWorktreeJudgeCost rolls the wave's worktree-side GitReins judge usage
// into the tick (the additive half of S12 §16 row 7). A worker worktree is
// a fresh checkout: gitignored .gitreins/usage.jsonl exists THERE but not
// in the main workdir's copy — resolveRealTickCost never sees it. Cost is
// converted with the same PUBLIC per-token rates as the serial judge path
// (sumGitreinsUsageInWindow). Best-effort: unreadable paths contribute 0.
func sumWorktreeJudgeCost(workers []database.TickWorker, start, end time.Time) float64 {
	total := 0.0
	for _, w := range workers {
		if w.Worktree == "" {
			continue
		}
		c, _, err := sumGitreinsUsageInWindow(filepath.Join(w.Worktree, ".gitreins", "usage.jsonl"), start, end)
		if err != nil {
			continue // no usage file in this worktree — normal for a clean worker
		}
		total += c
	}
	return total
}

// attributeTickWorkers is the completion-path entry point: on the SAME hook
// SCHED-GAP-110 used for manifest ingest (slot_pool.go, after
// lifecycle.Complete, completed/timeout ticks), it resolves each
// tick_workers row's attribution figure and persists it, then rolls the
// worktree-side judge cost — the only ADDITIVE piece — into ticks.cost_usd.
//
// Contract (mirrors ingest's fail-safe doctrine, §9.3):
//   - no worker rows (serial tick / no manifest) → (0, nil): the serial
//     path is untouched, byte-identical to pre-115 behavior;
//   - any error inside → logged + one WARN event, attribution simply does
//     not happen for that tick — it can NEVER fail the completion path;
//   - W4: tick_workers.cost_usd is written ONLY from the resolved
//     attribution (realized manifest figure wins; else the even split);
//     ticks.cost_usd gains ONLY worktreeJudgeCost (new money).
//
// Returns the worktree judge cost rolled into the tick.
func attributeTickWorkers(ctx context.Context, db *sql.DB, tickID string, tickStart, tickEnd time.Time) (float64, error) {
	workers, err := database.ListTickWorkersByTick(ctx, db, tickID)
	if err != nil {
		return 0, fmt.Errorf("wave attribution %s: list workers: %w", tickID, err)
	}
	if len(workers) == 0 {
		return 0, nil // serial tick — nothing to attribute
	}

	var tickCost float64
	if err := db.QueryRowContext(ctx,
		`SELECT cost_usd FROM ticks WHERE id = ?`, tickID).Scan(&tickCost); err != nil {
		return 0, fmt.Errorf("wave attribution %s: read tick cost: %w", tickID, err)
	}

	judgeCost := sumWorktreeJudgeCost(workers, tickStart, tickEnd)
	attrs := attributeWaveCost(workers, tickCost, judgeCost)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("wave attribution %s: begin tx: %w", tickID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op on commit

	for _, a := range attrs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tick_workers SET cost_usd = ?, updated_at = ? WHERE tick_id = ? AND task_id = ?`,
			a.Cost, nowRFC3339(clock.FromContext(ctx)), tickID, a.TaskID); err != nil {
			return 0, fmt.Errorf("wave attribution %s: update worker %q: %w", tickID, a.TaskID, err)
		}
	}
	if judgeCost > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE ticks SET cost_usd = cost_usd + ? WHERE id = ?`, judgeCost, tickID); err != nil {
			return 0, fmt.Errorf("wave attribution %s: roll in judge cost: %w", tickID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("wave attribution %s: commit: %w", tickID, err)
	}

	var realized, estimated int
	for _, a := range attrs {
		if a.Realized {
			realized++
		} else {
			estimated++
		}
	}
	log.Printf("WAVE-COST: %s tick=%s workers=%d realized=%d estimated=%d worktree_judge_usd=%.4f",
		workerProject(tickID), tickID, len(attrs), realized, estimated, judgeCost)
	return judgeCost, nil
}

// nowRFC3339 is the shared UTC timestamp stamp used by the attribution
// updates (same format as database.nowUTC / lifecycle.Complete). The clock is
// injected (SCHED-GAP-169) so attribution stamps follow the run's clock.
func nowRFC3339(clk clock.Clock) string {
	return clk.Now().UTC().Format(time.RFC3339)
}

// workerProject derives the project name from a tick id for log lines
// (tick ids are "<project>-<timestamp>" per NextTickID). Falls back to the
// full id.
func workerProject(tickID string) string {
	if i := strings.LastIndex(tickID, "-20"); i > 0 {
		return tickID[:i]
	}
	return tickID
}
