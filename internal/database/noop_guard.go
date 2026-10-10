package database

import (
	"context"
	"database/sql"
	"fmt"
)

// SCHED-GAP-1682 — the storage layer of the no-op-by-lane-class guard.
//
// The TRIGGER is the (HEAD commit sha, branch name) pair of the lane's
// workdir, captured BEFORE the foreman runs and again AFTER the tick
// finishes, and stored on the tick row (pre_commit/pre_branch,
// post_commit/post_branch) so the verdict is auditable in SQL — an operator
// reading the row sees both measurements, never just the derived verdict.
//
// The VERDICT is no-op iff both legs are UNCHANGED (pre==post commit AND
// pre==post branch) AND the pair was actually measured (the PRE legs
// non-empty — a zero/empty capture is "unmeasured" and can never produce a
// no-op). The board leg of Bane's definition ("unchanged commit, branch,
// AND board") is measured by the SCHED-GAP-1678 board-stasis machinery; on
// this trigger the git pair is the scheduler-owned evidence and the verdict
// consumes it, so the two systems keep their own single authorities.
//
// noop_flag=1 is written ONLY on the enforcement branch — a lane that may
// NOT no-op whose verdict is no-op. An allowed satellite no-op closes
// clean (flag stays 0); its verdict is observable through the noop_guard
// events. `WHERE noop_flag=1` therefore reads exactly "the ticks the lane
// was told to explain".

// StampTickPreTrigger records the BEFORE capture on the tick row at spawn
// time. Best-effort by contract: this is evidence collection — a failed
// stamp is logged by the caller and never blocks the spawn.
func StampTickPreTrigger(ctx context.Context, db *sql.DB, tickID, commit, branch, boardFP string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE ticks SET pre_commit = ?, pre_branch = ?, pre_board = ? WHERE id = ?`,
		commit, branch, boardFP, tickID)
	if err != nil {
		return fmt.Errorf("stamp pre trigger for tick %s: %w", tickID, err)
	}
	return nil
}

// RecordTickNoopVerdict is the ONE terminal write for the trigger: it
// stamps the AFTER capture and the enforcement flag in the SAME UPDATE, so
// a row can never carry a post measurement without its verdict (or a flag
// without the evidence behind it). noopFlag is 1 only on the re-entry
// branch; 0 keeps the honest default everywhere else.
func RecordTickNoopVerdict(ctx context.Context, db *sql.DB, tickID, postCommit, postBranch string, noopFlag int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE ticks SET post_commit = ?, post_branch = ?, noop_flag = ? WHERE id = ?`,
		postCommit, postBranch, noopFlag, tickID)
	if err != nil {
		return fmt.Errorf("record noop verdict for tick %s: %w", tickID, err)
	}
	return nil
}
