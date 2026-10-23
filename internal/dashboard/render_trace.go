package dashboard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// PERF-001-R3-R2 — render tracing for the fleet overview.
//
// The row's problem: the first GET / after an idle window blocked 79.7s with
// ~0 request CPU and the daemon logged NOTHING about it (`grep 'GET / '` →
// 0 matches), so the wait was unrecoverable after the fact. Two separate
// defects in one observation surface:
//
//  1. no per-request log line at all (nothing named the path or duration);
//  2. even with a duration, nothing named WHERE the time went — the cold
//     render is a pipeline (DB queries → lanes → batched stats → CI warm
//     pass → per-project enrichment), and "79s" without a stage name would
//     re-open the same investigation.
//
// A RenderTrace is the per-request recorder both halves need. The HTTP
// middleware in cmd/schedulerd owns one per request and puts it in the
// request context; collect() and its stages mark their own spans, so the
// slow-request line can print the stage breakdown (and, when the render
// budget trips, which stage was STILL IN FLIGHT). It is allocation-cheap on
// the fast path (a few struct appends) and never held across a render.
//
// Clock discipline: every instant comes from a clock.Clock (the generator's
// seam, real by default) so internal/clock's stdlib guard stays satisfied
// and the sim-clock tests can drive duration arithmetic.

// StageTiming is one finished stage of an overview render.
type StageTiming struct {
	Name string
	Dur  time.Duration
}

// renderTrace is the per-render stage recorder. The zero value is not usable;
// construct with NewRenderTrace. It is safe for concurrent use because
// collect()'s later stages run inside a bounded-concurrency pool.
//
// The trace is deliberately NOT a metrics registry: it exists to name one
// slow request in the log, so it keeps a flat stage list (no aggregation) and
// is discarded when the request ends.
type renderTrace struct {
	mu sync.Mutex
	// clk is the clock the stages are timed with (the generator's seam).
	clk clock.Clock
	// started is the render's first instant.
	started time.Time
	// stages are completed spans, in completion order.
	stages []StageTiming
	// current is the stage that started and has not finished yet (""=none).
	current string
	// currentSince is when current started.
	currentSince time.Time
	// served records how the render was satisfied.
	served string // "", "stale", "partial"
	// staleAge is the age of the snapshot served (served == "stale").
	staleAge time.Duration
	// budget is the effective render budget (0 = unbounded).
	budget time.Duration
	// note carries the operator-facing degrade reason.
	note string
}

// RenderTrace is the exported handle the HTTP middleware holds so it can log
// the breakdown after the handler returns. It wraps the internal recorder.
type RenderTrace struct{ tr *renderTrace }

// NewRenderTrace starts a trace. clk is the generator's clock seam; nil means
// the real clock.
func NewRenderTrace(clk clock.Clock) *RenderTrace {
	if clk == nil {
		clk = clock.Real()
	}
	return &RenderTrace{tr: &renderTrace{clk: clk, started: clk.Now()}}
}

type renderTraceKey struct{}

// WithRenderTrace returns a context carrying tr. The dashboard's HTTP
// entry points read it so a single collect() feeds both the page and the
// slow-request log line.
func WithRenderTrace(ctx context.Context, tr *RenderTrace) context.Context {
	if tr == nil || tr.tr == nil {
		return ctx
	}
	return context.WithValue(ctx, renderTraceKey{}, tr)
}

// RenderTraceFrom returns the trace carried by ctx, or nil.
func RenderTraceFrom(ctx context.Context) *RenderTrace {
	if ctx == nil {
		return nil
	}
	rt, _ := ctx.Value(renderTraceKey{}).(*RenderTrace)
	return rt
}

// stageTimer marks a stage boundary. It returns a function that records the
// stage's duration; calling it more than once records only the first span.
func stageTimer(ctx context.Context, name string) func() {
	rt := RenderTraceFrom(ctx)
	if rt == nil || rt.tr == nil {
		return func() {}
	}
	t := rt.tr
	start := t.clk.Now()
	t.mu.Lock()
	t.current = name
	t.currentSince = start
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			end := t.clk.Now()
			t.mu.Lock()
			t.stages = append(t.stages, StageTiming{Name: name, Dur: end.Sub(start)})
			if t.current == name {
				t.current = ""
			}
			t.mu.Unlock()
		})
	}
}

// SetBudget records the effective render budget on the trace (for the log).
func (rt *RenderTrace) SetBudget(d time.Duration) {
	if rt == nil || rt.tr == nil {
		return
	}
	rt.tr.mu.Lock()
	rt.tr.budget = d
	rt.tr.mu.Unlock()
}

// MarkServedStale records that the render served a snapshot older than the
// reuse window because the refresh did not finish inside the budget.
func (rt *RenderTrace) MarkServedStale(age time.Duration, note string) {
	if rt == nil || rt.tr == nil {
		return
	}
	rt.tr.mu.Lock()
	rt.tr.served = "stale"
	rt.tr.staleAge = age
	rt.tr.note = note
	rt.tr.mu.Unlock()
}

// MarkPartial records that the render degraded to a partial page because no
// snapshot existed and the first collect did not finish inside the budget.
func (rt *RenderTrace) MarkPartial(note string) {
	if rt == nil || rt.tr == nil {
		return
	}
	rt.tr.mu.Lock()
	rt.tr.served = "partial"
	rt.tr.note = note
	rt.tr.mu.Unlock()
}

// Elapsed is the time since the trace started.
func (rt *RenderTrace) Elapsed() time.Duration {
	if rt == nil || rt.tr == nil {
		return 0
	}
	return rt.tr.clk.Since(rt.tr.started)
}

// Summary renders the one-line breakdown a slow-request log line carries:
//
//	wait=ci-warm[78.1s] stages=db-projects[1.2s],db-samples[0.4s],... served=stale[84s]
//
// The wait= field is the stage that was IN FLIGHT when the render stopped
// waiting (or, for a completed render, the slowest stage) — that is the field
// an operator greps to name the holder.
func (rt *RenderTrace) Summary() string {
	if rt == nil || rt.tr == nil {
		return ""
	}
	t := rt.tr
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := make([]string, 0, len(t.stages))
	var slowest StageTiming
	var inflight string
	var inflightFor time.Duration
	for _, s := range t.stages {
		parts = append(parts, fmt.Sprintf("%s[%s]", s.Name, roundDur(s.Dur)))
		if s.Dur > slowest.Dur {
			slowest = s
		}
	}
	if t.current != "" {
		inflight = t.current
		inflightFor = t.clk.Since(t.currentSince)
	}
	wait := ""
	switch {
	case inflight != "":
		wait = fmt.Sprintf("%s[%s,in-flight]", inflight, roundDur(inflightFor))
	case slowest.Name != "":
		wait = fmt.Sprintf("%s[%s]", slowest.Name, roundDur(slowest.Dur))
	}
	var b strings.Builder
	if wait != "" {
		fmt.Fprintf(&b, "wait=%s", wait)
	}
	if len(parts) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "stages=%s", strings.Join(parts, ","))
	}
	if t.served != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "served=%s", t.served)
		if t.served == "stale" && t.staleAge > 0 {
			fmt.Fprintf(&b, "[%s]", roundDur(t.staleAge))
		}
	}
	if t.note != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "note=%q", t.note)
	}
	if t.budget > 0 {
		fmt.Fprintf(&b, " budget=%s", roundDur(t.budget))
	}
	return b.String()
}

// roundDur trims a duration to a compact, log-friendly form (ms resolution
// above a second, µs below, never float noise like 1.234567891s).
func roundDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	case d >= time.Microsecond:
		return d.Round(time.Microsecond).String()
	default:
		return d.String()
	}
}
