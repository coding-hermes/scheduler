package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1707 — session silence watchdog (deliverable 2).
//
// The builder no-artifact guard (SCHED-GAP-1674) judges a session that is
// TALKING-but-not-writing. This watchdog covers the other dead shape: a
// session that is not talking AT ALL — no token delta and no tool activity —
// sitting on its slot until the tick wall while the fleet cannot tell it
// from a productive long run. The zombie reapers only catch a DEAD OWNER
// (heartbeat gone); a live-but-silent gateway session owns a fresh
// heartbeat and burns the slot in silence.
//
// Contract:
//   - OFF by default. sessionSilenceGrace() returns 0 until the daemon arms
//     it (SetSessionSilenceGrace); embedding tests and unit suites keep
//     byte-identical behavior (the knob convention's library-default rule,
//     the same constructed-disabled posture as the board-stasis gate).
//   - A pass over every running gateway tick (pid=0 — the guard pass's own
//     query shape) measures the session's Hermes-state telemetry: token
//     totals and tool calls visible to the agent store. A tick whose
//     session shows NO activity since spawn for >= grace is CANCELLED
//     through Spawner.CancelTickSession — the same abort mechanism the
//     builder guard uses — so the spawn path classifies the outcome and
//     Wait()/lifecycle.Complete() close the row with failure_reason=
//     session_silent (via HarnessFailure) and ticks.session_silence_s set.
//   - NEVER kills a producing session: any nonzero activity (tokens moved
//     or tools ran) spares the tick. The acceptance arm drives a
//     genuinely long-running simulated tick on the simulator clock and
//     proves it survives passes a silent twin does not.
//   - NO BACKOFF CHANGE: the watchdog changes nothing about cooldowns,
//     consecutive_failures, or the "No timeout backoff" ruling — a killed
//     tick flows through the existing TickTimeout classification, and the
//     row is marked partial with reason session_silent (deliverable 1's
//     machinery).

var (
	sessionSilenceGraceD time.Duration
	sessionSilenceMu     sync.RWMutex
)

// SetSessionSilenceGrace installs the silence grace (0 = the watchdog is
// off). Called once from main/loop wiring; last writer wins.
func SetSessionSilenceGrace(d time.Duration) {
	sessionSilenceMu.Lock()
	defer sessionSilenceMu.Unlock()
	sessionSilenceGraceD = d
}

// sessionSilenceGrace returns the armed grace (0 = watchdog disabled).
func sessionSilenceGrace() time.Duration {
	sessionSilenceMu.RLock()
	defer sessionSilenceMu.RUnlock()
	return sessionSilenceGraceD
}

// silenceProbe is one running tick's measured session activity. found means
// the agent store HAS a sessions row for the tick; absent (or an unreadable
// store) measures as zero-activity-from-spawn — a session the gateway never
// persisted is the purest silence, and an unreadable store fails open the
// same way the builder guard's observe-nothing does. total is the summed
// input+output token figure; the silence DECISION keys on any nonzero
// activity, never on the split.
type silenceProbe struct {
	found     bool
	total     int64
	toolCalls int64
}

// probeSessionActivity reads one tick's session telemetry from the agent
// state store (read-only, bounded — the same discipline as the builder
// guard's observeSessionTelemetry). found=false on any failure: the
// watchdog must never act on an unreadable store.
func probeSessionActivity(tickID string) silenceProbe {
	var p silenceProbe
	path := hermesStateDBPath()
	if path == "" || tickID == "" {
		return p
	}
	sdb, err := database.OpenHermesStateReadOnly(path)
	if err != nil {
		return p
	}
	defer sdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hermesGuardQueryTimeout)
	defer cancel()

	// The gateway names the session with the tick id (the S-GAP-003
	// placeholder and the real id share the value by construction), so one
	// lookup by session_key serves the token totals (session_model_usage,
	// keyed by session id) and the tool counter (sessions) together.
	var total, tools int64
	err = sdb.QueryRowContext(ctx, `
SELECT COALESCE((SELECT COALESCE(SUM(u.input_tokens),0) + COALESCE(SUM(u.output_tokens),0)
                 FROM session_model_usage u WHERE u.session_id = s.id), 0),
       COALESCE(s.tool_call_count, 0)
  FROM sessions s WHERE s.session_key = ?
 ORDER BY s.started_at DESC LIMIT 1`, tickID).Scan(&total, &tools)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("WARN: session_silence probe for tick %s: %v (observe nothing)", tickID, err)
		}
		return p
	}
	p.found, p.total, p.toolCalls = true, total, tools
	return p
}

// sessionSilencePass is one watchdog sweep over the running gateway ticks.
// Called from the Loop's 60s reaper tick, after the builder guard. Returns
// the number of silent sessions terminated. Best-effort by contract: every
// failure inside is logged and can never touch the reaper's own semantics.
//
// now is the component clock's instant (simulator-driven in tests). The
// grace is read once per pass; 0 (watchdog off) makes the pass a no-op —
// callers may still call unconditionally from the reaper case.
func (l *Loop) sessionSilencePass(now time.Time) int {
	grace := sessionSilenceGrace()
	if grace <= 0 || l.spawner == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Same candidate shape the builder guard passes on: running gateway
	// ticks with the spawn instant. Exec ticks are out of scope (their
	// sessions are not in the agent store; the kill timer owns their wall).
	rows, err := l.db.QueryContext(ctx, `
SELECT t.id, t.project_name, COALESCE(t.spawned_at,'')
FROM ticks t
WHERE t.status = 'running' AND t.pid = 0`)
	if err != nil {
		log.Printf("WARN: session_silence running-tick query failed: %v", err)
		return 0
	}
	type cand struct {
		id        string
		project   string
		spawnedAt string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.project, &c.spawnedAt); err != nil {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if len(cands) == 0 {
		return 0
	}

	killed := 0
	for _, c := range cands {
		spawned, perr := time.Parse(time.RFC3339, c.spawnedAt)
		if perr != nil || spawned.IsZero() {
			continue // no usable clock — never act on an unmeasurable tick
		}
		// The silence window opens at spawn: a tick whose session has
		// produced nothing since spawn for >= grace is silent by
		// definition, whether or not the agent store ever registered it.
		if elapsed := now.Sub(spawned); elapsed < grace {
			continue // still inside the grace window
		}
		probe := probeSessionActivity(c.id)
		if probe.found && (probe.total != 0 || probe.toolCalls != 0) {
			// Activity observed — tokens moved or tools ran. NEVER kill a
			// producing session: this tick is a genuinely long run, spare
			// it (the acceptance arm pins exactly this against a silent
			// twin).
			continue
		}
		// Silent: zero observed activity (or the store never registered
		// the session). Kill through the same cancel registry the builder
		// guard uses; the quiet duration is the full spawn→now silence.
		quiet := now.Sub(spawned)
		if !l.spawner.CancelTickSession(c.id) {
			log.Printf("SESSION-SILENCE: %s tick=%s silent for %v but no live registered session to cancel — skipping",
				c.project, c.id, quiet.Round(time.Second))
			continue
		}
		// Stamp the verdict BEFORE the spawn path observes the cancelled
		// POST: sessionSilenceFor (consume-once, the guardAborted
		// contract) hands the tuple to the classification site, which
		// turns it into the session_silent partial row.
		l.spawner.markSessionSilent(c.id, markSilentTelemetry(0, 0, 0, int64(quiet/time.Second)))
		log.Printf("SESSION-SILENCE: %s tick=%s terminated after %v of zero token/tool activity (grace %v)",
			c.project, c.id, quiet.Round(time.Second), grace)
		killed++
	}
	return killed
}

// markSessionSilent records one watchdog kill on the spawner's consume-once
// registry. Guarded by the spawner's own mutex (the same lock the guard's
// registry uses), so the sweep goroutine never races the spawn path.
func (s *Spawner) markSessionSilent(tickID string, pt partialTelemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.silenceTicks == nil {
		s.silenceTicks = make(map[string]partialTelemetry)
	}
	s.silenceTicks[tickID] = pt
}

// sessionSilenceFor reports (and clears) the watchdog verdict for one tick.
// ok=false when the watchdog never killed it.
func (s *Spawner) sessionSilenceFor(tickID string) (partialTelemetry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.silenceTicks[tickID]
	if ok {
		delete(s.silenceTicks, tickID)
	}
	return pt, ok
}

// ─────────────────────────────────────────────────────────────────────────────
// SCHED-GAP-1698 — poll-loop idle guard (a sibling of the silence watchdog).
//
// The silence watchdog above covers the NOT-TALKING-AT-ALL shape. This guard
// covers the shape that evades it and every liveness check downstream: a
// session that talks forever but NEVER ADVANCES — a sleep-poll loop. Measured
// 2026-10-02 (bunker-foreman tick, 3.00h to the tick wall): 62 of the
// session's 81 tool calls were the identical `sleep 165; ps --ppid …; judge
// --status` terminal command, cycling at ~167s for 2.9 hours. Every poll
// emits a stream event, so the gateway idle deadline resets forever and the
// fleet held the slot for a clock watcher. Fleet-wide 48h: 22 timeout ticks =
// 114.5 slot-hours, 320 `sleep N` polling calls.
//
// Detection contract (what makes a session a poller):
//   - The probe reads the session's tool-call timestamps from the Hermes
//     state store (messages of role='assistant' carrying tool_calls). The
//     raw per-message token_count column is NULL on every gateway-path
//     session ever sampled, and sessions.input_tokens EXCLUDES cache reads —
//     a sleep-poll turn RE-SENDS the whole (cached) transcript, so its
//     non-cache token burn is tiny while cache_read grows. There is no
//     per-call token figure a guard can trust; the poll CYCLES THEMSELVES
//     are the signal (measured: the incident's tokens-after-first-sleep
//     column reads 0 — the brief's "token delta ~zero" is exactly the
//     zero-marginal-work property the cycle filter asserts structurally).
//   - A STRIDE is a 5-minute window ending at the probe instant. A stride is
//     POLL-DOMINATED when it holds >= pollLoopMinTicks tool calls, every
//     gap between consecutive calls (and from the window start to the first
//     call) is <= pollLoopMaxGap, the calls are (near-)uniform, and NO call
//     in the window carries non-poll payloads. A poll cycle is a terminal
//     command led by `sleep N` (`sleep <d>; <status check>`), or a status
//     read-only probe (git status/log/diff, ps, judge --status, tail/cat/wc/
//     grep/ls, rb_totals/side-by-side) — anything else (or a non-terminal
//     tool, or a non-tool call) is REAL WORK and fails the stride.
//   - CADENCE ALIASING is load-bearing arithmetic: the measured poll cadence
//     (~165s) does not divide the 60s guard cadence, so per-60s-pass tool
//     deltas read 1,0,1,0 — "grew by exactly the same small pattern" is
//     FALSE for the real incident shape. The detector therefore compares
//     CONSECUTIVE 5-MINUTE STRIDES: two strides in a row whose tick counts,
//     periods, and poll purity agree (the same fixed-cadence cycle) are the
//     repeating pattern, which per-60s deltas can never express.
//   - A poll-loop verdict needs >= pollLoopConfirmPasses consecutive guard
//     passes (60s apart) each confirming BOTH the current stride and the
//     previous one — ≈15 minutes of confirmed fixed-cadence polling — AND
//     the tick at least pollLoopMinAge old (a session in its first half
//     hour is never judged: early work legitimately polls a long build).
//   - KILL flows through Spawner.CancelTickSession exactly like the silence
//     watchdog; the spawn path consumes the verdict, closes the row
//     status=timeout with failure_reason=sleep_poll (via HarnessFailure +
//     failureReasonClass — marker registered in failureclass.go, comment
//     naming SCHED-GAP-1698) and ticks.session_silence_s carrying the
//     confirmed polling span.
//   - NEVER kills a producing session: fail open on ANY unreadable or
//     uninterpretable telemetry, on any non-poll call in a stride, on gaps
//     past pollLoopMaxGap, and when the store never registered the session.
//   - NO BACKOFF CHANGE: cooldowns, consecutive_failures and the "No timeout
//     backoff" ruling are untouched — the row rides the existing
//     TickTimeout classification (the same harness carve-out as
//     session_silent: failureReasonClass non-empty leaves the counter
//     alone).
//   - OFF by default: sessionPollLoopGuard() returns 0 until armed
//     (SetSessionPollLoopMinTicks); embedding tests and unit suites keep
//     byte-identical behavior (the knob convention's library-default rule).
// ─────────────────────────────────────────────────────────────────────────────

// The poll-loop detector's shape constants. They are deliberately NOT
// config: the classification is a measured fleet fact (the 2026-10-02
// incident), not a tuning surface — the operator knob (the armed minimum)
// can only make the guard STRICTER, never looser than these floors.
const (
	// pollLoopWindow is the sliding observation window probed on every
	// guard pass. It must hold >= 4 cycles of the SLOWEST measured poll
	// cadence: the 2026-10-02 incident polled at BOTH ~165s (62 sleep-led
	// calls over 2.9h in the state.db timeline) and — the brief's own
	// shape — 390-480s (`sleep 390..480; gitreins judge --status` every
	// ~7 min for 2.5h). A 30-minute window holds ~11 cycles of the fast
	// cadence and 4-5 of the slow one; a 5-minute stride would hold 0-1
	// of the slow shape and never classify it.
	pollLoopWindow = 30 * time.Minute
	// pollLoopMinTicks is the BUILT-IN per-window floor (the armed knob
	// can only raise it). 4 = the slow measured cadence's worst-case
	// window fill; anything sparser than the measured pollers is already
	// inside the silence-watchdog's territory, not this guard's.
	pollLoopMinTicks = 4
	// pollLoopMaxGap is the largest gap between consecutive tool calls
	// (and around the window edges) a poll-dominated window may contain.
	// It covers the measured 480s sleep with margin; a gap beyond 10
	// minutes means the session was doing something else (a long compile,
	// a test battery inside ONE terminal call) — spare it. The trailing
	// edge uses half of this: a LIVE poller's last call sits within one
	// cycle of the probe instant, so a window whose calls all predate a
	// >= pollLoopMaxGap/2 silence is a session that HAS stopped.
	pollLoopMaxGap = 10 * time.Minute
	// pollLoopMinAge: a tick younger than this is NEVER judged — the
	// window itself needs 30 minutes of history, so early-session polling
	// of a genuinely running build is spared by construction.
	pollLoopMinAge = 30 * time.Minute
	// pollLoopConfirmPasses is the streak of consecutive guard passes
	// (60s apart) whose windows must all classify as poll-dominated
	// before the kill — the brief's ">=3 consecutive guard passes"
	// minimum, each confirming a sliding 30-minute window (~32 minutes
	// of sustained polling observed end to end).
	pollLoopConfirmPasses = 3
)

// SetSessionPollLoopMinTicks arms the poll-loop guard with the per-stride
// tool-call minimum (0 = the guard is off — the library default). Called
// once from main/loop wiring; last writer wins. Values below
// pollLoopMinTicks are clamped up to the built-in floor: the knob makes the
// guard stricter, never looser.
func SetSessionPollLoopMinTicks(n int) {
	sessionSilenceMu.Lock()
	defer sessionSilenceMu.Unlock()
	if n > 0 && n < pollLoopMinTicks {
		n = pollLoopMinTicks
	}
	sessionPollLoopMinTicksN = n
}

// sessionPollLoopGuard returns the armed per-stride minimum (0 = disabled).
func sessionPollLoopGuard() int {
	sessionSilenceMu.RLock()
	defer sessionSilenceMu.RUnlock()
	return sessionPollLoopMinTicksN
}

// sessionPollLoopMinTicksN is the armed knob; guarded by sessionSilenceMu
// (the same lock the silence grace uses — one knob mutex for the sibling
// guards keeps the lock surface single).
var sessionPollLoopMinTicksN int

// pollLoopTimeBounds returns [start, end] of one probe window ending at
// now. Cheap arithmetic seam kept as a function for the tests.
func pollLoopTimeBounds(now time.Time) (time.Time, time.Time) {
	return now.Add(-pollLoopWindow), now
}

// pollSpan is one tool call's probe shape on the session timeline: the call
// instant (start==end; the guard classifies by INSTANT, not span) and the
// payload verdict. poll=true when the payload is a sleep-led terminal
// command or a read-only status probe; false = real work.
type pollSpan struct {
	start float64
	end   float64
	// poll is true when the payload is a sleep-led terminal command or a
	// read-only status probe; false = real work.
	poll bool
}

// pollSpanFromHistory builds the probe shape from one pollHistoryRow.
func pollSpanFromHistory(r pollHistoryRow) pollSpan {
	return pollSpan{start: r.started, end: r.started, poll: r.isPoll}
}

// isPollLoopWindow decides whether [start,end] is POLL-DOMINATED.
// history (ordered by start) covers the window; an unmeasurable window
// fails open (false) — classification never rests on missing telemetry.
func isPollLoopWindow(minTicks int, start, end float64, history []pollHistoryRow) bool {
	if len(history) == 0 {
		return false // no readable timeline: never classify (fail open)
	}
	if len(history) < minTicks {
		return false
	}
	// Window must be fully covered: the earliest call no earlier than the
	// window start and the latest no later than its end. A probe that
	// returned rows outside the requested bounds is unmeasurable — fail
	// open. (Trailing silence is checked separately: a LIVE poller's last
	// call sits within one cycle of the probe instant; a window whose calls
	// all predate pollLoopMaxGap/2 of silence is a session that HAS
	// stopped, never a kill candidate.)
	if history[0].started < start-0.001 || history[len(history)-1].started > end+0.001 {
		return false
	}
	var (
		spans    = make([]pollSpan, 0, len(history))
		workVote int
	)
	for _, r := range history {
		if r.started < start-0.001 || r.started > end+0.001 {
			return false
		}
		sp := pollSpanFromHistory(r)
		if !sp.poll {
			workVote++ // any real work in the window → the window is productive
		}
		spans = append(spans, sp)
	}
	if workVote > 0 {
		return false
	}
	// Uniformity: gaps between consecutive calls, plus the leading gap from
	// the window start to the first call, must fit a fixed-cadence cycle.
	lead := spans[0].start - start
	if lead > pollLoopMaxGap.Seconds() {
		return false
	}
	// Trailing gap: a LIVE poller's last call sits within one cycle period
	// of the probe instant. A window whose calls all predate a long silence
	// is a session that HAS stopped (or slowed) — never a kill candidate.
	trail := end - spans[len(spans)-1].start
	if trail > pollLoopMaxGap.Seconds()/2 {
		return false
	}
	var sum, sumSq float64
	gaps := make([]float64, 0, len(spans))
	for i := 1; i < len(spans); i++ {
		g := spans[i].start - spans[i-1].start
		if g <= 0 || g > pollLoopMaxGap.Seconds() {
			return false
		}
		gaps = append(gaps, g)
		sum += g
		sumSq += g * g
	}
	if len(gaps) == 0 {
		return false // a single call is not a "loop"
	}
	n := float64(len(gaps))
	mean := sum / n
	variance := sumSq/n - mean*mean
	if mean <= 0 {
		return false
	}
	// The cycle period must sit INSIDE the window (a window holding one
	// >30-minute cycle is not a repeating pattern within the window), and
	// the spread must stay tight (sigma <= mean/4 — the near-uniform sample
	// the fixed-cadence claim needs; measured incident sigma ≈ 1s on ≈167s,
	// and the 390-480s arm's jitter is ~5% of period — well inside).
	if mean >= pollLoopWindow.Seconds() {
		return false
	}
	if variance < 0 {
		variance = 0
	}
	if math.Sqrt(variance/n) > mean/4 {
		return false
	}
	return true
}

// pollLoopSample is one pass's observation for one tick: whether the
// sliding window ending at the pass instant classified as poll-dominated.
type pollLoopSample struct {
	window bool
	valid  bool // false when the window was unmeasurable (fail open)
}

// pollLoopTracker holds the per-tick pass history (streak of consecutive
// fully-confirming samples) and fires when the streak reaches
// pollLoopConfirmPasses. History is rebuilt per pass from the pass's
// candidate set (retain), so finished ticks cannot leak entries.
type pollLoopTracker struct {
	hist map[string][]pollLoopSample
}

// retain keeps history only for the tick ids in the current candidate set.
func (pt *pollLoopTracker) retain(ids map[string]bool) {
	if pt.hist == nil {
		pt.hist = make(map[string][]pollLoopSample)
		return
	}
	for id := range pt.hist {
		if !ids[id] {
			delete(pt.hist, id)
		}
	}
}

// sample records one pass's observation and reports whether the confirmation
// streak reached the threshold. A non-confirming observation resets the
// streak (the shape must be CONTINUOUS); an invalid one (unreadable
// telemetry) also resets — the verdict must never rest on a gap.
func (pt *pollLoopTracker) sample(tickID string, s pollLoopSample, confirm int) bool {
	if pt.hist == nil {
		pt.hist = make(map[string][]pollLoopSample)
	}
	if !s.valid || !s.window {
		delete(pt.hist, tickID)
		return false
	}
	pt.hist[tickID] = append(pt.hist[tickID], s)
	if len(pt.hist[tickID]) > confirm {
		pt.hist[tickID] = pt.hist[tickID][len(pt.hist[tickID])-confirm:]
	}
	return len(pt.hist[tickID]) == confirm
}

// probeSessionToolTimeline reads one tick's recent tool-call history from the
// agent state store (read-only, bounded). ok=false on ANY failure or empty
// timeline: the guard never acts on an unreadable store. window limits the
// scan to the two strides the verdict needs (the trailing 10 minutes).
func probeSessionToolTimeline(tickID string, windowEnd time.Time) (rows []pollHistoryRow, ok bool) {
	path := hermesStateDBPath()
	if path == "" || tickID == "" {
		return nil, false
	}
	sdb, err := database.OpenHermesStateReadOnly(path)
	if err != nil {
		return nil, false
	}
	defer sdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hermesGuardQueryTimeout)
	defer cancel()
	windowStart := windowEnd.Add(-pollLoopWindow)
	since := float64(windowStart.UnixNano()) / 1e9
	until := float64(windowEnd.UnixNano()) / 1e9
	// Same lookup shape as probeSessionActivity: the gateway names the
	// session with the tick id (session_key). Only ASSISTANT messages
	// carrying tool_calls are timeline points; the JSON extraction is
	// tolerant (a malformed payload classifies as non-poll below, never as
	// a query failure).
	r, err := sdb.QueryContext(ctx, `
SELECT m.timestamp,
       CASE WHEN json_valid(m.tool_calls)
            THEN COALESCE(json_extract(json_extract(m.tool_calls, '$[0]'), '$.function.arguments'), '')
            ELSE '' END,
       COALESCE(json_extract(json_extract(m.tool_calls, '$[0]'), '$.function.name'), '')
  FROM sessions s
  JOIN messages m ON m.session_id = s.id
 WHERE s.session_key = ? AND m.role = 'assistant' AND m.tool_calls IS NOT NULL
   AND m.timestamp >= ? AND m.timestamp <= ?
 ORDER BY m.timestamp ASC`, tickID, since, until)
	if err != nil {
		log.Printf("WARN: poll-loop probe for tick %s: %v (observe nothing)", tickID, err)
		return nil, false
	}
	defer r.Close()
	for r.Next() {
		var row pollHistoryRow
		var args, toolName string
		if err := r.Scan(&row.started, &args, &toolName); err != nil {
			return nil, false
		}
		row.isPoll = isPollCallPayload(toolName, args)
		rows = append(rows, row)
	}
	if err := r.Err(); err != nil {
		return nil, false
	}
	return rows, len(rows) > 0
}

// pollHistoryRow is one timeline point.
type pollHistoryRow struct {
	started float64
	isPoll  bool
}

// classifyPollLoopWindow probes the sliding window ending at now for one
// tick.
func classifyPollLoopWindow(minTicks int, probe func(windowEnd time.Time) ([]pollHistoryRow, bool), now time.Time) pollLoopSample {
	var s pollLoopSample
	start, _ := pollLoopTimeBounds(now)
	rows, ok := probe(now)
	s.window = ok && isPollLoopWindow(minTicks, float64(start.UnixNano())/1e9, float64(now.UnixNano())/1e9, rows)
	s.valid = ok
	return s
}

// sessionPollLoopPass is one guard sweep over the running gateway ticks.
// Called from the Loop's 60s reaper tick, AFTER the silence watchdog.
// Returns the number of poll-looping sessions terminated. Best-effort by
// contract: every failure inside is logged and can never touch the reaper's
// own semantics. now is the component clock's instant (simulator-driven in
// tests); the armed minimum 0 (guard off) makes the pass a no-op.
func (l *Loop) sessionPollLoopPass(now time.Time) int {
	minTicks := sessionPollLoopGuard()
	if minTicks <= 0 || l.spawner == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := l.db.QueryContext(ctx, `
SELECT t.id, t.project_name, COALESCE(t.spawned_at,'')
FROM ticks t
WHERE t.status = 'running' AND t.pid = 0`)
	if err != nil {
		log.Printf("WARN: session_poll_loop running-tick query failed: %v", err)
		return 0
	}
	type cand struct {
		id        string
		project   string
		spawnedAt string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.project, &c.spawnedAt); err != nil {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if len(cands) == 0 {
		return 0
	}
	keep := make(map[string]bool, len(cands))
	for _, c := range cands {
		keep[c.id] = true
	}
	l.pollLoopHist.retain(keep)

	killed := 0
	for _, c := range cands {
		spawned, perr := time.Parse(time.RFC3339, c.spawnedAt)
		if perr != nil || spawned.IsZero() {
			continue // no usable clock — never act on an unmeasurable tick
		}
		if age := now.Sub(spawned); age < pollLoopMinAge {
			continue // early-session polling of a running build is legit
		}
		sample := classifyPollLoopWindow(minTicks, func(windowEnd time.Time) ([]pollHistoryRow, bool) {
			return probeSessionToolTimeline(c.id, windowEnd)
		}, now)
		if !l.pollLoopHist.sample(c.id, sample, pollLoopConfirmPasses) {
			continue
		}
		// Confirmed: the tick has shown a sustained, fixed-cadence,
		// zero-advancement poll window for pollLoopConfirmPasses
		// consecutive passes. Kill through the same cancel registry the
		// silence watchdog uses.
		span := pollLoopWindow
		if !l.spawner.CancelTickSession(c.id) {
			log.Printf("POLL-LOOP: %s tick=%s confirmed poll-looping but no live registered session to cancel — skipping",
				c.project, c.id)
			delete(l.pollLoopHist.hist, c.id)
			continue
		}
		l.spawner.markSessionPollLoop(c.id, markPollLoopTelemetry(0, 0, 0, int64(span/time.Second)))
		log.Printf("POLL-LOOP: %s tick=%s terminated after %v of confirmed sleep-poll cycling (stride min %d, confirm %d passes)",
			c.project, c.id, span.Round(time.Second), minTicks, pollLoopConfirmPasses)
		delete(l.pollLoopHist.hist, c.id)
		killed++
	}
	return killed
}

// markSessionPollLoop records one poll-loop-guard kill on the spawner's
// consume-once registry. Guarded by the spawner's own mutex (the same lock
// the silence registry uses), so the sweep goroutine never races the spawn
// path.
func (s *Spawner) markSessionPollLoop(tickID string, pt partialTelemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pollLoopTicks == nil {
		s.pollLoopTicks = make(map[string]partialTelemetry)
	}
	s.pollLoopTicks[tickID] = pt
}

// sessionPollLoopFor reports (and clears) the poll-loop verdict for one tick.
// ok=false when the guard never killed it.
func (s *Spawner) sessionPollLoopFor(tickID string) (partialTelemetry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.pollLoopTicks[tickID]
	if ok {
		delete(s.pollLoopTicks, tickID)
	}
	return pt, ok
}

// pollReadonlyRe is the read-only status-probe allowlist for terminal
// commands without a leading `sleep`. All matches are case-insensitive
// prefix/word matches against the command's first token(s) — anything NOT in
// this list is real work by definition (a mutating command, a build, a test
// battery, a git commit). This is deliberately the WORST-case reading: when
// in doubt, the call is work and the stride fails open.
var pollReadonlyRe = regexp.MustCompile(
	`(?i)^\s*(git\s+(status|log|diff|show|branch|stash\s+list)|ps\b|tail\b|cat\b|wc\b|grep\b|ls\b|echo\b|printf\b|` +
		`gitreins\s+judge\s+--status|judge\s+--status|rb_totals|side-by-side)`)

// pollArgsInvalid reports whether a tool_calls function.arguments
// extraction hit a shape the classifier must NOT read as either poll or
// work: an empty or non-object payload (the SQL json_extract already
// proved the outer value is an array with a function member; the ARGUMENTS
// member itself must decode to a JSON object — every gateway sampled
// serializes it as an object, string-encoded or not). These are fail-open
// (the stride is not classifiable), never poll.
func pollArgsInvalid(rawArgs string) bool {
	trimmed := strings.TrimSpace(rawArgs)
	if trimmed == "" || trimmed[0] != '{' {
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return true
	}
	return false
}

// isPollCallPayload classifies one tool call. A poll cycle is a TERMINAL
// tool call whose command is led by `sleep` (the sleep+status-check cycle —
// `sleep 165; ps --ppid …; gitreins judge --status` is the measured incident
// shape), or a read-only status probe (the allowlist above). Anything else —
// including a non-terminal tool (browser/apply_patch/write), a non-JSON
// tool_calls payload (fail-open), or a command the allowlist does not name —
// is REAL WORK and fails the stride.
func isPollCallPayload(toolName, rawArgs string) bool {
	if strings.ToLower(strings.TrimSpace(toolName)) != "terminal" {
		return false
	}
	if pollArgsInvalid(rawArgs) {
		return false
	}
	cmd, err := jsonExtractString(rawArgs, "command")
	if err != nil || strings.TrimSpace(cmd) == "" {
		return false
	}
	body := strings.TrimSpace(cmd)
	lower := strings.ToLower(body)
	if strings.HasPrefix(lower, "sleep ") || strings.HasPrefix(lower, "sleep	") {
		return true
	}
	return pollReadonlyRe.MatchString(body)
}

// jsonExtractString is the minimal string-field extractor the payload
// classifier needs: it unmarshals the function.arguments JSON object (or any
// JSON object) and returns the named string field. It deliberately does NOT
// reach for encoding/json's structured tool-call types — the arguments blob
// varies by gateway, and only `command` is load-bearing here.
func jsonExtractString(rawJSON, field string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawJSON), &obj); err != nil {
		return "", err
	}
	v, ok := obj[field]
	if !ok {
		return "", fmt.Errorf("field %q absent", field)
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", err
	}
	return s, nil
}
