package scheduler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── ADV-R06 (G11): git-verified board freshness reader ───────────────────
//
// The tests below seed the A4 catalog instance classes against REAL
// throwaway git repos (same style as gitmetrics_test.go):
//
//   - future stamp        → immediate distrust, before any git probe
//   - orphan hash         → resolves as an object, unreachable from HEAD
//   - no pointer          → complete row with no commit_hash at all
//   - off-by-one pointer  → commit resolves at HEAD but names a different
//                           row id
//
// plus the flip-window/flip-overdue classes, the timestamp dialect zoo,
// aggregate fail-safety, and the AC pin (work-to-spawn never raised by a
// row whose hash resolves at HEAD).

// freshFixture is one throwaway repo + board + fixed read clock.
type freshFixture struct {
	t      *testing.T
	dir    string // git repo (worktree root)
	board  string // absolute board path
	now    time.Time
	window time.Duration
}

// newFreshFixture creates a repo with one seed commit and an empty board
// file. All commits carry explicit committer dates so flip-window
// arithmetic is deterministic; the read clock is fixed.
func newFreshFixture(t *testing.T) *freshFixture {
	t.Helper()
	fx := &freshFixture{
		t:      t,
		dir:    t.TempDir(),
		now:    time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		window: 55 * time.Minute,
	}
	fx.git("init", "-q")
	fx.git("config", "user.email", "freshness@example.com")
	fx.git("config", "user.name", "Freshness Test")
	fx.commitAt("seed", time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC), "seed")
	if err := os.MkdirAll(filepath.Join(fx.dir, ".coding-hermes", "board"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx.board = filepath.Join(fx.dir, ".coding-hermes", "board", "tasks.jsonl")
	fx.writeBoard()
	return fx
}

// git runs a git command in the fixture repo, failing the test on error.
func (fx *freshFixture) git(args ...string) string {
	fx.t.Helper()
	return runGitTest(fx.t, fx.dir, args...)
}

// commitAt commits file <name>.txt with an explicit GIT_COMMITTER_DATE
// (the %cI freshness source; git has no committer.date config key, the
// env var is the only lever) so flip-window math is deterministic.
// Returns the short sha, the pointer form boards actually write.
func (fx *freshFixture) commitAt(name string, when time.Time, message string) string {
	fx.t.Helper()
	if err := os.WriteFile(filepath.Join(fx.dir, name+".txt"), []byte(name+"\n"), 0o644); err != nil {
		fx.t.Fatal(err)
	}
	fx.git("add", name+".txt")
	cmd := exec.Command("git", "-C", fx.dir, "commit", "-q", "-m", message)
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_DATE="+when.Format(time.RFC3339),
		"GIT_AUTHOR_DATE="+when.Format(time.RFC3339))
	if out, err := cmd.CombinedOutput(); err != nil {
		fx.t.Fatalf("commit %s failed: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(fx.git("rev-parse", "--short", "HEAD"))
}

// orphanAt is commitAt plus a rewind: the commit is kept alive by a side
// ref but the current branch moves back, leaving the commit UNREACHABLE
// from HEAD — the A4 orphan-hash shape.
func (fx *freshFixture) orphanAt(name string, when time.Time, message string) string {
	fx.t.Helper()
	short := fx.commitAt(name, when, message)
	full := strings.TrimSpace(fx.git("rev-parse", "HEAD"))
	fx.git("branch", "keep-"+name)
	fx.git("reset", "-q", "--hard", "HEAD~1")
	if ok, _ := gitReachableFromHEAD(fx.dir, full); ok {
		fx.t.Fatal("test premise: orphan commit unexpectedly reachable from HEAD")
	}
	return short
}

// workLanded is the canonical recent landing time: 10 min before the read
// clock (inside the 55-min flip window).
func (fx *freshFixture) workLanded() time.Time {
	return fx.now.Add(-10 * time.Minute)
}

// oldLanded is one window + 5 min before the read clock.
func (fx *freshFixture) oldLanded() time.Time {
	return fx.now.Add(-(fx.window + 5*time.Minute))
}

// writeBoard (re)writes tasks.jsonl from the given raw lines.
func (fx *freshFixture) writeBoard(lines ...string) {
	fx.t.Helper()
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(fx.board, []byte(content), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

// freshRow builds one JSONL row. Field values with the magic strings
// "null", "true", "false" are emitted as raw JSON (for null commit_hash
// and boolean perpetual); everything else is a JSON string.
func freshRow(id, status string, fields map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"id":%q`, id)
	fmt.Fprintf(&b, `,"status":%q`, status)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	// deterministic field order: insertion sort
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		switch fields[k] {
		case "null":
			fmt.Fprintf(&b, ",%q:null", k)
		case "true":
			fmt.Fprintf(&b, ",%q:true", k)
		case "false":
			fmt.Fprintf(&b, ",%q:false", k)
		default:
			fmt.Fprintf(&b, ",%q:%q", k, fields[k])
		}
	}
	b.WriteString("}")
	return b.String()
}

// read runs the reader with the fixture's fixed clock and window.
func (fx *freshFixture) read() FreshnessReport {
	fx.t.Helper()
	return ReadBoardFreshness(fx.dir, fx.board, FreshnessOptions{
		FlipWindow: fx.window,
		Now:        fx.now,
	})
}

// verdictOf returns the verdict for one row id, failing if absent.
func verdictOf(t *testing.T, rep FreshnessReport, id string) RowVerdict {
	t.Helper()
	for _, v := range rep.Rows {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no verdict for row %q (rows: %d)", id, len(rep.Rows))
	return RowVerdict{}
}

// ── The four A4 catalog classes (the AC's seeded instances) ──────────────

// TestFreshnessCatalog_FutureStamp: a complete row whose completed_at is
// after the read clock is distrusted IMMEDIATELY — even though its commit
// resolves, is reachable, and is attributed (git evidence is not a rescue:
// the row lied about time; requirement 4 wins before any git probe).
func TestFreshnessCatalog_FutureStamp(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.workLanded(), "fix: thing. Addresses T-1.")
	fx.writeBoard(freshRow("T-1", "complete", map[string]string{
		"commit_hash":  short,
		"completed_at": "2026-09-15 13:30:00.000000", // 90 min in the future
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-1")
	if v.Status != RowUnverified || v.Reason != ReasonFutureStamp {
		t.Fatalf("future-stamp row: got status=%q reason=%q, want %q/%q",
			v.Status, v.Reason, RowUnverified, ReasonFutureStamp)
	}
	// Distrust is immediate: no commit resolution performed for it.
	if v.Commit != "" {
		t.Fatalf("future-stamp row must not resolve evidence, got commit %s", v.Commit)
	}
	if rep.WorkToSpawn {
		t.Fatal("an unverified row is never work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("idle must never be proven while a row is unverified")
	}
}

// TestFreshnessCatalog_OrphanHash: a complete row whose hash resolves as
// an object but is NOT reachable from HEAD (18 such rows on the live
// board) is claimed-closed-unverified, with the full sha and committer
// date still carried as evidence.
func TestFreshnessCatalog_OrphanHash(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.orphanAt("orphaned", fx.workLanded(), "fix: thing. Addresses T-2.")
	fx.writeBoard(freshRow("T-2", "complete", map[string]string{
		"commit_hash":  short,
		"completed_at": "2026-09-15 11:50:00.000000",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-2")
	if v.Status != RowUnverified || v.Reason != ReasonOrphanHash {
		t.Fatalf("orphan row: got status=%q reason=%q, want %q/%q",
			v.Status, v.Reason, RowUnverified, ReasonOrphanHash)
	}
	if v.Commit == "" {
		t.Fatal("orphan row should keep the resolved sha as evidence")
	}
	if v.Freshness.IsZero() {
		t.Fatal("orphan row should carry the committer date as evidence")
	}
	if rep.WorkToSpawn || rep.VerifiablyIdle {
		t.Fatalf("orphan row: aggregate must be neither work (%v) nor idle (%v)",
			rep.WorkToSpawn, rep.VerifiablyIdle)
	}
}

// TestFreshnessCatalog_NoPointer: a complete row with no commit_hash
// (absent, null, or empty — the 34 pointer-less closures on the live
// board) is claimed-closed-unverified and never idle-proof.
func TestFreshnessCatalog_NoPointer(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(
		freshRow("T-3", "complete", map[string]string{"completed_at": "2026-09-15 11:00:00.000000"}),
		freshRow("T-3b", "complete", map[string]string{"commit_hash": "null", "completed_at": "2026-09-15 11:00:00"}),
		freshRow("T-3c", "complete", map[string]string{"commit_hash": "", "completed_at": "2026-09-15 11:00:00"}),
	)
	rep := fx.read()
	for _, id := range []string{"T-3", "T-3b", "T-3c"} {
		v := verdictOf(t, rep, id)
		if v.Status != RowUnverified || v.Reason != ReasonNoPointer {
			t.Fatalf("no-pointer row %s: got status=%q reason=%q", id, v.Status, v.Reason)
		}
	}
	if rep.WorkToSpawn {
		t.Fatal("no-pointer complete rows are not work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("pointerless closures must break the idle claim")
	}
}

// TestFreshnessCatalog_OffByOnePointer: a complete row whose commit
// resolves at HEAD but names a DIFFERENT row's id (the off-by-one
// pointer) is claimed-closed-unverified with reason mis-attributed;
// the row actually named by the commit verifies closed on the same hash.
func TestFreshnessCatalog_OffByOnePointer(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work4", fx.workLanded(), "fix: thing. Addresses T-5.")
	fx.writeBoard(
		freshRow("T-4", "complete", map[string]string{
			"commit_hash":  short,
			"completed_at": "2026-09-15 11:50:00.000000",
		}),
		freshRow("T-5", "complete", map[string]string{
			"commit_hash":  short,
			"completed_at": "2026-09-15 11:50:00.000000",
		}),
	)
	rep := fx.read()
	if v := verdictOf(t, rep, "T-4"); v.Status != RowUnverified || v.Reason != ReasonMisAttributed {
		t.Fatalf("off-by-one row: got status=%q reason=%q, want %q/%q",
			v.Status, v.Reason, RowUnverified, ReasonMisAttributed)
	}
	if v := verdictOf(t, rep, "T-5"); v.Status != RowClosed {
		t.Fatalf("correctly attributed row T-5 should be closed, got %q", v.Status)
	}
}

// TestFreshnessClosedRow: the happy path — complete row, hash resolving
// at HEAD, commit names the row: CLOSED, freshness = committer date (not
// completed_at), idle claim allowed when it is the only row.
func TestFreshnessClosedRow(t *testing.T) {
	fx := newFreshFixture(t)
	landed := fx.workLanded().Add(-2 * time.Hour) // well before the stamp
	short := fx.commitAt("work", landed, "feat: reader. Addresses T-9.\n\nBody mentions T-9 again.")
	fx.writeBoard(freshRow("T-9", "complete", map[string]string{
		"commit_hash":  short,
		"completed_at": "2026-09-15 11:00:00.000000",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-9")
	if v.Status != RowClosed {
		t.Fatalf("verifiable row should be closed, got %q (reason %q)", v.Status, v.Reason)
	}
	if !v.Freshness.Equal(landed) {
		t.Fatalf("freshness must be the committer date %v, got %v", landed, v.Freshness)
	}
	if !rep.LastVerifiedLanding.Equal(landed) {
		t.Fatalf("LastVerifiedLanding should track the closed landing, got %v", rep.LastVerifiedLanding)
	}
	if rep.WorkToSpawn {
		t.Fatal("closed work must never raise work-to-spawn")
	}
	if !rep.VerifiablyIdle {
		t.Fatal("a board of one verified closure is verifiably idle")
	}
}

// TestFreshnessFlipWindow: requirement 5 — a row still open whose work
// demonstrably landed is NOT work-to-spawn inside the flip window
// (pointer path: the open row carries the attributed resolving hash).
func TestFreshnessFlipWindow(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.workLanded(), "feat: flip. Addresses T-10.")
	fx.writeBoard(freshRow("T-10", "pending", map[string]string{
		"commit_hash": short,
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-10")
	if v.Status != RowFlipWindow {
		t.Fatalf("in-window unflipped row: got %q, want %q", v.Status, RowFlipWindow)
	}
	if rep.WorkToSpawn {
		t.Fatal("git-first override failed: work exists yet row raised work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("mid-flip row is neither work nor idle-proof")
	}
	want := fx.workLanded().Add(fx.window)
	if !rep.RereadAfter.Equal(want) {
		t.Fatalf("RereadAfter = %v, want %v", rep.RereadAfter, want)
	}
}

// TestFreshnessFlipOverdue: past the window with a resolving attributed
// pointer the row is flip-overdue — its own anomaly class, but STILL
// never work-to-spawn (the AC: never report work for a row whose hash
// resolves at HEAD).
func TestFreshnessFlipOverdue(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "feat: stale flip. Addresses T-11.")
	fx.writeBoard(freshRow("T-11", "pending", map[string]string{
		"commit_hash": short,
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-11")
	if v.Status != RowFlipOverdue {
		t.Fatalf("overdue unflipped row: got %q, want %q", v.Status, RowFlipOverdue)
	}
	if rep.WorkToSpawn {
		t.Fatal("flip-overdue must never be work-to-spawn — the work exists")
	}
}

// TestFreshnessFlipWindowPointerless: the mid-flip case with NO pointer
// on the row (worker committed; foreman has not written commit_hash yet):
// the window-bounded recent-commit scan attributes the landing by message
// alone inside the window; past it the row returns to open.
func TestFreshnessFlipWindowPointerless(t *testing.T) {
	fx := newFreshFixture(t)
	fx.commitAt("work", fx.workLanded(), "feat: flip. Addresses T-12.")
	fx.writeBoard(freshRow("T-12", "pending", nil))
	rep := fx.read()
	if v := verdictOf(t, rep, "T-12"); v.Status != RowFlipWindow {
		t.Fatalf("pointerless in-window row: got %q, want %q", v.Status, RowFlipWindow)
	}
	if rep.WorkToSpawn {
		t.Fatal("mid-flip pointerless row must not raise work-to-spawn")
	}

	// Same shape but the naming commit is older than the window: the
	// grace period expired — the row is open again.
	fx.commitAt("oldwork", fx.oldLanded(), "feat: old. Addresses T-13.")
	fx.writeBoard(freshRow("T-13", "pending", nil))
	rep = fx.read()
	if v := verdictOf(t, rep, "T-13"); v.Status != RowOpen {
		t.Fatalf("pointerless past-window row must be open, got %q", v.Status)
	}
	if !rep.WorkToSpawn {
		t.Fatal("a genuinely open row must raise work-to-spawn")
	}
}

// TestFreshnessOpenRowRaisesWork: a plain open row with no git evidence
// either way is work-to-spawn (the point of the reader).
func TestFreshnessOpenRowRaisesWork(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(freshRow("T-14", "pending", nil))
	rep := fx.read()
	if !rep.WorkToSpawn {
		t.Fatal("open row must raise work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("open row forbids idle")
	}
}

// TestFreshnessStatusVocabularies: complete/completed/done close;
// pending/todo/in_progress/claimed/empty stay open; blocked is user-gated
// (not work-to-spawn, not idle-proof); unknown statuses are other.
func TestFreshnessStatusVocabularies(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "chore: closes T-C1 T-C2 T-C3.")
	fx.writeBoard(
		freshRow("T-C1", "complete", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
		freshRow("T-C2", "completed", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
		freshRow("T-C3", "done", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
		freshRow("T-O1", "pending", nil),
		freshRow("T-O2", "todo", nil),
		freshRow("T-O3", "in_progress", nil),
		freshRow("T-O4", "claimed", nil),
		freshRow("T-O5", "", nil),
		freshRow("T-B1", "blocked", map[string]string{"blocked_reason": "user gate"}),
		freshRow("T-X1", "wontfix", nil),
	)
	rep := fx.read()
	for _, id := range []string{"T-C1", "T-C2", "T-C3"} {
		if v := verdictOf(t, rep, id); v.Status != RowClosed {
			t.Fatalf("%s: completion vocabulary should close, got %q", id, v.Status)
		}
	}
	for _, id := range []string{"T-O1", "T-O2", "T-O3", "T-O4", "T-O5"} {
		if v := verdictOf(t, rep, id); v.Status != RowOpen {
			t.Fatalf("%s: open vocabulary should be open, got %q", id, v.Status)
		}
	}
	if v := verdictOf(t, rep, "T-B1"); v.Status != RowBlocked {
		t.Fatalf("blocked row: got %q", v.Status)
	}
	if v := verdictOf(t, rep, "T-X1"); v.Status != RowOther {
		t.Fatalf("unknown status row: got %q", v.Status)
	}
	if !rep.WorkToSpawn {
		t.Fatal("open rows present ⇒ work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("open+blocked+other rows forbid idle")
	}
}

// TestFreshnessTimestampZoo: every dialect parses for future-stamp
// detection; naive stamps read as UTC; garbage/empty are simply absent
// (no future-distrust from an unparseable stamp). Mid-epoch stamps from
// any dialect never distrust; the same dialects future-shifted always do.
func TestFreshnessTimestampZoo(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "fix: zoo. Addresses T-Z1 T-Z2 T-Z3 T-Z4 T-Z5 T-Z6 T-Z7.")
	past := []string{
		"2026-09-15 11:00:00.000000",  // synthetic-zero micros, space sep (UTC)
		"2026-09-15T11:00:00",         // naive ISO (UTC)
		"2026-09-15 11:00:00",         // space separator
		"2026-09-15T06:00:00-05:00",   // numeric offset == 11:00Z
		"2026-09-15T11:00:00.123456Z", // Z + micros
		"2026-09-15",                  // date only
	}
	for i, stamp := range past {
		id := fmt.Sprintf("T-Z%d", i+1)
		fx.writeBoard(freshRow(id, "complete", map[string]string{
			"commit_hash":  short,
			"completed_at": stamp,
		}))
		v := verdictOf(t, fx.read(), id)
		if v.Status != RowClosed {
			t.Fatalf("stamp %q: mid-epoch stamp must not distrust, got %q/%q",
				stamp, v.Status, v.Reason)
		}
	}
	futures := []string{
		"2026-09-15 13:00:00.000000",
		"2026-09-15T13:00:00",
		"2026-09-15 13:00:00",
		"2026-09-15T08:30:00-05:00", // == 13:30Z
		"2026-09-15T13:00:00.5Z",
		"2026-09-16",
	}
	for i, stamp := range futures {
		id := fmt.Sprintf("T-Z%d", i+1)
		fx.writeBoard(freshRow(id, "complete", map[string]string{
			"commit_hash":  short,
			"completed_at": stamp,
		}))
		v := verdictOf(t, fx.read(), id)
		if v.Status != RowUnverified || v.Reason != ReasonFutureStamp {
			t.Fatalf("future stamp %q must distrust immediately, got %q/%q",
				stamp, v.Status, v.Reason)
		}
	}
	// Unparseable stamps are absent stamps: no distrust, and the row can
	// still close on git evidence.
	fx.writeBoard(freshRow("T-Z7", "complete", map[string]string{
		"commit_hash":  short,
		"completed_at": "not-a-date",
	}))
	if v := verdictOf(t, fx.read(), "T-Z7"); v.Status != RowClosed {
		t.Fatalf("garbage stamp must not block git-verified closure, got %q/%q", v.Status, v.Reason)
	}
}

// TestFreshnessAggregates_FailSafe: malformed lines make the read
// work-visible (never hide work); an unreadable repo parks every complete
// row and forbids idle; a missing board yields the zero report.
func TestFreshnessAggregates_FailSafe(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "fix: safe. Addresses T-S1.")

	// Malformed line alongside a closed row: work-visible, not idle.
	fx.writeBoard(
		freshRow("T-S1", "complete", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
		`{"id": "T-BROKEN", "status": `,
	)
	rep := fx.read()
	if rep.MalformedLines != 1 {
		t.Fatalf("malformed lines: got %d, want 1", rep.MalformedLines)
	}
	if !rep.WorkToSpawn {
		t.Fatal("malformed line must count as work-visible")
	}
	if rep.VerifiablyIdle {
		t.Fatal("malformed line forbids idle")
	}

	// Non-git repo dir: complete rows park as repo-unreadable; open rows
	// stay open (work); idle impossible.
	fx.writeBoard(
		freshRow("T-S2", "complete", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
		freshRow("T-S3", "pending", nil),
	)
	empty := t.TempDir()
	rep = ReadBoardFreshness(empty, fx.board, FreshnessOptions{Now: fx.now, FlipWindow: fx.window})
	if v := verdictOf(t, rep, "T-S2"); v.Status != RowUnverified || v.Reason != ReasonRepoUnreadable {
		t.Fatalf("complete row in non-git dir: got %q/%q", v.Status, v.Reason)
	}
	if v := verdictOf(t, rep, "T-S3"); v.Status != RowOpen {
		t.Fatalf("open row in non-git dir must stay open, got %q", v.Status)
	}
	if rep.CanVerify {
		t.Fatal("non-git dir: CanVerify must be false")
	}
	if !rep.WorkToSpawn {
		t.Fatal("the open row must raise work-to-spawn even when the repo is unreadable")
	}
	if rep.VerifiablyIdle {
		t.Fatal("idle is impossible without a readable repo")
	}

	// Missing board: zero report, fail-safe.
	rep = ReadBoardFreshness(fx.dir, filepath.Join(fx.dir, "nope.jsonl"), FreshnessOptions{Now: fx.now})
	if rep.TotalRows != 0 || rep.WorkToSpawn || rep.VerifiablyIdle || rep.CanVerify {
		t.Fatalf("missing board must yield the zero report, got %+v", rep)
	}
}

// TestFreshnessAggregates_WorkExistsNotWork: the AC in one line — never
// report "work exists" for a row whose hash resolves at HEAD. Every row
// class with resolving evidence (closed, orphan, mis-attributed,
// flip-window, flip-overdue) is absent from work-to-spawn, while one bare
// open row raises it.
func TestFreshnessAggregates_WorkExistsNotWork(t *testing.T) {
	fx := newFreshFixture(t)
	orphan := fx.orphanAt("orph", fx.workLanded(), "fix: orphan. Addresses T-M2.")
	mis := fx.commitAt("mis", fx.workLanded(), "fix: points at T-M9. (this commit belongs to T-M9)")
	real := fx.commitAt("real", fx.workLanded(), "feat: real. Addresses T-M4.")
	inWin := fx.commitAt("inwin", fx.workLanded(), "feat: inwin. Addresses T-M5.")
	overdue := fx.commitAt("overdue", fx.oldLanded(), "feat: overdue. Addresses T-M6.")

	fx.writeBoard(
		freshRow("T-M4", "complete", map[string]string{"commit_hash": real, "completed_at": "2026-09-15 11:00:00"}),
		freshRow("T-M9", "complete", map[string]string{"commit_hash": mis, "completed_at": "2026-09-15 11:00:00"}),
		freshRow("T-M3", "complete", map[string]string{"commit_hash": mis, "completed_at": "2026-09-15 11:00:00"}),
		freshRow("T-M2", "complete", map[string]string{"commit_hash": orphan, "completed_at": "2026-09-15 11:00:00"}),
		freshRow("T-M5", "pending", map[string]string{"commit_hash": inWin}),
		freshRow("T-M6", "pending", map[string]string{"commit_hash": overdue}),
		freshRow("T-M0", "pending", nil),
	)
	rep := fx.read()
	classes := map[string]RowStatus{}
	for _, v := range rep.Rows {
		classes[v.ID] = v.Status
	}
	if classes["T-M4"] != RowClosed || classes["T-M9"] != RowClosed ||
		classes["T-M2"] != RowUnverified ||
		classes["T-M3"] != RowUnverified ||
		classes["T-M5"] != RowFlipWindow ||
		classes["T-M6"] != RowFlipOverdue ||
		classes["T-M0"] != RowOpen {
		t.Fatalf("class mix wrong: %+v", classes)
	}
	if !rep.WorkToSpawn {
		t.Fatal("the one bare open row must raise work-to-spawn")
	}
	if rep.VerifiablyIdle {
		t.Fatal("mixed board can never be idle")
	}
}

// TestFreshnessFixturesExcluded: registry-declared, perpetual-flagged and
// NEVER-DONE-family rows are fixtures (ADV-R05 layers) — never work,
// never idle-blockers, regardless of status.
func TestFreshnessFixturesExcluded(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "chore: closes T-F1.")
	reg := filepath.Join(fx.dir, ".coding-hermes", "board", "fixtures.jsonl")
	if err := os.WriteFile(reg, []byte(`{"id":"E2E-001","active":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.writeBoard(
		freshRow("E2E-001", "pending", nil), // registry fixture
		freshRow("T-F2", "pending", map[string]string{"perpetual": "true"}),
		freshRow("NEVER-DONE", "complete", nil), // id-family fixture
		freshRow("T-F1", "complete", map[string]string{"commit_hash": short, "completed_at": "2026-09-15 10:00:00"}),
	)
	rep := fx.read()
	for _, id := range []string{"E2E-001", "T-F2", "NEVER-DONE"} {
		if v := verdictOf(t, rep, id); v.Status != RowFixture {
			t.Fatalf("%s should be a fixture, got %q", id, v.Status)
		}
	}
	if v := verdictOf(t, rep, "T-F1"); v.Status != RowClosed {
		t.Fatalf("T-F1 should close, got %q", v.Status)
	}
	if !rep.VerifiablyIdle {
		t.Fatal("fixtures + verified closure should be idle")
	}
	if rep.WorkToSpawn {
		t.Fatal("fixtures never raise work-to-spawn")
	}
}

// TestFreshnessMarkdownBoard: legacy markdown boards: open headers raise
// work; checked headers are unverified (no evidence fields in markdown);
// fixtures excluded; idle unreachable.
func TestFreshnessMarkdownBoard(t *testing.T) {
	fx := newFreshFixture(t)
	board := filepath.Join(fx.dir, ".coding-hermes", "tasks.md")
	if err := os.WriteFile(board, []byte(
		"## [ ] T-K1 - open thing\n## [x] T-K2 - done thing\n## [ ] NEVER-DONE - audit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := ReadBoardFreshness(fx.dir, board, FreshnessOptions{Now: fx.now, FlipWindow: fx.window})
	if v := verdictOf(t, rep, "T-K1"); v.Status != RowOpen {
		t.Fatalf("markdown open header should be open, got %q", v.Status)
	}
	if v := verdictOf(t, rep, "T-K2"); v.Status != RowUnverified || v.Reason != ReasonNoPointer {
		t.Fatalf("markdown checked header should park no-pointer, got %q/%q", v.Status, v.Reason)
	}
	if v := verdictOf(t, rep, "NEVER-DONE"); v.Status != RowFixture {
		t.Fatalf("markdown NEVER-DONE should be a fixture, got %q", v.Status)
	}
	if !rep.WorkToSpawn || rep.VerifiablyIdle || rep.CanVerify {
		t.Fatalf("markdown board: work=%v idle=%v canVerify=%v, want true/false/false",
			rep.WorkToSpawn, rep.VerifiablyIdle, rep.CanVerify)
	}
}

// TestFreshnessDedupKeepLast: a re-filed id keeps its LAST incarnation
// (the fleet board-scan law).
func TestFreshnessDedupKeepLast(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(
		freshRow("T-D1", "complete", map[string]string{"commit_hash": "", "completed_at": "2026-09-15 10:00:00"}),
		freshRow("T-D1", "pending", nil),
	)
	rep := fx.read()
	if rep.TotalRows != 1 {
		t.Fatalf("dedup: got %d rows, want 1", rep.TotalRows)
	}
	if rep.Rows[0].Status != RowOpen {
		t.Fatalf("re-filed row should keep LAST incarnation (open), got %q", rep.Rows[0].Status)
	}
}

// TestFreshnessIdleRequiresRows: an empty board (0 rows, no malformed
// lines) is not idle-proof — absence of rows is not evidence (fail-safe).
func TestFreshnessIdleRequiresRows(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard()
	rep := fx.read()
	if rep.TotalRows != 0 {
		t.Fatalf("empty board should have no rows, got %d", rep.TotalRows)
	}
	if rep.VerifiablyIdle {
		t.Fatal("zero rows must not prove idle")
	}
	if rep.WorkToSpawn {
		t.Fatal("zero rows: no visible work either")
	}
}

// TestFreshnessUnattributedCommit: a resolving, reachable commit that
// names NO board row id parks as unattributed (distinct from
// mis-attributed).
func TestFreshnessUnattributedCommit(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "fix: nothing board-shaped here.")
	fx.writeBoard(freshRow("T-U1", "complete", map[string]string{
		"commit_hash":  short,
		"completed_at": "2026-09-15 10:00:00",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-U1")
	if v.Status != RowUnverified || v.Reason != ReasonUnattributed {
		t.Fatalf("unattributed: got %q/%q, want %q/%q", v.Status, v.Reason, RowUnverified, ReasonUnattributed)
	}
}

// TestFreshnessAliasAttribution: alias forms still attribute — the
// word-bounded base id matches a dash-suffixed alias (T-A1-OLD) and a
// parenthetical mention, but never a longer id sharing the prefix
// (T-A11 is NOT matched by a T-A1 needle... and a commit naming T-A11
// does not attribute a T-A1 row). Documents what the reader accepts.
func TestFreshnessAliasAttribution(t *testing.T) {
	fx := newFreshFixture(t)
	// Commit explicitly names the alias form T-A1-OLD.
	alias := fx.commitAt("work", fx.oldLanded(), "fix: closes T-A1-OLD properly.")
	fx.writeBoard(freshRow("T-A1", "complete", map[string]string{
		"commit_hash":  alias,
		"completed_at": "2026-09-15 10:00:00",
	}))
	if v := verdictOf(t, fx.read(), "T-A1"); v.Status != RowClosed {
		t.Fatalf("alias T-A1-OLD should attribute row T-A1, got %q/%q", v.Status, v.Reason)
	}

	// A commit naming the LONGER id T-A11 must not attribute row T-A1
	// (word boundary: T-A11 is a different row).
	longer := fx.commitAt("work2", fx.oldLanded(), "fix: closes T-A11 only.")
	fx.writeBoard(
		freshRow("T-A1", "complete", map[string]string{
			"commit_hash":  longer,
			"completed_at": "2026-09-15 10:00:00",
		}),
		freshRow("T-A11", "complete", map[string]string{
			"commit_hash":  longer,
			"completed_at": "2026-09-15 10:00:00",
		}),
	)
	rep := fx.read()
	if v := verdictOf(t, rep, "T-A1"); v.Status != RowUnverified {
		t.Fatalf("commit naming T-A11 must not attribute T-A1, got %q", v.Status)
	}
	if v := verdictOf(t, rep, "T-A11"); v.Status != RowClosed {
		t.Fatalf("commit naming T-A11 should attribute T-A11, got %q", v.Status)
	}
}

// TestFreshnessMalformedPointer: free-text pointers ("out-of-band
// (daemon)" — the live board carries these) fail the hash shape check
// and park as malformed-pointer rather than being probed against git.
func TestFreshnessMalformedPointer(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(freshRow("T-P1", "complete", map[string]string{
		"commit_hash":  "out-of-band (daemon)",
		"completed_at": "2026-09-15 10:00:00",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-P1")
	if v.Status != RowUnverified || v.Reason != ReasonMalformedPointer {
		t.Fatalf("malformed pointer: got %q/%q, want %q/%q",
			v.Status, v.Reason, RowUnverified, ReasonMalformedPointer)
	}
}

// TestFreshnessAbsentHash: a hash-shaped pointer resolving to no object
// (the nonexistent-hash class) parks as absent-hash.
func TestFreshnessAbsentHash(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(freshRow("T-N1", "complete", map[string]string{
		"commit_hash":  "0123456789abcdef0123456789abcdef0123abcf",
		"completed_at": "2026-09-15 10:00:00",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-N1")
	if v.Status != RowUnverified || v.Reason != ReasonAbsentHash {
		t.Fatalf("absent hash: got %q/%q, want %q/%q",
			v.Status, v.Reason, RowUnverified, ReasonAbsentHash)
	}
}

// TestFreshnessFullShaPointer: full 40-char shas (the live board carries
// both forms) resolve the same as short ones.
func TestFreshnessFullShaPointer(t *testing.T) {
	fx := newFreshFixture(t)
	short := fx.commitAt("work", fx.oldLanded(), "feat: full sha. Addresses T-G1.")
	full := strings.TrimSpace(fx.git("rev-parse", "HEAD"))
	fx.writeBoard(freshRow("T-G1", "complete", map[string]string{
		"commit_hash":  full,
		"completed_at": "2026-09-15 10:00:00",
	}))
	rep := fx.read()
	v := verdictOf(t, rep, "T-G1")
	if v.Status != RowClosed {
		t.Fatalf("full-sha pointer should close, got %q/%q (short=%s full=%s)",
			v.Status, v.Reason, short, full)
	}
}
