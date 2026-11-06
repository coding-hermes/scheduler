package lanehook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// script writes a small shell script and returns its path.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// T6: a closed PRE failure vetoes the tick and stops the remaining PRE steps.
func TestPreClosedVeto(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-ran")
	fail := script(t, dir, "fail.sh", "exit 3")
	ok := script(t, dir, "ok.sh", "touch "+marker)
	plan := Plan{Pre: []Step{
		{ID: "fail", Phase: PhasePre, Level: LevelLane, Target: "l", Command: fail, OnFailure: FailClosed, TimeoutS: 5},
		{ID: "second", Phase: PhasePre, Level: LevelLane, Target: "l", Command: ok, OnFailure: FailOpen, TimeoutS: 5},
	}}
	var vetoed int
	r := NewExecRunner(nil, func(ctx context.Context, sev, msg string, d map[string]any) {
		if msg == "hook.pre.veto" {
			vetoed++
		}
	})
	err := r.Pre(context.Background(), Env{TickID: "t1", Lane: "l"}, plan)
	var hf *ErrHookFailed
	if !errors.As(err, &hf) || hf.HookID != "fail" || hf.Code != 3 {
		t.Fatalf("want ErrHookFailed(fail,3), got %v", err)
	}
	if exists(marker) {
		t.Fatal("second PRE step must not run after a closed veto")
	}
	if vetoed != 1 {
		t.Fatalf("want 1 veto event, got %d", vetoed)
	}
}

// T7: an open POST failure is recorded and the remaining steps still run.
func TestPostOpenContinues(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "after-ran")
	fail := script(t, dir, "fail.sh", "exit 7")
	after := script(t, dir, "after.sh", "touch "+marker)
	plan := Plan{Post: []Step{
		{ID: "fail", Phase: PhasePost, Level: LevelLane, Target: "l", Command: fail, OnFailure: FailOpen, TimeoutS: 5},
		{ID: "after", Phase: PhasePost, Level: LevelLane, Target: "l", Command: after, OnFailure: FailOpen, TimeoutS: 5},
	}}
	NewExecRunner(nil, nil).Post(context.Background(), Env{TickID: "t2", Lane: "l"}, plan)
	if !exists(marker) {
		t.Fatal("an open POST failure must not stop the chain")
	}
}

// T17: a closed POST failure skips the remaining steps.
func TestPostClosedAbortsChain(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "prune-ran")
	fail := script(t, dir, "gate.sh", "exit 1")
	prune := script(t, dir, "prune.sh", "touch "+marker)
	plan := Plan{Post: []Step{
		{ID: "gate", Phase: PhasePost, Level: LevelLane, Target: "l", Command: fail, OnFailure: FailClosed, TimeoutS: 5},
		{ID: "prune", Phase: PhasePost, Level: LevelLane, Target: "l", Command: prune, OnFailure: FailOpen, TimeoutS: 5},
	}}
	var aborted int
	r := NewExecRunner(nil, func(ctx context.Context, sev, msg string, d map[string]any) {
		if msg == "hook.post.chain_aborted" {
			aborted++
		}
	})
	r.Post(context.Background(), Env{TickID: "t3", Lane: "l"}, plan)
	if exists(marker) {
		t.Fatal("prune must not run after a closed gate failure")
	}
	if aborted != 1 {
		t.Fatalf("want 1 chain_aborted event, got %d", aborted)
	}
}

// T8: a timeout kills the whole process group, not just the leader.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The shell writes its child's pid, then both sleep; §2.3.2 requires both dead.
	body := fmt.Sprintf("sleep 300 & echo $! > %s; wait", pidFile)
	slow := script(t, dir, "slow.sh", body)
	plan := Plan{Pre: []Step{{
		ID: "slow", Phase: PhasePre, Level: LevelLane, Target: "l",
		Command: slow, OnFailure: FailOpen, TimeoutS: 1,
	}}}
	start := time.Now()
	if err := NewExecRunner(nil, nil).Pre(context.Background(), Env{TickID: "t4", Lane: "l"}, plan); err != nil {
		t.Fatalf("open timeout must not error: %v", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("runner blocked past its timeout: %v", d)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !exists(fmt.Sprintf("/proc/%d", pid)) {
			return // child reaped: the group kill worked
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived the group kill", pid)
}

// T9: retry policy runs up to N attempts and reports the attempt count.
func TestRetryThenSucceed(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	flaky := script(t, dir, "flaky.sh", fmt.Sprintf(`
n=$(cat %s 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > %s
if [ "$n" -lt 3 ]; then exit 1; fi
exit 0`, counter, counter))
	plan := Plan{Pre: []Step{{
		ID: "flaky", Phase: PhasePre, Level: LevelLane, Target: "l",
		Command: flaky, OnFailure: FailRetry, Retries: 3, RetryDelayS: 0, TimeoutS: 5,
	}}}
	if err := NewExecRunner(nil, nil).Pre(context.Background(), Env{TickID: "t5", Lane: "l"}, plan); err != nil {
		t.Fatalf("retry should have succeeded: %v", err)
	}
	raw, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("want 3 attempts, got %q", raw)
	}
}

// T10: an exhausted PRE budget fails the tick and skips the rest.
func TestPreBudgetExhausted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "never")
	slow := script(t, dir, "slow.sh", "sleep 5")
	never := script(t, dir, "never.sh", "touch "+marker)
	_, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel2()

	plan := Plan{Pre: []Step{
		{ID: "slow", Phase: PhasePre, Level: LevelLane, Target: "l", Command: slow, OnFailure: FailOpen, TimeoutS: 3},
		{ID: "never", Phase: PhasePre, Level: LevelLane, Target: "l", Command: never, OnFailure: FailOpen, TimeoutS: 3},
	}}
	err := NewExecRunner(nil, nil).Pre(ctx, Env{TickID: "t6", Lane: "l"}, plan)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %v", err)
	}
	if exists(marker) {
		t.Fatal("step after budget exhaustion must not run")
	}
}

// The env contract reaches the child.
func TestHookEnvReachesChild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	dump := script(t, dir, "dump.sh", "env | grep '^CODING_HERMES_' | sort > "+out)
	plan := Plan{Pre: []Step{{
		ID: "dump", Phase: PhasePre, Level: LevelLane, Target: "cf-qa",
		Command: dump, OnFailure: FailClosed, TimeoutS: 5,
	}}}
	env := Env{TickID: "tick-7", Lane: "cf-qa", Project: "cf", Role: "qa",
		Namespace: "cf", Workdir: dir, Repo: "/r", Deadline: time.Now().Add(time.Hour)}
	if err := NewExecRunner(nil, nil).Pre(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"CODING_HERMES_TICK=tick-7", "CODING_HERMES_LANE=cf-qa",
		"CODING_HERMES_ROLE=qa", "CODING_HERMES_HOOK=1",
		"CODING_HERMES_HOOK_ID=dump", "CODING_HERMES_PHASE=pre",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("child env missing %q in:\n%s", want, got)
		}
	}
}

// A missing command fails the step instead of hanging the runner.
func TestMissingCommandFails(t *testing.T) {
	plan := Plan{Pre: []Step{{
		ID: "gone", Phase: PhasePre, Level: LevelLane, Target: "l",
		Command: filepath.Join(t.TempDir(), "nope"), OnFailure: FailClosed, TimeoutS: 2,
	}}}
	err := NewExecRunner(nil, nil).Pre(context.Background(), Env{TickID: "t8", Lane: "l"}, plan)
	var hf *ErrHookFailed
	if !errors.As(err, &hf) {
		t.Fatalf("want ErrHookFailed, got %v", err)
	}
}

// SCHED-GAP-16991: exit 10 is the hook contract's named "nothing to do"
// skip (worktree-ensure returns it when the lane's workdir IS the repo's
// primary checkout). A skipped PRE step must not fail the tick (no
// ErrHookFailed), must not stop the following PRE step, must not be
// retried even under a retry policy, and its receipt row must carry
// state=skipped with the child's captured output.
func TestPreExit10SkipIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "second-ran")
	counter := filepath.Join(dir, "count")
	skip := script(t, dir, "skip.sh", fmt.Sprintf(`
n=$(cat %s 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > %s
echo skip reason=workdir-is-primary-checkout >&2
exit 10`, counter, counter))
	ok := script(t, dir, "ok.sh", "touch "+marker)

	db, err := sql.Open("sqlite", filepath.Join(dir, "receipts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Schema-faithful fixture for the hook_runs receipt (migration v81+v82):
	// exactly the columns the runner writes.
	_, err = db.Exec(`CREATE TABLE hook_runs (
		run_id       INTEGER PRIMARY KEY AUTOINCREMENT,
		tick_id      TEXT NOT NULL,
		lane         TEXT NOT NULL,
		phase        TEXT NOT NULL,
		level        TEXT NOT NULL,
		hook_id      TEXT NOT NULL,
		priority     INTEGER NOT NULL,
		seq          INTEGER NOT NULL,
		command      TEXT NOT NULL,
		argv         TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL,
		exit_code    INTEGER NOT NULL DEFAULT 0,
		started_at   TEXT NOT NULL,
		finished_at  TEXT,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		attempts     INTEGER NOT NULL DEFAULT 1,
		output       TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal(err)
	}

	var skipEvents int
	r := NewExecRunner(db, func(ctx context.Context, sev, msg string, d map[string]any) {
		if msg == "hook.pre.skipped" {
			skipEvents++
		}
	})
	plan := Plan{Pre: []Step{
		{ID: "skip", Phase: PhasePre, Level: LevelLane, Target: "l",
			Command: skip, OnFailure: FailRetry, Retries: 3, TimeoutS: 5},
		{ID: "second", Phase: PhasePre, Level: LevelLane, Target: "l",
			Command: ok, OnFailure: FailClosed, TimeoutS: 5},
	}}
	if err := r.Pre(context.Background(), Env{TickID: "t-skip", Lane: "l"}, plan); err != nil {
		t.Fatalf("an exit-10 skip must not fail the tick, got %v", err)
	}
	if !exists(marker) {
		t.Fatal("the PRE step after a skip must still run")
	}
	if raw, _ := os.ReadFile(counter); strings.TrimSpace(string(raw)) != "1" {
		t.Fatalf("a skip must not be retried, got %q attempt(s)", raw)
	}
	var state string
	var code int
	var output string
	if err := db.QueryRow(`SELECT state, exit_code, output FROM hook_runs WHERE hook_id='skip'`).
		Scan(&state, &code, &output); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" {
		t.Fatalf("receipt state = %q, want skipped", state)
	}
	if code != ExitSkip {
		t.Fatalf("receipt exit_code = %d, want %d", code, ExitSkip)
	}
	if !strings.Contains(output, "reason=workdir-is-primary-checkout") {
		t.Fatalf("receipt output lost the child's stderr: %q", output)
	}
	if skipEvents != 1 {
		t.Fatalf("want 1 hook.pre.skipped event, got %d", skipEvents)
	}
}

// PERF-006: quiesce-check exit 11 is a named BUSY outcome, not exit 10's
// "nothing to do" skip. The receipt and event must say busy, and a closed
// POST gate must still stop merge/prune.
func TestPostExit11BusyStopsChainAndReceipt(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "merge-ran")
	busy := script(t, dir, "busy.sh", "echo 'busy: 123:cwd' >&2; exit 11")
	merge := script(t, dir, "merge.sh", "touch "+marker)

	db, err := sql.Open("sqlite", filepath.Join(dir, "receipts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE hook_runs (
		run_id       INTEGER PRIMARY KEY AUTOINCREMENT,
		tick_id      TEXT NOT NULL,
		lane         TEXT NOT NULL,
		phase        TEXT NOT NULL,
		level        TEXT NOT NULL,
		hook_id      TEXT NOT NULL,
		priority     INTEGER NOT NULL,
		seq          INTEGER NOT NULL,
		command      TEXT NOT NULL,
		argv         TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL CHECK(state IN ('running','ok','failed','timeout','skipped','busy')),
		exit_code    INTEGER NOT NULL DEFAULT 0,
		started_at   TEXT NOT NULL,
		finished_at  TEXT,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		attempts     INTEGER NOT NULL DEFAULT 1,
		output       TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal(err)
	}

	var busyEvents int
	r := NewExecRunner(db, func(ctx context.Context, sev, msg string, d map[string]any) {
		if msg == "hook.post.busy" {
			busyEvents++
		}
	})
	plan := Plan{Post: []Step{
		{ID: "quiesce-check", Phase: PhasePost, Level: LevelLane, Target: "l",
			Command: busy, OnFailure: FailClosed, TimeoutS: 5},
		{ID: "lane-merge", Phase: PhasePost, Level: LevelLane, Target: "l",
			Command: merge, OnFailure: FailOpen, TimeoutS: 5},
	}}
	r.Post(context.Background(), Env{TickID: "t-busy", Lane: "l"}, plan)
	if exists(marker) {
		t.Fatal("lane-merge ran after a closed busy gate")
	}
	if busyEvents != 1 {
		t.Fatalf("want 1 hook.post.busy event, got %d", busyEvents)
	}
	var state, output string
	var code int
	if err := db.QueryRow(`SELECT state, exit_code, output FROM hook_runs WHERE hook_id='quiesce-check'`).
		Scan(&state, &code, &output); err != nil {
		t.Fatal(err)
	}
	if state != "busy" || code != ExitBusy {
		t.Fatalf("receipt = state %q exit %d, want busy/%d", state, code, ExitBusy)
	}
	if !strings.Contains(output, "busy: 123:cwd") {
		t.Fatalf("receipt lost busy output: %q", output)
	}
}

// moduleRoot walks up from the test's package dir to the go.mod that
// anchors the module, so the cross-compile guard can build the whole
// module the way the board-row gates do.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the lanehook package")
		}
		dir = parent
	}
}

// TestLanehookCrossCompileWindows is the INT-CI-4184 guard: runner.go once
// used syscall.SysProcAttr{Setpgid: true} and syscall.Kill without build
// tags, so GOOS=windows go build ./... failed repo-wide and only a tagged
// multiarch release would have noticed. This shells the REAL compiler for
// both windows targets on every `go test` run, so a regression fails
// loudly in the suite instead of silently. CGO stays off because the
// module is pure Go (modernc.org/sqlite), matching the multiarch
// workflow's cross-build contract; the platform split itself lives in
// runner_unix.go / runner_windows.go.
func TestLanehookCrossCompileWindows(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("cross-compile guard needs the go toolchain on PATH: %v", err)
	}
	root := moduleRoot(t)
	for _, arch := range []string{"amd64", "arm64"} {
		build := exec.Command(goBin, "build", "./...")
		build.Dir = root
		build.Env = append(os.Environ(),
			"GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0")
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("GOOS=windows GOARCH=%s go build ./... failed: %v\n%s", arch, berr, out)
		}
	}
}
