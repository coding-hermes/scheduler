package scheduler

// SCHED-GAP-1688: the end-of-tick hook, owned by the scheduler.
//
// The hook belongs IN THE SCHEDULER, not in a borrower script: the scheduler
// already knows the tick produced zero artifacts, and it holds the gateway
// session id, so it can fire ONE extra turn into the SAME live session before
// that session closes. The session gets a chance to explain itself while it
// still has its own context, instead of a post-hoc script guessing from git
// status. The summariser (~/.hermes/scripts/duckbrain-session-summary.sh) is
// what the lane CALLS from inside that turn; this file is the trigger and the
// in-session instruction.
//
// OFF by default: it ships dark until it is proven on one family, so the no-op
// rate can be measured before and after.

import (
	"os"
	"strings"
	"time"
)

// idleInterventionTimeout bounds the single follow-up turn. Deliberately short
// — the session is past its main work, and this turn exists to force a
// decision (do it, or say why not), not to start a second full tick.
const idleInterventionTimeout = 5 * time.Minute

// envIdleIntervention is the SCHEDULER_* switch for the end-of-tick hook.
const envIdleIntervention = "SCHEDULER_IDLE_INTERVENTION"

// idleInterventionFromEnv resolves the global switch. False unless the env var
// is explicitly truthy — the hook ships OFF until proven on one family.
func idleInterventionFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envIdleIntervention))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// idleInterventionPrompt is the ONE turn fired into a zero-artifact session.
// It is the intervention prompt as written on the SCHED-GAP-1688 row: the
// lane must either do the work now, or leave a written, stored reason.
func idleInterventionPrompt(lane, tickID, workdir string) string {
	return "END-OF-TICK HOOK. This tick produced no commits and no board rows. " +
		"Workdir: " + workdir + ". " +
		"Either do the work now, or — if there is genuinely nothing dispatchable — write why: " +
		"what you looked at, what you chose not to do and the honest reason, and what blocked you. " +
		"Store that record with ~/.hermes/scripts/duckbrain-session-summary.sh --lane " + lane +
		" --tick " + tickID + " --kind noop. " +
		"A record explaining an idle tick is a GOOD outcome; a silent one is not."
}

// resolveIdleIntervention (SCHED-GAP-1688 AC5) resolves the end-of-tick hook's
// switch for one lane: the lane's own setting wins, else its namespace's, else
// the global (env) default. Tri-state at each level: -1 = inherit, 0 = off,
// 1 = on. An explicit OFF anywhere above the global default is honoured — a
// namespace can hold a whole family dark even when the global default is on.
func (s *Spawner) resolveIdleIntervention(p PackedProject) bool {
	if p.IdleIntervention == 0 {
		return false
	}
	if p.IdleIntervention == 1 {
		return true
	}
	if p.NamespaceIdleIntervention == 0 {
		return false
	}
	if p.NamespaceIdleIntervention == 1 {
		return true
	}
	return s.idleIntervention
}
