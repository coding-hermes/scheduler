package scheduler

import (
	"sync/atomic"
	"time"
)

// PERF-001-R2 — eval-pass attribution for /api/v1/status, reconciled with
// PERF-001-R3's lock restructure.
//
// The history is the whole point of this file:
//
//   - PERF-001-R2 measured /api/v1/status at p50 326 ms / p99 ~350 ms against
//     PERF-001's recorded bar (p99 < 100 ms) with CPU:wall = 47 ms : 326 ms,
//     and the goroutine dump taken at the stall showed the handler parked in
//     sync.RWMutex.RLock inside Loop.ZeroSelectStats while evaluate() held the
//     write lock across its whole pass. The row asks for the A/B and the block
//     profile — attribution first, no guessed fix.
//   - PERF-001-R3 then removed the coupling itself: evaluate() serializes
//     passes on evalMu and holds Loop.mu only for O(1) state updates, and
//     ZeroSelectStats / LastEvalTime / IsPaused read atomics. The status read
//     path therefore has NO Loop.mu site left.
//
// So this file's job is now two things, both measurable without a profiler:
//
//   - evalPass / evalInFlight: how long each evaluate() DECISION window takes
//     (the evalMu-serialized Phase 1a–1c), and whether one is open right now.
//     A sampler uses EvalInFlight() to split its idle arm from its "a pass is
//     running" arm — the arm that used to convoy (A/B in PERF-001-R2's sense).
//     The window is now OFF the read path: an operator request no longer waits
//     for it, which is exactly the "no lock wait detected" arm the row named.
//   - evalStages: that window decomposed by phase, which answers the row's
//     third question — WHICH sub-call of evaluate() spends the time.
//
// Read-path lock discipline is served as a string (ReadPathLockSite) so a
// consumer cannot mistake "0 samples" for "never observed": after PERF-001-R3
// there is no lock to observe.
//
// Measurement clock: these observers read the loop's CLOCK SEAM
// (l.clock(), the real wall clock by default), never time.Now directly — the
// seam is this project's single time choke point (ADV-R04/G6), guarded by
// internal/clock's TestNoDirectStdlibClockCallsOutsideClock. A simulator clock
// therefore drives these windows too, which is the correct reading: under a
// simulator the "pass" is simulated time as well.
//
// A pass's window is the DECISION phase (evalMu-serialized Phase 1a-1c). Under
// a simulator the seam's Now may not advance inside one pass, so a sim-driven
// test legitimately observes a zero-length window; the real daemon (and every
// production-shaped test) reads real durations.

// PassStats is a process-lifetime summary of evaluate()'s decision window.
// Counters are monotonic and reset only by a restart, matching the other
// process-scoped counters on the Loop (spawn / gateway / admission).
type PassStats struct {
	// Samples is the number of completed windows observed.
	Samples int64 `json:"samples"`
	// MaxNs / LastNs / MeanNs are the longest, the most recent and the
	// average window, in nanoseconds.
	MaxNs  int64 `json:"max_ns"`
	LastNs int64 `json:"last_ns"`
	MeanNs int64 `json:"mean_ns"`
	// TotalNs is the summed window time.
	TotalNs int64 `json:"total_ns"`
}

// EvalStageStats is one stage's process-lifetime wall-clock cost inside the
// evaluate() decision window.
type EvalStageStats struct {
	Stage   string `json:"stage"`
	Samples int64  `json:"samples"`
	MaxNs   int64  `json:"max_ns"`
	LastNs  int64  `json:"last_ns"`
	MeanNs  int64  `json:"mean_ns"`
	TotalNs int64  `json:"total_ns"`
}

// LockAttribution is the JSON-ready attribution block the API serves. Every
// number is measured, never derived: zero samples mean "no pass has completed
// since this process started", never a derived zero.
type LockAttribution struct {
	// EvalPass is the evaluate() decision window (evalMu-serialized, the
	// window a second operator read used to queue behind).
	EvalPass PassStats `json:"eval_pass"`
	// EvalInFlight is true only while that window is open.
	EvalInFlight bool `json:"eval_in_flight"`
	// ReadPathLockSite states the read path's lock discipline as a fact
	// rather than a measurement: a consumer that sees no read-path lock
	// numbers must be able to tell "nothing waits on a lock" from "the
	// probe is missing".
	ReadPathLockSite string `json:"read_path_lock_site"`
	// Stages decomposes EvalPass by phase — the row's "which sub-call"
	// answer. Stages a pass shape never reaches are omitted.
	Stages []EvalStageStats `json:"stages,omitempty"`
}

// readPathLockSite is served verbatim so the lock discipline travels with the
// numbers it explains.
const readPathLockSite = "lock-free: no Loop.mu site on the /api/v1/status read path (PERF-001-R3: ZeroSelectStats, LastEvalTime and IsPaused read atomics)"

// BlockProfileState reports the armed block profile (PERF-001-R2). It is
// process-scoped, not per-pass: the rate is resolved once, at the head of the
// first evaluate() that runs.
type BlockProfileState struct {
	Armed bool   `json:"armed"`
	Rate  int    `json:"rate"`
	Env   string `json:"env"`
}

// Stage vocabulary — the evaluate() decision window, in the order the phases
// run (PERF-001-R3's lock discipline is what these names document):
//
//	phase1a      — entry, under mu: the paused gate, the decision instant, the
//	               board-stasis arming, the snapshots Phase 1b needs
//	host_sample  — recordHostSample + the goroutine-count check + the
//	               "evaluation started" event
//	cleanup_stale— CleanupStaleProjects + slot-pool reconciliation
//	budget_gate  — NewBudgetGate + wave-shed wiring (spend queries)
//	cadence      — LoadCadenceRates (achieved-rate query) + packer wiring
//	pick         — the packer (namespace pack or flat Pick), incl. the
//	               per-board freshness reads
//	admit_emit   — the ADMIT pass emitter + the budget-hold bookkeeping
//	zero_select  — GAP-043 zero-select diagnostics (zero-select passes only)
//	phase1c      — the mu-held tail: budget-hold view publish, the zero-select
//	               counter reset, the EVAL line
const (
	EvalStagePhase1A    = "phase1a"
	EvalStageHostSample = "host_sample"
	EvalStageCleanup    = "cleanup_stale"
	EvalStageBudgetGate = "budget_gate"
	EvalStageCadence    = "cadence"
	EvalStagePick       = "pick"
	EvalStageAdmitEmit  = "admit_emit"
	EvalStageZeroSelect = "zero_select"
	EvalStagePhase1C    = "phase1c"
)

// evalStageObs is the fixed-vocabulary stage accumulator. The map is built
// once (NewLoop) and never mutated afterwards, so the status handler reads it
// without a lock while a pass writes into it.
type evalStageObs struct {
	order []string
	obs   map[string]*durationObs
}

func newEvalStageObs() *evalStageObs {
	order := []string{
		EvalStagePhase1A, EvalStageHostSample, EvalStageCleanup, EvalStageBudgetGate,
		EvalStageCadence, EvalStagePick, EvalStageAdmitEmit, EvalStageZeroSelect,
		EvalStagePhase1C,
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

// durationObs is the atomic accumulator behind every exported summary. It is
// NOT a histogram: the row needs max/last/mean plus a count, and a
// fixed-bucket histogram would cost more than the thing it measures.
// Deliberately lock-free — an observer that takes a lock to measure a lock
// would measure itself.
type durationObs struct {
	samples atomic.Int64
	nonzero atomic.Int64
	maxNs   atomic.Int64
	lastNs  atomic.Int64
	totalNs atomic.Int64
}

// observe records one completed window. Negative durations (a clock
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

// endEvalPassObs closes evaluate()'s measurement window exactly once (the
// atomic swap is the guard): in-flight goes false and the window's duration is
// recorded. Called explicitly at the Phase-1/Phase-2 boundary and defensively
// by a defer, so every early return and a panic are covered.
func (l *Loop) endEvalPassObs(since time.Time) {
	if l.evalInFlight.Swap(false) {
		l.evalPass.observe(l.clock().Now().Sub(since))
	}
}

// observeEvalStage records how long one stage of the decision window took.
// Measurement only: one seam read plus an atomic store.
func (l *Loop) observeEvalStage(stage string, since time.Time) {
	l.evalStages.observe(stage, l.clock().Now().Sub(since))
}

// EvalInFlight reports whether an evaluate() decision window is open right
// now. A sampler uses it to label each /api/v1/status sample: false = the idle
// arm, true = the "a pass is running" arm.
func (l *Loop) EvalInFlight() bool { return l != nil && l.evalInFlight.Load() }

// LockAttribution returns the measured pass attribution (PERF-001-R2). Cheap:
// a few atomic loads plus arithmetic, safe to call from the status handler on
// every request.
func (l *Loop) LockAttribution() LockAttribution {
	if l == nil {
		return LockAttribution{}
	}
	samples, _, maxNs, lastNs, totalNs, meanNs := l.evalPass.snapshot()
	return LockAttribution{
		EvalPass: PassStats{
			Samples: samples, MaxNs: maxNs, LastNs: lastNs, MeanNs: meanNs, TotalNs: totalNs,
		},
		EvalInFlight:     l.evalInFlight.Load(),
		ReadPathLockSite: readPathLockSite,
		Stages:           l.evalStages.snapshot(),
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
