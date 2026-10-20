package scheduler

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// PERF-001-R2 — block-profile arming for the evaluate() path.
//
// The row: /api/v1/status answers p50 326ms / p99 ~350ms against PERF-001's
// recorded PASS bar (p99 < 100ms), while the handler's profile is
// CPU:wall = 47ms:326ms — the request spends ~86% of its life NOT computing,
// so a query rewrite cannot close the gap. The two surfaces that would name
// the WAIT were both dark: GET /debug/pprof/block and /debug/pprof/mutex each
// returned "0 nodes ... 0 total" because nothing in the process ever called
// runtime.SetBlockProfileRate (cmd/schedulerd/main.go already registers
// net/http/pprof on DefaultServeMux, so only the RATE was missing).
//
// Arming the block profile turns that already-exposed endpoint into the
// attribution surface: it records, per blocking event, the goroutine stack
// that blocked and how long it waited — i.e. WHICH lock/syscall the status
// request queued on. A sync.RWMutex (Loop.mu) is implemented over runtime
// semaphores, so its read-lock waits land in the block profile; the mutex
// profile (SetMutexProfileFraction) is deliberately NOT armed here — it
// profiles contention on sync.Mutex critical sections, not the lock wait the
// row is chasing.
//
// Placement (the row's step 1): the arm call sits at the HEAD of
// Loop.evaluate(), BEFORE it takes l.mu, so the profile is live for the pass
// that holds the write lock the status read path contends for. It is
// idempotent: the rate is resolved once per process, so a long-lived daemon
// pays a single atomic load per evaluate pass after the first.
//
// Cost control: rate 1 records EVERY blocking event (the row's explicit
// request), which is the most expensive setting. The rate stays tunable, and
// 0 disables the profile entirely:
//
//	SCHEDULER_BLOCK_PROFILE_RATE=<n>   n > 0  ⇒ sample ~one event per n ns blocked
//	                                  n <= 0  ⇒ disabled (no sampling overhead)
//	                                  unset / unparsable ⇒ DefaultBlockProfileRate
const (
	// BlockProfileRateEnv tunes the armed block-profile rate. Empty means
	// DefaultBlockProfileRate; 0 or negative disables the profile.
	BlockProfileRateEnv = "SCHEDULER_BLOCK_PROFILE_RATE"
	// DefaultBlockProfileRate is the rate armed when the env var is unset:
	// 1 = record every blocking event (PERF-001-R2's ask — the wait could not
	// be attributed at any coarser sampling, and the live p50 is 3.3x the
	// criterion, so precision beats overhead here).
	DefaultBlockProfileRate = 1
)

var (
	// blockProfileOnce makes the arming a one-shot: the runtime rate is
	// process-global state, and re-resolving it per evaluate pass would
	// re-introduce the cost the once-only arm exists to avoid.
	blockProfileOnce sync.Once
	// blockProfileArmed records that the arm attempt completed (rate stored).
	blockProfileArmed atomic.Bool
	// blockProfileRate is the RESOLVED rate once armed: 0 = disabled.
	blockProfileRate atomic.Int64
)

// ArmBlockProfile arms the runtime block profile from the evaluate() path
// (PERF-001-R2) and reports whether the profile is enabled. Safe to call on
// every pass: after the first call it is one atomic load. The rate comes from
// SCHEDULER_BLOCK_PROFILE_RATE, defaulting to DefaultBlockProfileRate.
func ArmBlockProfile() bool {
	if !blockProfileArmed.Load() {
		blockProfileOnce.Do(func() { armBlockProfile(os.Getenv(BlockProfileRateEnv)) })
	}
	return blockProfileRate.Load() > 0
}

// armBlockProfile resolves the rate and installs it. Rate is stored BEFORE
// the armed flag so a concurrent reader can never observe "armed" with an
// unset rate.
func armBlockProfile(raw string) {
	rate := resolveBlockProfileRate(raw)
	if rate > 0 {
		runtime.SetBlockProfileRate(rate)
	}
	blockProfileRate.Store(int64(rate))
	blockProfileArmed.Store(true)
}

// resolveBlockProfileRate maps the env spelling to a runtime rate. Empty and
// unparsable input fall back to the default (an operator typo must not
// silently disable the attribution the row asked for); 0 and negative mean
// "off" so an operator can always shed the sampling cost.
func resolveBlockProfileRate(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBlockProfileRate
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultBlockProfileRate
	}
	if n <= 0 {
		return 0
	}
	return n
}

// BlockProfileArmed reports whether the block profile is enabled.
func BlockProfileArmed() bool { return blockProfileRate.Load() > 0 }

// BlockProfileRate returns the resolved block-profile rate (0 = disabled or
// not yet armed).
func BlockProfileRate() int { return int(blockProfileRate.Load()) }

// BlockProfileArmed reports whether the block profile is enabled (process
// scope; exposed on the Loop so the API surface can report it next to the
// lock attribution it explains).
func (l *Loop) BlockProfileArmed() bool { return BlockProfileArmed() }

// BlockProfileRate returns the resolved block-profile rate (0 = disabled or
// not yet armed).
func (l *Loop) BlockProfileRate() int { return BlockProfileRate() }
