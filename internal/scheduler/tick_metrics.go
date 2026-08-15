package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// modelRate is the per-million-token USD price for a model.
type modelRate struct {
	inPerM  float64 // input $/1M tokens
	outPerM float64 // output $/1M tokens
}

// modelRates holds pricing for models the coding-hermes fleet actually uses.
// Prices are approximate per-1M-token USD rates from public pricing pages as
// of 2026-08. The table is intentionally small — it exists so cost_usd is in
// the right ballpark for fleet reporting, not to be a billing-grade catalog.
// Unknown models fall back to estimateTickCost constants.
var modelRates = map[string]modelRate{
	"deepseek-v4-flash": {0.14, 0.28},
	"deepseek-v4-pro":   {0.27, 1.10},
	"glm-5.2":           {0.15, 0.60},
	"kimi-k3":           {0.60, 2.50},
	"minimax-m3":        {1.00, 3.00},
	"gemma":             {0.10, 0.10},
	"gpt-5.6-luna":      {2.50, 10.00},
	"gpt-5.6-sol":       {2.50, 10.00},
	"gpt-5.6-terra":     {2.50, 10.00},
	"grok-4.5":          {2.00, 10.00},
	"step-3.7-flash":    {0.20, 0.80},
}

// computeCostUSD returns the estimated cost in USD for a tick based on token
// usage and the model's per-1M-token rates. Unknown models fall back to the
// fixed estimate constants (estCostPerIn / estCostPerOut) so cost aggregation
// is never zero.
func computeCostUSD(model string, tokensIn, tokensOut int) float64 {
	rate, ok := modelRates[model]
	if !ok {
		// Fallback: use the fixed per-token estimates so unknown models
		// still produce a non-zero, roughly proportional cost.
		return float64(tokensIn)*estCostPerIn + float64(tokensOut)*estCostPerOut
	}
	return float64(tokensIn)/1e6*rate.inPerM + float64(tokensOut)/1e6*rate.outPerM
}

// gitCommitCountInWindow returns the number of commits in workdir between [since, until].
// Returns 0 for repos without .git or on any git error — it must never block
// the tick completion path. (Named distinctly from gitmetrics.gitCommitCount —
// the spawn-time baseline counter — after the upstream merge.)
func gitCommitCountInWindow(ctx context.Context, workdir, since, until string) int {
	out, err := runGit(ctx, workdir, "rev-list", "--count",
		"--since="+since, "--until="+until, "HEAD")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

// gitFilesChanged returns the number of unique files changed by commits in
// workdir between [since, until]. It diffs the first commit's parent in the
// window against the last commit. Returns 0 for repos without .git, windows
// with zero commits, or any git error.
func gitFilesChanged(ctx context.Context, workdir, since, until string) int {
	// List commits in the window (oldest first).
	oldest := runGitFirst(ctx, workdir, "rev-list",
		"--reverse", "--since="+since, "--until="+until, "HEAD")
	if oldest == "" {
		return 0 // zero commits in window
	}
	newest := runGitFirst(ctx, workdir, "rev-list",
		"--since="+since, "--until="+until, "HEAD")
	if newest == "" || newest == oldest {
		// Single commit: diff against its parent.
		out, err := runGit(ctx, workdir, "diff-tree", "--no-commit-id",
			"--name-only", "-r", oldest)
		if err != nil {
			return 0
		}
		return countUniqueNonEmpty(out)
	}
	// Multiple commits: diff oldest's parent against newest.
	parentOfOldest := runGitFirst(ctx, workdir, "rev-parse", oldest+"^")
	if parentOfOldest == "" {
		parentOfOldest = oldest
	}
	out, err := runGit(ctx, workdir, "diff", "--name-only", parentOfOldest, newest)
	if err != nil {
		return 0
	}
	return countUniqueNonEmpty(out)
}

// runGit executes a git command in workdir and returns stdout as a string.
// Returns "" + error on any failure.
func runGit(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil // suppress — errors are expected for non-repo dirs
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// runGitFirst returns the first line of a git command's stdout, or "" on error.
func runGitFirst(ctx context.Context, workdir string, args ...string) string {
	out, err := runGit(ctx, workdir, args...)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

// countUniqueNonEmpty counts unique non-empty lines in a newline-separated string.
func countUniqueNonEmpty(s string) int {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		seen[line] = struct{}{}
	}
	return len(seen)
}

// countGitChanges runs git commit + file counting over the [start, end] window
// in workdir. Returns (commits, filesChanged). Never panics or blocks — all git
// errors produce (0, 0).
func countGitChanges(workdir string, start, end time.Time) (int, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	since := start.Format(time.RFC3339)
	until := end.Format(time.RFC3339)
	commits := gitCommitCountInWindow(ctx, workdir, since, until)
	files := gitFilesChanged(ctx, workdir, since, until)
	return commits, files
}

// formatCostSummary returns a one-line log-friendly summary string.
func formatCostSummary(model string, tin, tout int, cost float64, commits, files int) string {
	return fmt.Sprintf("model=%s tokens=%d/%d cost=$%.4f commits=%d files=%d",
		model, tin, tout, cost, commits, files)
}
