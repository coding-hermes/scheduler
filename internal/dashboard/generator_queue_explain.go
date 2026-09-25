package dashboard

// SCHED-GAP-1589 — queue-page explainability. The queue has ranked lanes by
// urgency since SCHED-GAP-174 but explained nothing: what the columns mean,
// why a 5-digit urgency number appears, or why an eligible lane is still
// waiting. This file adds the EXPLAIN pass: per-lane "why waiting" evidence
// (latest deferrals pass-over + running-tick admission stamps, SCHED-GAP-157)
// and display-only urgency context (the lane's own tick interval, the waited
// time, a band relative to the page's own distribution, and the page's
// median/p90 scale).
//
// The evidence rides in the SAME single query queueEntries already runs
// (scalar subqueries + projected columns — no join, so multiple running
// ticks per lane cannot duplicate a row), keeping the SCHED-GAP-174 /
// DASH-PERF one-query budget intact. What remains here is pure in-memory
// post-processing.
//
// Display only — the sort key and the values the SCHED-GAP-174 parity gate
// pins are untouched, and no scheduling behavior changes here. Whether the
// packer should CLAMP urgency for ordering is a packer-semantics decision
// that belongs in its own row.

import (
	"fmt"
	"math"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// queueLaneEvidence is one lane's raw explain evidence, scanned alongside
// its projects row by queueEntries' single query.
type queueLaneEvidence struct {
	DefReason string
	DefDetail string
	DefAt     string
	RunTickID string
	RunAdmit  string
	RunWaitMs int64
	RunAt     string
}

// queueEvidenceSelect is the SELECT-list fragment queueEntries appends to
// its rank query. Scalar subqueries only (SCHED-GAP-1589): a LEFT JOIN over
// running ticks would duplicate a lane with several live workers. Every
// subquery orders identically (created_at DESC, id DESC) so its columns
// describe one consistent record, and COALESCE keeps the scan NULL-free.
const queueEvidenceSelect = `,
  COALESCE((SELECT d.reason FROM deferrals d WHERE d.project_name = p.name ORDER BY d.created_at DESC, d.id DESC LIMIT 1), ''),
  COALESCE((SELECT d.detail FROM deferrals d WHERE d.project_name = p.name ORDER BY d.created_at DESC, d.id DESC LIMIT 1), ''),
  COALESCE((SELECT d.created_at FROM deferrals d WHERE d.project_name = p.name ORDER BY d.created_at DESC, d.id DESC LIMIT 1), ''),
  COALESCE((SELECT rt.id FROM ticks rt WHERE rt.project_name = p.name AND rt.status = 'running' ORDER BY rt.created_at DESC, rt.id DESC LIMIT 1), ''),
  COALESCE((SELECT rt.admit_reason FROM ticks rt WHERE rt.project_name = p.name AND rt.status = 'running' ORDER BY rt.created_at DESC, rt.id DESC LIMIT 1), ''),
  COALESCE((SELECT rt.slot_wait_ms FROM ticks rt WHERE rt.project_name = p.name AND rt.status = 'running' ORDER BY rt.created_at DESC, rt.id DESC LIMIT 1), 0),
  COALESCE((SELECT rt.created_at FROM ticks rt WHERE rt.project_name = p.name AND rt.status = 'running' ORDER BY rt.created_at DESC, rt.id DESC LIMIT 1), '')
`

// deferralGloss translates the SCHED-GAP-155 deferrals vocabulary into the
// operator terms the queue page shows. Unmapped reasons fall back to the
// raw vocabulary word.
var deferralGloss = map[string]string{
	"cap":             "namespace concurrency cap - every slot in the lane's namespace is in use",
	"cooldown":        "cooldown - the lane is waiting out its pacing window since its last completed tick",
	"load_gate":       "load gate - host load is above the spawn threshold, so spawns are deferred",
	"tasks_no_work":   "no admissible work on the lane's board",
	"board_unowned":   "board unowned - another agent currently owns the lane's board",
	"budget":          "budget - the weight budget or a spend cap left no room for this lane",
	"tasks_deferred":  "deferred - failure backoff or post-tick pacing after the last pass",
	"failed_cooldown": "failed cooldown - the last tick failed, so the full cooldown applies",
	"ok":              "admitted normally on the last pass",
}

// explainQueueEntries is the pure post-processing half of SCHED-GAP-1589:
// it turns the per-lane evidence (already scanned into Entry.Why fields by
// queueEntries) into rendered why-waiting cells, computes the urgency bands
// relative to THIS page's distribution, and the header's median/p90 scale.
// Must run AFTER the sort (bands and percentiles are order-derived).
func explainQueueEntries(data *QueueData, now time.Time) {
	for i := range data.Entries {
		e := &data.Entries[i]
		switch {
		case e.WhyRunning:
			// The lane IS running: show its SCHED-GAP-157 admission stamp.
			e.WhyDetail = humanSlotWait(e.WhyWaitMs)
			e.WhyTitle = fmt.Sprintf("running since %s; admitted %s after %s slot wait (ticks.admit_reason + slot_wait_ms, SCHED-GAP-157)",
				relativeTime(e.WhyAt, now), e.WhyReason, e.WhyDetail)
		case e.WhyReason != "" && !e.CooldownActive:
			// Eligible (past cooldown) but the packer last passed it over:
			// THIS is the answer to "why is a high-urgency lane stuck".
			gloss := e.WhyReason
			if mapped, known := deferralGloss[e.WhyReason]; known {
				gloss = mapped
			}
			e.WhyTitle = fmt.Sprintf("%s. Last recorded pass-over %s: %s (deferrals table, SCHED-GAP-157)",
				gloss, relativeTime(e.WhyAt, now), e.WhyDetail)
		case e.CooldownActive:
			e.WhyTitle = "The lane waits cooldown_s seconds after its last COMPLETED tick before it is eligible again. Cooldown is a pacing floor, not the queue order — lanes held PAST their cooldown are shown in this column with their recorded pass-over reason instead."
		default:
			e.WhyTitle = "No recorded pass-over and no active cooldown: the lane is ready and is waiting on the packer's weight budget or the global concurrency cap."
		}
	}

	n := len(data.Entries)
	if n == 0 {
		return
	}
	// Entries are sorted urgency-desc; nearest-rank percentiles on the
	// descending slice.
	urgs := make([]float64, n)
	for i, e := range data.Entries {
		urgs[i] = e.Urgency
	}
	data.MedianUrgencyText = fmt.Sprintf("%.1f", urgs[nearestRankIdx(n, 0.5)])
	data.P90UrgencyText = fmt.Sprintf("%.1f", urgs[nearestRankIdx(n, 0.9)])
	if n >= 4 {
		top := (n + 9) / 10 // ceil(n/10), >= 1: the top decile of THIS page
		for i := range data.Entries {
			switch {
			case i < top:
				data.Entries[i].Band = "top decile"
			case i < n/2:
				data.Entries[i].Band = "above median"
			}
		}
	}
}

// fillQueueTiming computes the display-only timing context (SCHED-GAP-1589):
// the lane's own tick interval from its priority (the same ComputeInterval
// the engine uses), how long it has waited — mirroring the engine's elapsed
// input: last completion, else creation — and whether its cooldown window is
// still active.
func fillQueueTiming(e *QueueEntry, calc *scheduler.UrgencyCalculator, now time.Time, lastCompleted *time.Time, createdAt time.Time) {
	if calc != nil {
		e.IntervalText = humanWait(calc.ComputeInterval(float64(e.Priority)).Seconds())
	}
	var elapsed time.Duration
	switch {
	case lastCompleted != nil:
		elapsed = now.Sub(*lastCompleted)
	case !createdAt.IsZero():
		elapsed = now.Sub(createdAt)
	default:
		e.WaitedText = "unknown"
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	e.WaitedText = humanWait(elapsed.Seconds())
	if e.CooldownS > 0 && lastCompleted != nil {
		if remaining := time.Duration(e.CooldownS)*time.Second - elapsed; remaining > 0 {
			e.CooldownActive = true
			e.CooldownText = "cooldown " + humanWait(remaining.Seconds()) + " left"
		}
	}
}

// humanWait renders a seconds count the way the queue page explains waits
// and intervals. Countdowns (cooldown remaining, waited time) FLOOR: never
// overstate the remaining window.
func humanWait(seconds float64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%.0fs", seconds)
	case seconds < 90*60:
		return fmt.Sprintf("%.0fm", math.Floor(seconds/60))
	case seconds < 36*3600:
		return fmt.Sprintf("%.1fh", math.Floor(seconds/360)/10)
	default:
		return fmt.Sprintf("%.1fd", math.Floor(seconds/8640)/10)
	}
}

// humanSlotWait renders a SCHED-GAP-157 slot_wait_ms stamp.
func humanSlotWait(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%.1fm", float64(ms)/60000)
	}
}

// relativeTime renders an RFC3339 stamp as "Xs/Xm/Xh ago" against now.
func relativeTime(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "unknown time"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return humanWait(d.Seconds()) + " ago"
}

// nearestRankIdx returns the slice index of the nearest-rank percentile p
// over a DESCENDING slice of n values: ascending rank r maps to descending
// index n-rank, NOT rank-1 (rank-1 indexes from the top and yields the
// INVERSE percentile — a descending-slice p90 read as p10).
func nearestRankIdx(n int, p float64) int {
	rank := int(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return n - rank
}
