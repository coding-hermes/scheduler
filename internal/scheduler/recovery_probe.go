package scheduler

// SCHED-GAP-16936 piece 2 — THE RECOVERY PROBE.
//
// THE PROBLEM. A tick that ended dry (ran, landed no artifact) and a tick whose
// session never produced anything wrote the same shape ("no_changes", outcome
// dry_run): the row could not say whether the WORKTREE was genuinely clean or
// whether it was holding uncommitted content a later tick could harvest. The
// two states have opposite operational meanings — one is "nothing to do here",
// the other is "there is work sitting on the floor" — and the fleet had no way
// to tell them apart without re-running git against the box by hand.
//
// THE PROBE. At finalization the scheduler asks git ONE question about the
// project's workdir — `status --porcelain` — and derives a closed-vocabulary
// verdict:
//
//	no_changes          the worktree is clean: no modified, staged or
//	                    untracked content. A dry tick in this state is
//	                    genuinely nothing-happened.
//	recovery_available  the worktree holds uncommitted CONTENT (modified,
//	                    staged, renamed, deleted or untracked paths that are
//	                    not foreman bookkeeping). A later tick could harvest
//	                    it; a failed tick in this state is repair-eligible
//	                    (see repair_redispatch.go).
//	not_probed          the probe could not answer (no workdir, not a git
//	                    worktree, git missing, probe timeout). NEVER folded
//	                    into no_changes: "could not measure" is not "clean".
//
// BOOKKEEPING IS NOT CONTENT. The foreman's own board/wave writes live under
// .coding-hermes/ (board/tasks.jsonl, waves/<tick>.json, …) and a wave tick
// ALWAYS produces some of them; counting those as recoverable content would
// label every wave tick recovery_available and make the verdict useless. Paths
// under .coding-hermes/ are therefore counted separately (reported as
// bookkeeping=<n> in the detail) and never decide the verdict.
//
// The verdict rides the SAME single finalization UPDATE as the outcome, the
// reason code and the dry-reason token (LifecycleTracker.Complete — the
// coverage convention: no second write that can silently miss a row), on
// ticks.recovery_state / ticks.recovery_detail (migration v92).

import (
	"bufio"
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The closed recovery-state vocabulary (mirrors the v92 CHECK exactly; the Go
// twin is RecoveryStateIsValid). Empty is NOT a member: it is the honest "not
// owed" for a productive tick / legacy row, checked with == "" like every other
// honest-empty column.
const (
	// RecoveryStateNoChanges — the worktree is clean.
	RecoveryStateNoChanges = "no_changes"
	// RecoveryStateAvailable — the worktree holds uncommitted content a later
	// tick could harvest.
	RecoveryStateAvailable = "recovery_available"
	// RecoveryStateNotProbed — the probe could not answer. Deliberately
	// distinct from no_changes.
	RecoveryStateNotProbed = "not_probed"
)

// RecoveryStates is the closed vocabulary in reporting order.
var RecoveryStates = []string{
	RecoveryStateNoChanges,
	RecoveryStateAvailable,
	RecoveryStateNotProbed,
}

// RecoveryStateIsValid reports whether v is a member of the closed vocabulary.
// The empty string is NOT valid here (it means "no verdict owed").
func RecoveryStateIsValid(v string) bool {
	for _, s := range RecoveryStates {
		if s == v {
			return true
		}
	}
	return false
}

// recoveryBookkeepingPrefix is the foreman's own bookkeeping tree inside a
// lane's checkout. Writes there are the tick doing its paperwork, not work a
// harvester could pick up — see the file comment.
const recoveryBookkeepingPrefix = ".coding-hermes/"

// recoveryProbeTimeout bounds the git call. `git status` on a cold, large
// checkout can take seconds; the finalization path must never hang on it, and
// a timeout is reported as not_probed (never as clean).
const recoveryProbeTimeout = 10 * time.Second

// recoveryProbeMaxSample bounds how many sample paths the detail carries (the
// counts stay exact; only the NAMES are bounded so a huge change set cannot
// blow up a log line or a column).
const recoveryProbeMaxSample = 3

// RecoveryProbe is one worktree's recovery verdict.
type RecoveryProbe struct {
	// State is a member of the closed vocabulary above, or "" when the probe
	// was never run (the caller is not a dry/failed tick).
	State string
	// Modified counts changed tracked paths (staged and/or unstaged, renamed,
	// deleted, conflicted) under content paths.
	Modified int
	// Untracked counts untracked content paths.
	Untracked int
	// Bookkeeping counts paths under .coding-hermes/ (excluded from the
	// verdict, reported for honesty).
	Bookkeeping int
	// Samples holds up to recoveryProbeMaxSample content paths (sorted).
	Samples []string
	// Detail is the human-readable evidence the verdict was derived from,
	// bounded in length. It is persisted on ticks.recovery_detail.
	Detail string
}

// Recoverable reports whether the worktree holds uncommitted content.
func (r RecoveryProbe) Recoverable() bool { return r.State == RecoveryStateAvailable }

// String renders the probe for a log line.
func (r RecoveryProbe) String() string {
	if r.Detail != "" {
		return r.State + " (" + r.Detail + ")"
	}
	return r.State
}

// probeWorktreeRecovery asks git for one worktree's uncommitted content.
//
// A nil/empty workdir, a directory that is not a git worktree, a missing git
// binary, a probe timeout and an unreadable status stream all return
// not_probed WITH the reason in Detail — never no_changes. This is the whole
// point of the third state: an unmeasured worktree must never be reported as
// clean, because clean is a positive claim somebody will act on.
func probeWorktreeRecovery(ctx context.Context, workdir string) RecoveryProbe {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return RecoveryProbe{State: RecoveryStateNotProbed, Detail: "no workdir on the project row"}
	}
	if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
		return RecoveryProbe{State: RecoveryStateNotProbed, Detail: "workdir is not a directory: " + workdir}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, recoveryProbeTimeout)
	defer cancel()

	// rev-parse first: `git status` outside a repository writes noise to stderr
	// and exits non-zero, but a bare error cannot distinguish "not a repo" from
	// "git is broken", and the operator needs to know which.
	if out, err := runGitProbe(probeCtx, workdir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return RecoveryProbe{State: RecoveryStateNotProbed,
			Detail: "not a git worktree (" + firstErrorLine(err.Error()+" "+out) + ")"}
	}

	out, err := runGitProbe(probeCtx, workdir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		reason := "git status failed"
		if probeCtx.Err() == context.DeadlineExceeded {
			reason = "git status timed out after " + recoveryProbeTimeout.String()
		}
		return RecoveryProbe{State: RecoveryStateNotProbed,
			Detail: reason + " (" + firstErrorLine(err.Error()) + ")"}
	}

	probe := classifyPorcelainStatus(out)
	probe.Detail = probe.describe(workdir)
	return probe
}

// classifyPorcelainStatus derives the verdict from `git status --porcelain`
// output. Pure, so the classification is unit-testable without a repository.
//
// Porcelain v1 lines are two status columns, a space, then the path; an
// untracked entry reads "?? path". Anything else (M, A, R, D, U, C, or a
// rename's "orig -> new") is uncommitted content on a tracked path.
func classifyPorcelainStatus(out string) RecoveryProbe {
	probe := RecoveryProbe{State: RecoveryStateNoChanges}
	var samples []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 3 {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			continue // branch header (porcelain v2 / -b output)
		}
		path := strings.TrimSpace(line[3:])
		// A rename/copy entry names both sides ("orig -> new"): the DESTINATION
		// is the path that exists now.
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = strings.TrimSpace(path[idx+4:])
		}
		path = strings.Trim(path, `"`)
		if strings.HasPrefix(path, recoveryBookkeepingPrefix) ||
			strings.Contains(path, "/"+strings.TrimSuffix(recoveryBookkeepingPrefix, "/")+"/") {
			probe.Bookkeeping++
			continue
		}
		if strings.HasPrefix(line, "??") {
			probe.Untracked++
		} else {
			probe.Modified++
		}
		if len(samples) < recoveryProbeMaxSample {
			samples = append(samples, path)
		}
	}
	if probe.Modified > 0 || probe.Untracked > 0 {
		probe.State = RecoveryStateAvailable
	}
	sort.Strings(samples)
	probe.Samples = samples
	return probe
}

// describe renders the bounded evidence string persisted on
// ticks.recovery_detail. The counts are exact; the sample names are bounded.
func (p RecoveryProbe) describe(workdir string) string {
	parts := []string{
		"modified=" + strconv.Itoa(p.Modified),
		"untracked=" + strconv.Itoa(p.Untracked),
		"bookkeeping=" + strconv.Itoa(p.Bookkeeping),
	}
	if len(p.Samples) > 0 {
		parts = append(parts, "sample="+strings.Join(p.Samples, ","))
	}
	if workdir != "" {
		parts = append(parts, "workdir="+workdir)
	}
	return strings.Join(parts, " ")
}

// runGitProbe runs one bounded git command in dir and returns its stdout. It is
// deliberately separate from gitOut (wave_stale_base.go), which carries no
// context/timeout: the finalization path cannot afford an unbounded subprocess.
func runGitProbe(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Bound the READ, not just the wait: a giant untracked tree would otherwise
	// drag an unbounded listing into memory for a verdict that only needs
	// counts.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", err
	}
	const maxPorcelainBytes = 4 << 20
	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxPorcelainBytes))
	waitErr := cmd.Wait()
	if waitErr != nil {
		return string(raw), waitErr
	}
	if readErr != nil {
		return string(raw), readErr
	}
	return string(raw), nil
}

// probeProjectRecovery resolves the lane's workdir from the project row and
// probes it. A missing project row (or a NULL workdir) probes as not_probed —
// the honest "we had nothing to look at".
func probeProjectRecovery(ctx context.Context, db *sql.DB, project string) RecoveryProbe {
	return probeWorktreeRecovery(ctx, projectWorkdirColumn(db, project))
}
