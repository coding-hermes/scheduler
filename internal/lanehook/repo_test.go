package lanehook

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The {repo} placeholder must never hand a remote URL to a command line that
// treats it as a directory (SCHED-GAP-16987): the live repo-fetch hook runs
// `git -C {repo} fetch`, so a URL repo_url has to resolve to the lane's local
// workdir, and with no local path at all the runner must skip the step with a
// named reason instead of recording state=failed.

func TestRepoPathClassification(t *testing.T) {
	remote := []string{
		"https://github.com/totalwindupflightsystems/gitreins",
		"http://forgejo.internal/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"git://github.com/org/repo",
		"file:///srv/git/repo",
		"git@github.com:totalwindupflightsystems/gitreins.git",
		"user@forgejo.internal:org/repo.git",
	}
	for _, s := range remote {
		if !isRemoteRepoShape(s) {
			t.Errorf("isRemoteRepoShape(%q) = false, want true", s)
		}
		if isLocalRepoPath(s) {
			t.Errorf("isLocalRepoPath(%q) = true, want false", s)
		}
	}
	local := []string{
		"/home/kara/code-factory",
		"/tmp/does/not/exist/yet",
		"relative/checkout",
		"repos-user@example.com", // '@' but no scp-like host:path tail
	}
	for _, s := range local {
		if isRemoteRepoShape(s) {
			t.Errorf("isRemoteRepoShape(%q) = true, want false", s)
		}
		if !isLocalRepoPath(s) {
			t.Errorf("isLocalRepoPath(%q) = false, want true", s)
		}
	}
	if isLocalRepoPath("") {
		t.Error(`isLocalRepoPath("") = true, want false`)
	}
}

// repoFetchProbe writes the argv it received to <dir>/argv.txt, mirroring the
// live repo-fetch hook's `git -C {repo} fetch --prune origin` shape.
func repoFetchProbe(t *testing.T, dir string) string {
	t.Helper()
	return script(t, dir, "repo-fetch.sh", "printf '%s\\n' \"$@\" > "+filepath.Join(dir, "argv.txt"))
}

// A remote repo_url resolves {repo} to the lane's workdir — the local
// checkout the scheduler already knows — so the fetch runs against a path,
// never against the URL.
func TestRepoPlaceholderFallsBackToWorkdir(t *testing.T) {
	dir := t.TempDir()
	probe := repoFetchProbe(t, dir)
	step := Step{ID: "repo-fetch", Phase: PhasePre, Level: LevelGlobal, Target: "*",
		Command:   probe,
		Args:      []string{"-C", "{repo}", "fetch", "--prune", "origin"},
		OnFailure: FailClosed, TimeoutS: 5}
	plan := Plan{Pre: []Step{step}}
	env := Env{TickID: "t-repo-local", Lane: "cf-qa", Repo: "https://github.com/totalwindupflightsystems/gitreins", Workdir: dir}
	if err := NewExecRunner(nil, nil).Pre(context.Background(), env, plan); err != nil {
		t.Fatalf("a local workdir fallback must fetch, not fail: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil {
		t.Fatalf("repo-fetch probe never ran: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, dir) {
		t.Fatalf("probe argv must carry the workdir %s, got:\n%s", dir, got)
	}
	if strings.Contains(got, "https://") {
		t.Fatalf("probe argv must never carry the URL, got:\n%s", got)
	}
}

// A {repo} step with no local path anywhere is a named skip, not a failure:
// nothing is exec'd, the receipt carries state=skipped with reason
// repo-not-local, the tick is not vetoed even under on_failure=closed, the
// following step still runs, and the skip is not retried.
func TestRepoNotLocalSkipsInsteadOfFailed(t *testing.T) {
	dir := t.TempDir()
	probe := repoFetchProbe(t, dir)
	marker := filepath.Join(dir, "second-ran")
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

	var skipReason string
	r := NewExecRunner(db, func(ctx context.Context, sev, msg string, d map[string]any) {
		if msg == "hook.pre.skipped" {
			skipReason, _ = d["reason"].(string)
		}
	})
	plan := Plan{Pre: []Step{
		{ID: "repo-fetch", Phase: PhasePre, Level: LevelGlobal, Target: "*",
			Command: probe, Args: []string{"-C", "{repo}", "fetch", "--prune", "origin"},
			OnFailure: FailClosed, TimeoutS: 5},
		{ID: "second", Phase: PhasePre, Level: LevelLane, Target: "l",
			Command: ok, OnFailure: FailClosed, TimeoutS: 5},
	}}
	env := Env{TickID: "t-repo-remote", Lane: "gitreins-fm",
		Repo: "https://github.com/totalwindupflightsystems/gitreins", Workdir: ""}
	if err := r.Pre(context.Background(), env, plan); err != nil {
		t.Fatalf("a repo-not-local skip must not veto the tick, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "argv.txt")); !os.IsNotExist(err) {
		t.Fatalf("the runner must not exec git with a URL (probe err=%v)", err)
	}
	if !exists(marker) {
		t.Fatal("the PRE step after a repo-not-local skip must still run")
	}
	if skipReason != RepoNotLocalReason {
		t.Fatalf("skipped event reason = %q, want %q", skipReason, RepoNotLocalReason)
	}
	var state, output string
	if err := db.QueryRow(`SELECT state, output FROM hook_runs WHERE hook_id='repo-fetch'`).
		Scan(&state, &output); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" {
		t.Fatalf("receipt state = %q, want skipped (not failed)", state)
	}
	if !strings.Contains(output, RepoNotLocalReason) {
		t.Fatalf("receipt output must name %s, got %q", RepoNotLocalReason, output)
	}
}

// The happy path is untouched: a lane whose repo is a local path still gets
// that exact path in {repo} and a state=ok receipt.
func TestLocalRepoStillFetches(t *testing.T) {
	dir := t.TempDir()
	probe := repoFetchProbe(t, dir)
	repo := filepath.Join(dir, "checkout")
	step := Step{ID: "repo-fetch", Phase: PhasePre, Level: LevelGlobal, Target: "*",
		Command:   probe,
		Args:      []string{"-C", "{repo}", "fetch", "--prune", "origin"},
		OnFailure: FailClosed, TimeoutS: 5}
	plan := Plan{Pre: []Step{step}}
	env := Env{TickID: "t-repo-happy", Lane: "cf-qa", Repo: repo, Workdir: filepath.Join(dir, "wt")}
	if err := NewExecRunner(nil, nil).Pre(context.Background(), env, plan); err != nil {
		t.Fatalf("a local-path lane must still fetch: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil {
		t.Fatalf("repo-fetch probe never ran: %v", err)
	}
	if !strings.Contains(string(raw), repo+"\n") {
		t.Fatalf("probe argv must carry the declared repo path %s verbatim, got:\n%s", repo, raw)
	}
}
