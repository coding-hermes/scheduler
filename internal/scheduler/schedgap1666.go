package scheduler

// SCHED-GAP-1666 — ONE shared effective-cooldown admission predicate for every
// entry point into the slot pool.
//
// THE DEFECT. SlotPool.spawn (slot_pool.go) gates dedup (tryReserve) and the
// namespace cap (SCHED-GAP-144) itself, and — since SCHED-GAP-171 hoisted the
// load gate OUT of spawn() — relies on its CALLERS for the load gate. The
// same is true of the per-lane effective cooldown: the packer applies it
// inside its selection gates (packer.go / packer_select.go /
// multipool_packer.go), but a NUDGE-sourced spawn (startup resume, board
// wake, wave/queue replay) enters straight through
// SlotPool.SpawnEnqueued/Spawn without ever re-deriving it. Measured
// consequence: 402 dogfood/perf/docs/readme ticks in 7 days, 123 of them
// nudge-sourced — admissions a lane's own cooldown would have refused.
//
// THE SHAPE. Exactly the shape SCHED-GAP-171 used for the load gate: the
// decision lives in ONE place and every caller consults it BEFORE it hands
// the pool a tick id (never inside spawn(), where a deferral would strand an
// already-enqueued row — see the SCHED-GAP-171 doc block in slot_pool.go).
//
//	effectiveCooldownGate(CooldownGateRequest{...}, now) CooldownGateDecision
//
// Callers (a change to this list is a change to the admission surface, and
// TestSCHEDGAP1666_GateConsultsAllEntryPoints fails when a new entry point is
// added without one):
//
//	packer.go              effectiveCooldownDur → the greedy pack + isOverdue
//	packer_select.go       the namespace-path selection gate + queued mirror
//	multipool_packer.go    packFlat (the flat fallback path)
//	loop.go                countEligibleProjects (the GAP-043 eligibility mirror)
//	loop.go                SpawnNow (the operator/API entry point)
//	session_resume.go      resumeOrphans (the boot / reconnect resume scan)
//	board_wake.go          the board-wake entry point
//
// BYPASSES. A lane may be admitted AHEAD of its effective cooldown only under
// one of the sanctioned reasons below, and the predicate validates each
// claim against its entitlement before granting it — an entry point cannot
// mint a bypass it is not entitled to:
//
//	manual                 (e) the operator's explicit spawn; always logged.
//	continuity:in_flight   (c) STARTUP RESUME of a tick the previous instance
//	                           left running/queued — a restart is a continuity
//	                           event, never a cadence reset.
//	flip:board_empty       (b) a board write flipping a PARKED-EMPTY
//	                           tasks-admission lane back to admission
//	                           (SCHED-GAP-1660's one allowed board effect).
//
// The tasks-admission WAIVER (SCHED-GAP-124 — a tasks lane whose board holds
// admissible non-perpetual work admits immediately) is modelled here too, so
// a non-packer entry point reaches the identical answer the packer would.
// Failed / repeatedly-failing tasks lanes pace on their full effective
// cooldown (SCHED-GAP-214 / SCHED-GAP-133), exactly as the packer's tasks
// branch does.
//
// FAIL-OPEN: an unreadable project row (or a nil DB) leaves the caller no
// facts to judge with; the gate then reports ok=false and the caller keeps
// its historical behaviour, loudly logged. A telemetry/read fault must never
// freeze orphan recovery or the operator endpoint — the same fail-open
// doctrine the load gate and the board scanner already follow.

import (
	"database/sql"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// CooldownGateBypass names the ONLY sanctioned reasons a lane may be admitted
// ahead of its effective cooldown. The empty value means "no bypass claimed" —
// the caller wants the plain wall-clock decision.
type CooldownGateBypass string

const (
	// CooldownBypassNone is the plain gate: admit only once the lane's
	// effective cooldown has elapsed. Every packer path uses this.
	CooldownBypassNone CooldownGateBypass = ""
	// CooldownBypassManual (e) is the operator's explicit spawn
	// (Loop.SpawnNow / the API spawn endpoint). It is the one bypass that is
	// always granted — and it is always logged, so an operator-driven
	// admission can never hide behind an on-schedule "ok".
	CooldownBypassManual CooldownGateBypass = "manual"
	// CooldownBypassInFlightContinuity (c) is the startup-resume bypass for a
	// tick the PREVIOUS instance left running/queued. The claim is valid only
	// when the orphan row carries an in-flight drop reason
	// (isInFlightInterruption) — a row with no such mark is not evidence of an
	// interrupted tick and is never admitted ahead of its lane's cooldown.
	CooldownBypassInFlightContinuity CooldownGateBypass = "continuity:in_flight"
	// CooldownBypassTasksParkedFlip (b) is the board-wake bypass: the single
	// effect a board write is allowed to have on a cooldown (SCHED-GAP-1660).
	// It is granted only for a TASKS-admission lane whose park mark is set —
	// the mark exists solely because a previous pass deferred the lane on an
	// empty (perpetual-free) board. The stamp the pool records for such an
	// admission is AdmissionReasonFlipBoardEmpty, never a generic resume
	// label.
	CooldownBypassTasksParkedFlip CooldownGateBypass = "tasks:parked_empty_flip"
)

// CooldownGateRequest is every fact the shared predicate needs about one lane
// at one instant. It is deliberately a plain value: the packers fill it from
// their in-memory scored rows, the non-packer callers fill it from the
// project row, and both reach the identical decision.
type CooldownGateRequest struct {
	// Project is the lane name (observability only; the predicate never keys
	// off it).
	Project string
	// AdmissionMode is the EFFECTIVE mode (project override → namespace
	// default → cooldown), already resolved by the caller.
	AdmissionMode string
	// CooldownS is the RAW cooldown the shared effectiveCooldown() starts
	// from — the packers fold an active bump in before building this
	// request, exactly as they always have.
	CooldownS int
	// CooldownPinS is the operator pin (nil / non-positive = no pin). It
	// outranks every other base (SCHED-GAP-1661).
	CooldownPinS *int
	// Priority feeds the dynamic-interval branch when CooldownS == 0.
	Priority float64
	// ConsecutiveFailures drives the S-GAP-001 failure backoff.
	ConsecutiveFailures int
	// LastTickStatus is the terminal status of the most recent tick ("" =
	// never); it stands the tasks waiver down after a FAILED tick
	// (SCHED-GAP-214).
	LastTickStatus string
	// LastCompleted is the lane's last terminal tick instant. nil / zero
	// means "never completed": nothing to pace against, so the lane is
	// admitted.
	LastCompleted *time.Time
	// Workdir / BoardOwnership are the inputs of the SCHED-GAP-141 board
	// ownership walk.
	Workdir        string
	BoardOwnership string
	// BlackoutWindows and Calculator are the config inputs of the shared
	// effectiveCooldown() arithmetic.
	BlackoutWindows []config.BlackoutWindow
	Calculator      *UrgencyCalculator

	// Bypass is the caller's CLAIMED sanctioned reason ("" = none). The
	// predicate validates it — a claim without its entitlement is refused.
	Bypass CooldownGateBypass
	// InFlightContinuity is the entitlement for
	// CooldownBypassInFlightContinuity: the row being resumed was left
	// running/queued by the previous instance (an in-flight drop mark).
	InFlightContinuity bool
	// ParkedEmpty is the entitlement for CooldownBypassTasksParkedFlip: the
	// lane carries the SCHED-GAP-1660 park mark.
	ParkedEmpty bool
	// TasksWork is the SCHED-GAP-124 waiver signal: a tasks-admission lane
	// whose board currently holds admissible (non-perpetual) work.
	TasksWork bool
}

// CooldownGateDecision is the shared predicate's answer.
type CooldownGateDecision struct {
	// Defer is the verdict: true = the caller must NOT hand this tick to the
	// pool (the lane is inside its effective cooldown and no sanctioned
	// bypass applies).
	Defer bool
	// Cooldown is the lane's effective cooldown at this instant.
	Cooldown time.Duration
	// EffectiveS is Cooldown in seconds (float, for the log/observation
	// surfaces that already speak float seconds).
	EffectiveS float64
	// RemainingS is how much of the cooldown was left (0 when elapsed).
	RemainingS float64
	// SkipMode is the shared effectiveCooldown()'s blackout skip flag: the
	// lane must not spawn AT ALL inside the window, whatever the bypass.
	SkipMode bool
	// Mode is the resolved admission mode the decision was taken under.
	Mode string
	// Bypass is the sanctioned reason the lane was admitted ahead of its
	// cooldown ("" when none was needed or none was granted).
	Bypass CooldownGateBypass
	// Reason is the admit_reason family the admission belongs to: the tick
	// row's own vocabulary (ok / resume:<source> / flip:board_empty /
	// cooldown / failed_cooldown).
	Reason string
}

// effectiveCooldownGate is the ONE shared admission predicate. See the file
// header for the contract and the caller list.
func effectiveCooldownGate(req CooldownGateRequest, now time.Time) CooldownGateDecision {
	dec := CooldownGateDecision{Mode: req.AdmissionMode}
	dur, skipMode := effectiveCooldown(req.CooldownS, req.Priority, req.ConsecutiveFailures,
		req.BlackoutWindows, now, req.Calculator, req.CooldownPinS)
	dec.Cooldown = dur
	dec.EffectiveS = dur.Seconds()
	dec.SkipMode = skipMode
	if skipMode {
		// A skip-mode blackout refuses to spawn at all: no bypass mints work
		// inside a window the operator marked "do not run".
		dec.Defer = true
		dec.Reason = AdmissionReasonCooldown
		return dec
	}
	if req.LastCompleted == nil || req.LastCompleted.IsZero() {
		// Never completed: nothing to pace against.
		dec.Reason = AdmissionReasonOK
		return dec
	}
	remaining := dur.Seconds() - now.Sub(*req.LastCompleted).Seconds()
	if remaining <= 0 {
		// On schedule — the ordinary admission.
		dec.Reason = AdmissionReasonOK
		return dec
	}
	dec.RemainingS = remaining

	// 1. The board-write entry's ONLY sanctioned bypass (deliverable (b)).
	// A board write may act on a lane in exactly one way: admitting a
	// TASKS-admission lane whose board holds admissible non-perpetual work —
	// the SCHED-GAP-124 waiver reached through the board-wake entry, with
	// the SCHED-GAP-1660 park mark scoping the flip LABEL (the slot pool
	// consumes the mark). A claim the ruling does not sanction is REFUSED
	// outright rather than falling through to the generic decision: a board
	// write on a parked-but-workless lane must stamp nothing at all, or the
	// process-wide board_wake stamp would leak onto whichever lane admits
	// next and label a cadence-legitimate tick as a wake admission.
	if req.Bypass == CooldownBypassTasksParkedFlip {
		if req.AdmissionMode != database.AdmissionModeTasks || !req.TasksWork {
			dec.Defer = true
			dec.Reason = AdmissionReasonCooldown
			return dec
		}
		if lastTickStatusFailed(req.LastTickStatus) || req.ConsecutiveFailures > 1 {
			dec.Defer = true
			dec.Reason = AdmissionReasonFailedCooldown
			return dec
		}
		dec.Bypass = CooldownBypassTasksParkedFlip
		if req.ParkedEmpty {
			dec.Reason = AdmissionReasonFlipBoardEmpty
		} else {
			dec.Reason = AdmissionReasonOK
		}
		return dec
	}

	// 2. The tasks-admission waiver (SCHED-GAP-124) for every other entry
	// point. A tasks lane whose board holds admissible non-perpetual work is
	// admitted immediately — that IS its cooldown semantics, not a bypass. A
	// FAILED last tick (SCHED-GAP-214) or repeated failures (SCHED-GAP-133)
	// stand the waiver down: the lane paces on the full effective cooldown.
	if req.AdmissionMode == database.AdmissionModeTasks && req.TasksWork {
		if lastTickStatusFailed(req.LastTickStatus) || req.ConsecutiveFailures > 1 {
			dec.Defer = true
			dec.Reason = AdmissionReasonFailedCooldown
			return dec
		}
		dec.Reason = AdmissionReasonOK
		return dec
	}

	// 3. The remaining sanctioned bypasses — each validated against its
	// entitlement.
	switch req.Bypass {
	case CooldownBypassManual:
		// (e) the operator's explicit spawn. Always granted, always logged by
		// the caller: a manual admission ahead of a cooldown is a decision,
		// not an accident.
		dec.Bypass = CooldownBypassManual
		dec.Reason = "resume:" + NudgeSourceManual
		return dec
	case CooldownBypassInFlightContinuity:
		// (c) continuity: only a tick the previous instance left
		// running/queued is resumed ahead of the cooldown. Without the
		// in-flight mark the claim is refused and the lane waits out its
		// cooldown like any other nudge — a restart is never a cadence reset.
		if req.InFlightContinuity {
			dec.Bypass = CooldownBypassInFlightContinuity
			dec.Reason = "resume:" + NudgeSourceStartup
			return dec
		}
	}

	dec.Defer = true
	dec.Reason = AdmissionReasonCooldown
	return dec
}

// cooldownGateCall names one non-packer entry point's request: which lane,
// which sanctioned bypass it claims, and the entitlements it can prove.
type cooldownGateCall struct {
	Project            string
	Bypass             CooldownGateBypass
	InFlightContinuity bool
	ParkedEmpty        bool
}

// effectiveCooldownGate is the Loop-level use of the shared predicate for the
// non-packer entry points: it loads the lane's facts from the project row
// (the same columns the packer's SQL-side mirror reads), computes the
// SCHED-GAP-124 waiver signal, and answers. ok=false means the facts could
// not be read (unknown project, nil DB, query error) — the caller keeps its
// historical behaviour and logs loudly (fail-open; see the file header).
func (l *Loop) effectiveCooldownGate(call cooldownGateCall) (CooldownGateDecision, bool) {
	req, ok := l.cooldownGateRequestFor(call)
	if !ok {
		return CooldownGateDecision{}, false
	}
	return effectiveCooldownGate(req, l.clock().Now()), true
}

// cooldownGateRequestFor builds the shared-predicate request for a lane
// straight from its project row.
func (l *Loop) cooldownGateRequestFor(call cooldownGateCall) (CooldownGateRequest, bool) {
	req := CooldownGateRequest{Project: call.Project, Bypass: call.Bypass,
		InFlightContinuity: call.InFlightContinuity, ParkedEmpty: call.ParkedEmpty}
	if l.db == nil || call.Project == "" {
		return req, false
	}
	var pin int
	var lastComp string
	var projMode, nsMode string
	err := l.db.QueryRow(`
SELECT cooldown_s, COALESCE(cooldown_pin_s, 0), priority,
       COALESCE(consecutive_failures, 0), COALESCE(last_tick_completed, ''),
       COALESCE(admission_mode, ''),
       COALESCE((SELECT admission_mode FROM namespaces ns WHERE ns.id = projects.namespace_id), ''),
       COALESCE(workdir, ''), COALESCE(board_ownership, ''), COALESCE(last_tick_status, '')
  FROM projects WHERE name = ?`, call.Project).Scan(
		&req.CooldownS, &pin, &req.Priority, &req.ConsecutiveFailures, &lastComp,
		&projMode, &nsMode, &req.Workdir, &req.BoardOwnership, &req.LastTickStatus)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("COOLDOWN-GATE: project row %s unreadable (%v) — falling open to the caller's own decision", call.Project, err)
		}
		return req, false
	}
	if pin > 0 {
		p := pin
		req.CooldownPinS = &p
	}
	if lastComp != "" {
		if t, perr := time.Parse(time.RFC3339, lastComp); perr == nil {
			req.LastCompleted = &t
		}
	}
	req.AdmissionMode = admissionModeFor(projMode, "", map[string]string{"": nsMode})
	// SCHED-GAP-141: an unowned/foreign board refuses the tasks waiver, so a
	// lane that only READS another project's board stays time-based.
	req.TasksWork = tasksAdmissionDue(req.Workdir, req.BoardOwnership)
	if l.packer != nil {
		req.BlackoutWindows = l.packer.blackoutWindows
	}
	req.Calculator = l.calculator
	return req, true
}
