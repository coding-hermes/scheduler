package scheduler

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1682 — no-op by lane class: satellites MAY no-op, foremen may NOT.
//
// Bane 2026-09-30 (the row's verbatim intent): "a no-op tick is not one global
// rule — it depends on the LANE CLASS. For a tick that we need a flag for if
// noop tick is allowed for satellites it is. For foreman not. We have a trigger
// that checks the commit so branch before and after the foreman runs ... The
// example is that we can then use same session and say you did no work please
// commit a report why you did nothing. Or try again. That way we get work and
// if we don't get work we are told why we didn't."
//
// The TRIGGER: the (HEAD commit sha, branch) pair of the lane's workdir is
// captured BEFORE the foreman runs (in Spawn, right before the gateway POST)
// and again AFTER the tick finishes (in the gateway completion branch of
// Wait's producer, while the session channel still exists), and BOTH
// measurements are stored on the tick row (ticks.pre_commit/pre_branch/
// pre_board, post_commit/post_branch — migration v68) so the verdict is
// auditable in SQL: an operator reads the row and sees the evidence, not
// just the conclusion.
//
// The VERDICT (noopVerdict): a tick is a NO-OP iff both legs are UNCHANGED
// (pre==post commit AND pre==post branch) AND the pair was actually measured
// (the pre legs non-empty) AND the board fingerprint did not move (Bane's
// third leg). An empty pre capture is "unmeasured" — no workdir (remote
// lanes), or a repo that was unreadable at capture time — and can never
// produce a no-op verdict; an evidence gap must never re-enter a session.
//
// The CLASS: the verdict is judged per lane class. A SATELLITE (noop allowed
// by derivation) closes a no-op tick CLEAN — its product is a report / a
// DuckBrain key / a filed row, not a commit (SCHED-GAP-177: "the satellites
// don't always need to commit"). A FOREMAN (not allowed by derivation) whose
// verdict is no-op gets ticks.noop_flag=1 and the re-entry: the SAME gateway
// session receives "You did no work this tick. Either commit real work now,
// or commit a written report explaining exactly why you did nothing." — the
// retry-with-instruction that yields either work or a reason, never a silent
// zero.
//
// The OVERRIDE: projects.noop_allowed (fleet.toml `noop_allowed` under the
// project table, or the API PUT) wins over the derived class in BOTH
// directions — false on a satellite arms re-entry there, true on a foreman
// closes its no-op ticks clean. NULL (the default on every pre-existing row)
// = derive from the lane class (ClassifyLane, SCHED-GAP-1696 — the same
// derivation the admission law and the boundary guard use, so the surfaces
// can never disagree).
//
// The CHANNEL: the re-entry fires through the EXISTING gateway client path —
// the same SendResponseStream call the SCHED-GAP-1688 idle-intervention hook
// uses, carrying the tick id as X-Hermes-Session-Key so the gateway
// re-attaches the SAME session. When there is no session channel (no gateway
// client, or the context is already gone — exec spawns, remote dispatch), the
// scheduler EVENT is emitted instead (noop_reentry_unavailable) — the miss is
// never fabricated into a delivery.
//
// The EVENTS: every verdict emits through the events table under the
// "noop_guard" component — noop_detected (enforcement verdict),
// noop_allowed (satellite/override-permitted verdict), reentry_requested
// (the in-session instruction was dispatched), noop_reentry_unavailable
// (no session channel; the event IS the delivery).

// NoopGuardEventComponent is the events.component value for every event the
// guard emits, so an operator can query the guard's history in one WHERE.
const NoopGuardEventComponent = "noop_guard"

// noopStampTimeout bounds every trigger stamp — the same 2s class the
// admission stamp uses; evidence collection must never stall a spawn or a
// completion.
const noopStampTimeout = 2 * time.Second

// noopReentryTimeout bounds the single re-entry turn. Deliberately short —
// the same shape as the 1688 idle-intervention turn: the session is past
// its main work and this turn exists to force a decision (do it, or say
// why not), not to start a second full tick.
const noopReentryTimeout = 5 * time.Minute

// noopReentryInstruction is the exact message re-entered into the SAME
// session of a lane whose tick was judged a no-op. The wording is the
// row's own contract — "either commit real work now, or commit a written
// report explaining exactly why you did nothing" — plus the naming
// (lane, tick, workdir) a session needs to act on it.
func noopReentryInstruction(lane, tickID, workdir string) string {
	return "You did no work this tick. Either commit real work now, or commit " +
		"a written report explaining exactly why you did nothing. " +
		"Workdir: " + workdir + ". Lane: " + lane + ". Tick: " + tickID + "."
}

// captureGitPair reads the (HEAD sha, branch) pair of the git repo at dir.
// Both legs are "" when the workdir is empty, the directory is not a
// readable git repo, or the repo has no commits yet — the caller stores the
// pair verbatim, and the empty legs make the verdict UNMEASURED. The exec
// shape mirrors gitmetrics.gitBaseline (best-effort, no error path).
func captureGitPair(dir string) (commit, branch string) {
	if strings.TrimSpace(dir) == "" {
		return "", ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", ""
	}
	commit = strings.TrimSpace(string(out))
	// symbolic-ref names the checked-out branch; a detached HEAD (or any
	// other failure) falls back to the sha itself so the pair stays
	// change-sensitive — "" would fake "unchanged branch" across states.
	bout, berr := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if berr != nil {
		branch = commit
	} else {
		branch = strings.TrimSpace(string(bout))
	}
	return commit, branch
}

// noopVerdict is the verdict computation at tick close. The pre pair is the
// spawn-time measurement; the post pair the close-time one; boardUnchanged
// carries the SCHED-GAP-1678 fingerprint leg (Bane's "AND the board is
// unchanged"). No-op iff both git legs are unchanged, the pair was measured
// (pre non-empty), and the board did not move.
func noopVerdict(preCommit, preBranch, postCommit, postBranch string, boardUnchanged bool) bool {
	if preCommit == "" && preBranch == "" {
		return false // unmeasured — never a no-op
	}
	if !boardUnchanged {
		return false // the board moved — output happened
	}
	return preCommit == postCommit && preBranch == postBranch
}

// boardFingerprintOrEmpty is the guard's read of the SCHED-GAP-1678
// fingerprint: the mtime+size pair of the lane's board file, "" when the
// board is missing or unreadable. Reusing boardFingerprint keeps ONE
// fingerprint implementation — the guard and the spawn gate can never
// measure the board differently.
func boardFingerprintOrEmpty(workdir string) string {
	fp, _ := boardFingerprint(workdir)
	return fp
}

// resolveNoopAllowed resolves the lane's no-op policy for one verdict: the
// explicit row override wins in BOTH directions; nil (NULL) derives from
// the lane class — satellites allowed, foremen not.
func resolveNoopAllowed(override *bool, laneClass string) bool {
	if override != nil {
		return *override
	}
	return laneClass == LaneClassSatellite
}

// laneClassForProject derives the lane class from the project row the way
// every other class consumer does (ClassifyLane, the SCHED-GAP-1696 API
// boundary derivation). allNames is the set of every project name (the
// owner test needs it). An empty row degrades to foreman — the strict
// side: an unclassifiable lane is governed, never exempted by an accident.
func laneClassForProject(p database.Project, allNames map[string]bool) string {
	if p.Name == "" {
		return LaneClassForeman
	}
	return ClassifyLane(p, allNames)
}

// recordNoopPreCapture stamps the BEFORE pair (git pair + board
// fingerprint) onto the tick row. Best-effort by contract: evidence
// collection must never block or fail the spawn — a failed stamp leaves the
// pre legs empty, which reads UNMEASURED and can never produce a no-op
// verdict (the safe failure direction).
func (s *Spawner) recordNoopPreCapture(tickID, workdir string) (commit, branch, boardFP string) {
	commit, branch = captureGitPair(workdir)
	boardFP = boardFingerprintOrEmpty(workdir)
	ctx, cancel := context.WithTimeout(context.Background(), noopStampTimeout)
	defer cancel()
	if err := database.StampTickPreTrigger(ctx, s.db, tickID, commit, branch, boardFP); err != nil {
		log.Printf("WARN: noop_guard pre-trigger stamp for %s: %v", tickID, err)
		return "", "", ""
	}
	return commit, branch, boardFP
}

// closeNoopVerdict is the tick-close half of the trigger: capture the AFTER
// pair, compute the verdict (git legs + the board leg), judge it by lane
// class, persist the verdict on the row, and run the enforcement branch
// (re-entry or clean close) with the matching event.
//
// Called from the gateway spawn path right after the POST returns, while
// the session context is still alive. Best-effort by contract: every
// failure inside is logged and never changes the tick's outcome — the
// trigger observes the tick, it does not own it.
//
// gwClient/gwCtx may be nil (no live channel): the verdict is still
// computed and recorded; the enforcement branch emits
// noop_reentry_unavailable instead of re-entering the session.
//
// model/provider are the pair that ACTUALLY ran this tick (the spawn
// path's resolved values), so a router-selected or chain-hopped turn
// re-enters on the same lane the tick used.
func (s *Spawner) closeNoopVerdict(gwClient *GatewayClient, gwCtx context.Context, proj PackedProject, tickID, preCommit, preBranch, preBoard, model, provider, gwKey string) {
	if s.db == nil || tickID == "" {
		return
	}
	// A never-captured pre pair means the trigger was not armed for this
	// tick (legacy row, or the spawn-side stamp failed): nothing to
	// compare — do not stamp a half-verdict.
	if preCommit == "" && preBranch == "" && preBoard == "" {
		return
	}
	postCommit, postBranch := captureGitPair(proj.Workdir)
	postBoard := boardFingerprintOrEmpty(proj.Workdir)
	// The board leg: UNCHANGED means both captures saw a readable board
	// with the same fingerprint, OR both saw no board at all ("" == "").
	// A pre fingerprint that vanished (board deleted mid-tick) reads as
	// changed — output or cleanup happened, never judged a no-op.
	boardUnchanged := preBoard == postBoard
	verdict := noopVerdict(preCommit, preBranch, postCommit, postBranch, boardUnchanged)

	// The lane class + override: one row read (the packed row carries
	// neither the parent reference nor the override), one indexed pass for
	// the owner test's name set.
	var cls string
	var override *bool
	ctx, cancel := context.WithTimeout(context.Background(), noopStampTimeout)
	defer cancel()
	p, err := database.GetProject(ctx, s.db, proj.Name)
	if err != nil {
		cls = LaneClassForeman // strict side: unknown lane is governed
		log.Printf("WARN: noop_guard lane lookup for %s failed (%v) — deriving foreman", proj.Name, err)
	} else {
		all := map[string]bool{}
		if rows, qerr := s.db.QueryContext(ctx, `SELECT name FROM projects`); qerr == nil {
			for rows.Next() {
				var n string
				if rows.Scan(&n) == nil {
					all[n] = true
				}
			}
			rows.Close()
		}
		cls = laneClassForProject(*p, all)
		override = p.NoopAllowed
	}

	noopAllowed := resolveNoopAllowed(override, cls)
	noopFlag := 0
	if verdict && !noopAllowed {
		noopFlag = 1
	}
	vctx, vcancel := context.WithTimeout(context.Background(), noopStampTimeout)
	defer vcancel()
	if err := database.RecordTickNoopVerdict(vctx, s.db, tickID, postCommit, postBranch, noopFlag); err != nil {
		log.Printf("WARN: noop_guard verdict stamp for %s: %v", tickID, err)
	}

	if !verdict {
		return // work happened (or the capture was partial) — nothing to judge
	}

	if noopAllowed {
		s.emitNoopEvent(SeverityInfo, "noop_allowed", proj.Name, tickID,
			"no-op verdict on a lane that may no-op — tick closes clean", map[string]any{
				"noop_allowed": true,
				"lane_class":   cls,
				"pre_commit":   shortSHA(preCommit),
				"post_commit":  shortSHA(postCommit),
			})
		return
	}

	s.emitNoopEvent(SeverityMedium, "noop_detected", proj.Name, tickID,
		"no-op verdict on a lane that may not no-op — re-entry armed", map[string]any{
			"noop_allowed": false,
			"lane_class":   cls,
			"pre_commit":   shortSHA(preCommit),
			"pre_branch":   preBranch,
			"post_commit":  shortSHA(postCommit),
			"post_branch":  postBranch,
		})

	s.reenterNoopSession(gwClient, gwCtx, proj, tickID, model, provider, gwKey)
}

// reenterNoopSession fires the instruction into the SAME gateway session
// (the tick id rides X-Hermes-Session-Key, exactly like the GAP-080 retry
// and the 1688 idle hook — the gateway re-attaches the session instead of
// minting a new one). With no live channel (nil client or a dead ctx) the
// miss degrades to the noop_reentry_unavailable event — the row's contract
// says "emit a scheduler event instead; do not fabricate one".
func (s *Spawner) reenterNoopSession(gwClient *GatewayClient, gwCtx context.Context, proj PackedProject, tickID, model, provider, gwKey string) {
	database.RecordFeatureUse(database.FeatureNoopGuard)
	if gwClient == nil || gwCtx == nil || gwCtx.Err() != nil {
		log.Printf("NOOP-GUARD: %s tick=%s re-entry unavailable (no live session channel) — event emitted instead", proj.Name, tickID)
		s.emitNoopEvent(SeverityMedium, "noop_reentry_unavailable", proj.Name, tickID,
			"no session channel for the no-op re-entry — event emitted instead", map[string]any{
				"instruction": noopReentryInstruction(proj.Name, tickID, proj.Workdir),
			})
		return
	}
	msg := noopReentryInstruction(proj.Name, tickID, proj.Workdir)
	log.Printf("NOOP-GUARD: %s tick=%s re-entering SAME session: %s", proj.Name, tickID, msg)
	ivCtx, ivCancel := context.WithTimeout(gwCtx, noopReentryTimeout)
	_, _, ierr := gwClient.SendResponseStream(ivCtx, msg, model, provider, gwKey, tickID, noopReentryTimeout)
	ivCancel()
	if ierr != nil {
		log.Printf("NOOP-GUARD: %s tick=%s re-entry turn error (ignored; tick verdict unchanged): %v", proj.Name, tickID, ierr)
	}
	s.emitNoopEvent(SeverityMedium, "reentry_requested", proj.Name, tickID,
		"same-session re-entry dispatched with the do-it-or-explain instruction", map[string]any{
			"instruction": msg,
			"turn_error":  errorStringOrNil(ierr),
		})
}

// emitNoopEvent is the guard's single event writer (component noop_guard).
// Best-effort: a failed event insert costs a log line, never the tick.
func (s *Spawner) emitNoopEvent(severity EventSeverity, eventType, project, tickID, message string, extra map[string]any) {
	if s.events == nil {
		return
	}
	details := map[string]any{
		"event_type": eventType,
		"tick_id":    tickID,
		"project":    project,
	}
	for k, v := range extra {
		details[k] = v
	}
	s.events.Emit(context.Background(), severity, NoopGuardEventComponent, message, details)
}

// errorStringOrNil renders err for an event payload ("" when nil — an empty
// string reads as "none" in the JSON details).
func errorStringOrNil(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
