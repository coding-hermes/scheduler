package scheduler

import (
	"context"
	"database/sql"
	"log"
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
