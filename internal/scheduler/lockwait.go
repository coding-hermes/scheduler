package scheduler

import (
	"sync/atomic"
	"time"
)

// PERF-001-R2 — live lock-contention attribution for the /api/v1/status read
// path.
//
// What the row knows: the status handler is DB-bound on CPU (computeProjectFailureRates
// 57.9% of the window) but spends ~86% of its wall time NOT computing, and the
// goroutine dump taken at the stall showed evaluate() holding Loop.mu. What the
// row asked for: an A/B that names WHICH side the wait lives on, instead of a
// guessed fix.
//
// The read path's ONLY Loop.mu site is ZeroSelectStats() (GAP-043, the
// zero-select diagnostics block the status handler renders), and evaluate()
// holds the WRITE lock across its whole Phase 1. Those two facts make the
// convoy measurable with two tiny atomic observers, live, with no profiler and
// no test seam:
//
//   - statusReadLock — how long an RLock acquire on the status read path
//     actually waited (nanoseconds). Recorded at ZeroSelectStats().
//   - evalHold — how long each evaluate() pass HELD the write lock. Recorded
//     at every evaluate() exit.
//
// A/B reading: sample /api/v1/status while EvalInFlight() is false (idle) and
// again while it is true (a pass is holding the lock). Idle ⇒ statusReadLock
// samples are ~0 and the endpoint answers in its CPU+DB time; contended ⇒ the
// blocked samples track the remaining hold. If BOTH states read ~0 lock wait,
// the wait is NOT this lock and the armed block profile
// (GET /debug/pprof/block, see blockprofile.go) names the real blocker.
//
// Measurement clock: these observers use the REAL wall clock (time.Now /
// time.Since), never the loop's clock seam. The seam is the scheduler's
// decision instant (ADV-R04/G6) — a simulator clock would make every hold
// read as 0 and hide the contention this file exists to expose.

// LockWaitStats is a process-lifetime summary of one read-lock's contention.
// Counters are monotonic and reset only by a restart, matching the other
// process-scoped counters on the Loop (spawn/gateway/admission).
type LockWaitStats struct {
	// Samples is the number of acquires observed.
	Samples int64 `json:"samples"`
	// Blocked is the subset of samples that had to wait at all (> 0ns).
	Blocked int64 `json:"blocked"`
	// MaxNs / LastNs / MeanNs are the worst, the most recent and the average
	// wait, in nanoseconds.
	MaxNs  int64 `json:"max_ns"`
	LastNs int64 `json:"last_ns"`
	MeanNs int64 `json:"mean_ns"`
	// TotalNs is the summed wait, so a consumer can re-derive the mean and
	// see whether the worst case is a spike or the steady state.
	TotalNs int64 `json:"total_ns"`
}

// HoldStats is a process-lifetime summary of how long a write lock was held.
type HoldStats struct {
	// Samples is the number of completed holds observed.
	Samples int64 `json:"samples"`
	// MaxNs / LastNs / MeanNs are the longest, the most recent and the
	// average hold, in nanoseconds.
	MaxNs  int64 `json:"max_ns"`
	LastNs int64 `json:"last_ns"`
	MeanNs int64 `json:"mean_ns"`
	// TotalNs is the summed hold time.
	TotalNs int64 `json:"total_ns"`
}

// LockAttribution is the JSON-ready attribution block the API serves. Every
// number is measured, never derived: a zero here means "never observed", and
// the field set is deliberately small enough that an operator reads it in one
// glance next to the endpoint's p50.
type LockAttribution struct {
	// StatusReadLock is the status read path's Loop.mu wait (the
	// ZeroSelectStats RLock).
	StatusReadLock LockWaitStats `json:"status_read_lock"`
	// EvalHold is how long evaluate() held the write lock, per pass.
	EvalHold HoldStats `json:"eval_hold"`
	// EvalInFlight is true while a pass holds the write lock right now —
	// the state a sampler tests to split its idle arm from its contended arm.
	EvalInFlight bool `json:"eval_in_flight"`
	// EvalHoldSite names the deadline the pass is bounded by, so the
	// attribution travels with the invariant it belongs to.
	EvalHoldSite string `json:"eval_hold_site"`
	// Stages decomposes EvalHold into evaluate()'s Phase-1 stages: the
	// sub-call attribution the row asks for. Stages a pass shape never
	// reaches are omitted.
	Stages []EvalStageStats `json:"stages,omitempty"`
}

// BlockProfileState reports the armed block profile (PERF-001-R2). It is
// process-scoped, not per-pass: the rate is resolved once, at the head of the
// first evaluate() that runs.
type BlockProfileState struct {
	Armed bool   `json:"armed"`
	Rate  int    `json:"rate"`
	Env   string `json:"env"`
}

// evalHoldSiteStatusReadLock is the documented site the status-lock observer
// is wired to. It is served alongside the measurement so an operator (or a
// later change) can see what the number means without reading this file.
const evalHoldSiteStatusReadLock = "Loop.ZeroSelectStats RLock (/api/v1/status zero-select block)"

// durationObs is the atomic accumulator behind both exported summaries. It is
// NOT a histogram: the row needs max/last/mean and a blocked count, and a
// fixed-bucket histogram would cost more than the thing it measures.
// Deliberately lock-free — an observer that takes a lock to measure a lock
// measures itself.
type durationObs struct {
	samples atomic.Int64
	nonzero atomic.Int64
	maxNs   atomic.Int64
	lastNs  atomic.Int64
	totalNs atomic.Int64
}

// observe records one completed wait/hold. Negative durations (a clock
// adjustment) clamp to 0 rather than corrupting the totals.
func (o *durationObs) observe(d time.Duration) {
	if o == nil {
		return
	}
	ns := int64(d)
	if ns < 0 {
		ns = 0
	}
	o.samples.Add(1)
	if ns > 0 {
		o.nonzero.Add(1)
	}
	o.lastNs.Store(ns)
	o.totalNs.Add(ns)
	for {
		cur := o.maxNs.Load()
		if ns <= cur || o.maxNs.CompareAndSwap(cur, ns) {
			return
		}
	}
}

// snapshot returns (samples, nonzero, max, last, total) and the derived mean.
func (o *durationObs) snapshot() (int64, int64, int64, int64, int64, int64) {
	if o == nil {
		return 0, 0, 0, 0, 0, 0
	}
	samples := o.samples.Load()
	total := o.totalNs.Load()
	var mean int64
	if samples > 0 {
		mean = total / samples
	}
	return samples, o.nonzero.Load(), o.maxNs.Load(), o.lastNs.Load(), total, mean
}

// observeStatusReadLock records the wait the status read path paid for
// Loop.mu. Called (only) at ZeroSelectStats — the single Loop.mu site on
// /api/v1/status. A future second site must call this too, or the attribution
// under-reports.
func (l *Loop) observeStatusReadLock(d time.Duration) { l.statusReadLock.observe(d) }

// unlockEval releases the evaluate() write lock, clears the in-flight flag and
// records how long the pass held it (PERF-001-R2). Called at every evaluate()
// exit path, so the hold is measured even for the early returns (paused loop,
// packer error, zero select).
func (l *Loop) unlockEval(since time.Time) {
	l.evalInFlight.Store(false)
	l.evalHold.observe(time.Since(since))
	l.mu.Unlock()
}

// EvalInFlight reports whether an evaluate() pass holds the write lock right
// now. A sampler uses it to label each /api/v1/status sample: false = the idle
// arm, true = the contended arm.
func (l *Loop) EvalInFlight() bool { return l != nil && l.evalInFlight.Load() }

// LockAttribution returns the measured lock-contention attribution
// (PERF-001-R2). Cheap: five atomic loads plus arithmetic, safe to call from
// the status handler on every request.
func (l *Loop) LockAttribution() LockAttribution {
	if l == nil {
		return LockAttribution{}
	}
	samples, blocked, maxNs, lastNs, totalNs, meanNs := l.statusReadLock.snapshot()
	hs, _, hMax, hLast, hTotal, hMean := l.evalHold.snapshot()
	return LockAttribution{
		StatusReadLock: LockWaitStats{
			Samples: samples, Blocked: blocked,
			MaxNs: maxNs, LastNs: lastNs, MeanNs: meanNs, TotalNs: totalNs,
		},
		EvalHold: HoldStats{
			Samples: hs, MaxNs: hMax, LastNs: hLast, MeanNs: hMean, TotalNs: hTotal,
		},
		EvalInFlight: l.evalInFlight.Load(),
		EvalHoldSite: evalHoldSiteStatusReadLock,
		Stages:       l.evalStages.snapshot(),
	}
}

// BlockProfileState reports the armed block profile (PERF-001-R2).
func (l *Loop) BlockProfileState() BlockProfileState {
	return BlockProfileState{
		Armed: BlockProfileArmed(),
		Rate:  BlockProfileRate(),
		Env:   BlockProfileRateEnv,
	}
}

// ── PERF-001-R2 stage attribution ─────────────────────────────────────────
//
// The A/B answers "does the status read path wait on Loop.mu, and for how
// long". It does not answer the row's follow-on question: WHICH sub-call of
// evaluate() is holding the lock for that long. The hold alone cannot say —
// evaluate() is one monolithic locked section — so each stage of evaluate()'s
// Phase 1 is timed separately. The stage numbers are the lock hold decomposed:
// a reader that waited H ns waited for exactly the stages that were running.
//
// Stage vocabulary (fixed, never extended ad hoc — an operator reads these as
// a series):
//
//	gate          — board-stasis gate arming (atomics + packer wiring)
//	host_sample   — recordHostSample (one load/mem sample per pass)
//	events_emit   — the "evaluation started" event write
//	cleanup_stale — lifecycle.CleanupStaleProjects + slot-pool reconciliation
//	budget_gate   — NewBudgetGate + wave-shed wiring (spend queries)
//	cadence       — LoadCadenceRates (achieved-rate query) + packer wiring
//	pick          — the picker, including board reads under the lock
//	admit_emit    — the ADMIT pass emitter + budget-hold bookkeeping
//	zero_select   — GAP-043 zero-select diagnostics (eligible-lane walk)
const (
	EvalStageGate       = "gate"
	EvalStageHostSample = "host_sample"
	EvalStageEventsEmit = "events_emit"
	EvalStageCleanup    = "cleanup_stale"
	EvalStageBudgetGate = "budget_gate"
	EvalStageCadence    = "cadence"
	EvalStagePick       = "pick"
	EvalStageAdmitEmit  = "admit_emit"
	EvalStageZeroSelect = "zero_select"
)

// EvalStageStats is one stage's process-lifetime wall-clock cost inside the
// evaluate() write lock.
type EvalStageStats struct {
	Stage   string `json:"stage"`
	Samples int64  `json:"samples"`
	MaxNs   int64  `json:"max_ns"`
	LastNs  int64  `json:"last_ns"`
	MeanNs  int64  `json:"mean_ns"`
	TotalNs int64  `json:"total_ns"`
}

// evalStageObs is the fixed-vocabulary stage accumulator. The map is built
// once (NewLoop) and never mutated afterwards, so the status handler reads it
// without a lock while a pass writes into it.
type evalStageObs struct {
	order []string
	obs   map[string]*durationObs
}

func newEvalStageObs() *evalStageObs {
	order := []string{
		EvalStageGate, EvalStageHostSample, EvalStageEventsEmit, EvalStageCleanup,
		EvalStageBudgetGate, EvalStageCadence, EvalStagePick, EvalStageAdmitEmit,
		EvalStageZeroSelect,
	}
	obs := make(map[string]*durationObs, len(order))
	for _, s := range order {
		obs[s] = &durationObs{}
	}
	return &evalStageObs{order: order, obs: obs}
}

func (o *evalStageObs) observe(stage string, d time.Duration) {
	if o == nil {
		return
	}
	if ob, ok := o.obs[stage]; ok {
		ob.observe(d)
	}
}

// snapshot returns the stages in vocabulary order, skipping the ones a pass
// shape never reaches (their absence is information, not a zero to print).
func (o *evalStageObs) snapshot() []EvalStageStats {
	if o == nil {
		return nil
	}
	out := make([]EvalStageStats, 0, len(o.order))
	for _, stage := range o.order {
		ob := o.obs[stage]
		samples, _, maxNs, lastNs, totalNs, meanNs := ob.snapshot()
		if samples == 0 {
			continue
		}
		out = append(out, EvalStageStats{
			Stage: stage, Samples: samples,
			MaxNs: maxNs, LastNs: lastNs, MeanNs: meanNs, TotalNs: totalNs,
		})
	}
	return out
}

// observeEvalStage records how long one stage of evaluate()'s locked Phase 1
// took. Measurement only: the call is a time.Since plus an atomic store.
func (l *Loop) observeEvalStage(stage string, since time.Time) {
	l.evalStages.observe(stage, time.Since(since))
}
