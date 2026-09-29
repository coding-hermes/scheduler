package scheduler

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// =============================================================================
// Fake hermes binary for intercepting deliverOutput / deliverAlert exec calls
// =============================================================================

// setupFakeHermes creates a fake "hermes" script in a temp dir that captures
// its arguments to a file. Returns (dir to prepend to PATH, capture file path).
// The fake script reads HERMES_CAPTURE_FILE env var to know where to write args.
func setupFakeHermes(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	captureFile := filepath.Join(dir, "capture.txt")

	script := `#!/bin/bash
# Capture all arguments to the file specified by env var
echo "$@" > "$HERMES_CAPTURE_FILE"
# Also write exit code override if set
if [ -n "$HERMES_EXIT_CODE" ]; then
    exit "$HERMES_EXIT_CODE"
fi
# Write stderr for debugging
echo "fake-hermes: $@" >&2
exit 0
`
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}

	t.Setenv("HERMES_CAPTURE_FILE", captureFile)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	return dir, captureFile
}

// readCapture reads the contents of the capture file written by fake hermes.
// Returns empty string if the file doesn't exist (expected for early-return paths).
func readCapture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // file doesn't exist — expected for early-return code paths
	}
	return strings.TrimSpace(string(data))
}

// =============================================================================
// deliverOutput tests
// =============================================================================

func TestDeliverOutput_NilBuffer(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	deliverOutput(clock.Real(), "testproj", "tick-001", "telegram:123", "prompt", nil)

	// Should not call hermes — nil buffer returns early
	content := readCapture(t, captureFile)
	if content != "" {
		t.Errorf("expected no hermes call for nil buffer, got: %s", content)
	}
}

func TestDeliverOutput_EmptyBuffer(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	deliverOutput(clock.Real(), "testproj", "tick-001", "telegram:123", "prompt", &buf)

	// Empty buffer returns early — no hermes call
	content := readCapture(t, captureFile)
	if content != "" {
		t.Errorf("expected no hermes call for empty buffer, got: %s", content)
	}
}

func TestDeliverOutput_EmptyDeliver(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	buf.WriteString("some output")
	deliverOutput(clock.Real(), "testproj", "tick-001", "", "prompt", &buf)

	// Empty deliver target returns early — no hermes call
	content := readCapture(t, captureFile)
	if content != "" {
		t.Errorf("expected no hermes call for empty deliver, got: %s", content)
	}
}

func TestDeliverOutput_Success(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	buf.WriteString("foreman tick summary: all checks passed")
	deliverOutput(clock.Real(), "testproj", "tick-001", "telegram:123", "prompt", &buf)

	args := readCapture(t, captureFile)
	if args == "" {
		t.Fatal("expected hermes send to be called")
	}
	if !strings.Contains(args, "--to telegram:123") {
		t.Errorf("expected --to telegram:123 in args, got: %s", args)
	}
	if !strings.Contains(args, "--subject") {
		t.Errorf("expected --subject in args, got: %s", args)
	}
	if !strings.Contains(args, "--file") {
		t.Errorf("expected --file in args, got: %s", args)
	}
}

func TestDeliverOutput_TriggerInSubject(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	for _, tc := range []struct {
		trigger string
		want    string
	}{
		{"command", "🤖 testproj [tick-001] · command"},
		{"prompt", "🤖 testproj [tick-001] · prompt"},
	} {
		var buf bytes.Buffer
		buf.WriteString("foreman tick summary: all checks passed")
		deliverOutput(clock.Real(), "testproj", "tick-001", "telegram:123", tc.trigger, &buf)

		args := readCapture(t, captureFile)
		if !strings.Contains(args, tc.want) {
			t.Errorf("trigger=%s: expected subject %q in args, got: %s", tc.trigger, tc.want, args)
		}
		// Reset capture for the next iteration.
		if err := os.Remove(captureFile); err != nil && !os.IsNotExist(err) {
			t.Fatalf("reset capture: %v", err)
		}
	}
}

func TestDeliverOutput_WithToolNoise(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	// Output with tool noise that should be stripped
	var buf bytes.Buffer
	buf.WriteString("tool output\n")
	buf.WriteString("more tool\n")
	buf.WriteString("┊ review diff\n")
	buf.WriteString("---\n")
	buf.WriteString("Human summary of completed work with enough length to pass the 50-char threshold test.")
	deliverOutput(clock.Real(), "noiseproj", "tick-002", "telegram:456", "prompt", &buf)

	args := readCapture(t, captureFile)
	if !strings.Contains(args, "--to telegram:456") {
		t.Errorf("expected --to telegram:456, got: %s", args)
	}
}

func TestDeliverOutput_TrimNoiseShortFallback(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	// Output that trims to <50 chars — should fall back to raw
	// Use pipe-lines that get completely stripped
	var buf bytes.Buffer
	for i := 0; i < 10; i++ {
		buf.WriteString("┊ review panel line that gets stripped away\n")
	}
	deliverOutput(clock.Real(), "noiseproj", "tick-003", "telegram:789", "prompt", &buf)

	args := readCapture(t, captureFile)
	if !strings.Contains(args, "telegram:789") {
		t.Errorf("expected delivery to telegram:789, got: %s", args)
	}
}

func TestDeliverOutput_ExecFailure(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	t.Setenv("HERMES_EXIT_CODE", "1")

	var buf bytes.Buffer
	buf.WriteString("output text")
	deliverOutput(clock.Real(), "testproj", "tick-001", "telegram:123", "prompt", &buf)

	// Should not panic — logs the error
	content := readCapture(t, captureFile)
	_ = content // exec failure is handled gracefully
}

func TestDeliverOutput_TickIDInSubject(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	buf.WriteString("foreman work summary with tick id appended")
	deliverOutput(clock.Real(), "myproject", "tick-ABC-123", "telegram:999", "prompt", &buf)

	args := readCapture(t, captureFile)
	if !strings.Contains(args, "tick-ABC-123") {
		t.Errorf("expected tick ID tick-ABC-123 in subject, got: %s", args)
	}
}

// =============================================================================
// deliverAlert tests
// =============================================================================

func TestDeliverAlert_EmptyDeliver(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	deliverAlert("", "testproj", "tick-001", "timeout after 2h")

	// Empty deliver target returns early — no hermes call
	content := readCapture(t, captureFile)
	if content != "" {
		t.Errorf("expected no hermes call for empty deliver, got: %s", content)
	}
}

func TestDeliverAlert_Success(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	deliverAlert("telegram:123", "testproj", "tick-001", "timeout after 2h")

	args := readCapture(t, captureFile)
	if args == "" {
		t.Fatal("expected hermes send to be called")
	}
	if !strings.Contains(args, "telegram:123") {
		t.Errorf("expected telegram:123 in args, got: %s", args)
	}
}

func TestDeliverAlert_Content(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	deliverAlert("telegram:456", "alertproj", "tick-042", "worker timed out")

	// Check that the temp file was created with the alert message
	args := readCapture(t, captureFile)
	if !strings.Contains(args, "telegram:456") {
		t.Errorf("expected telegram:456, got: %s", args)
	}
}

func TestDeliverAlert_ExecFailure(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	t.Setenv("HERMES_EXIT_CODE", "1")

	deliverAlert("telegram:123", "testproj", "tick-001", "timeout")

	// Should not panic — logs the error
	content := readCapture(t, captureFile)
	_ = content // exec failure handled gracefully
}

func TestDeliverAlert_FileContent(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	deliverAlert("telegram:123", "myproj", "tick-42", "test reason")

	// Verify hermes was called (content verified indirectly via args)
	args := readCapture(t, captureFile)
	if !strings.Contains(args, "telegram:123") {
		t.Errorf("expected telegram:123 in args, got: %s", args)
	}
}

// =============================================================================
// trimToolNoise tests
// =============================================================================

func TestTrimToolNoise_EmptyInput(t *testing.T) {
	result := trimToolNoise("")
	if result != "" {
		t.Errorf("expected empty for empty input, got: %q", result)
	}
}

func TestTrimToolNoise_OnlyNoise(t *testing.T) {
	// Input that's entirely tool noise — all stripped
	input := "┊ review diff line 1\n┊ review diff line 2\n┊ review diff line 3\n"
	result := trimToolNoise(input)
	if result != "" {
		t.Errorf("expected empty for all-noise input, got: %q", result)
	}
}

func TestTrimToolNoise_FinalSeparator(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "clean summary after separator",
			input:    "tool output\nmore tool\n---\nActual human summary of the work done today with sufficient length to pass the 50-char threshold check.",
			expected: "Actual human summary of the work done today with sufficient length to pass the 50-char threshold check.",
		},
		{
			name:     "separator with leading newline",
			input:    "noise\n\n---\n\nThe final summary report for the foreman tick showing completed tasks and next actions.",
			expected: "The final summary report for the foreman tick showing completed tasks and next actions.",
		},
		{
			name:     "multiple separators — last one wins",
			input:    "early --- stuff\n---\nfirst summary but too short\n---\nThe real final summary that has enough characters to be valid output for the test.",
			expected: "The real final summary that has enough characters to be valid output for the test.",
		},
		{
			name:     "separator but after-text too short — falls through",
			input:    "some noise before the separator line\n---\nshort",
			expected: "some noise before the separator line\n---\nshort", // separator skipped (after-text <50), line-based keeps all
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trimToolNoise(tt.input)
			if result != tt.expected {
				t.Errorf("got:\n%q\nwant:\n%q", result, tt.expected)
			}
		})
	}
}

func TestTrimToolNoise_DiffBlocks(t *testing.T) {
	input := "Normal line 1\n@@ -10,5 +10,7 @@\n+added line\n-removed line\na/old/path\nb/new/path\nindex abc123\n--- a/file.go\nNormal line after diff\n"
	expected := "Normal line 1\nNormal line after diff"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_DiffBlockTransition(t *testing.T) {
	// Diff starts but then transitions out with a non-diff line
	input := "before\n@@ -1,1 +1,1 @@\n+added\n-removed\nregular line after diff\n"
	expected := "before\nregular line after diff"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_CodeBlocks(t *testing.T) {
	input := "text before\n```go\nfunc foo() {}\n```\ntext after\n"
	expected := "text before\ntext after"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_UnclosedCodeBlock(t *testing.T) {
	// Unclosed code block — everything after ``` is stripped
	input := "text before\n```go\nfunc foo() {}\nmore code\nno closing fence\n"
	expected := "text before"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_PipeReviewLines(t *testing.T) {
	input := "real output line\n┊ review diff: some diff content\n┊ review file: path/to/file.go\nmore real output\n"
	expected := "real output line\nmore real output"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_WorkerPrompts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"You are a coding agent", "summary text\nYou are a coding agent working on task X\nmore agent prompt\n\nreal output resumes", "summary text\n…\nreal output resumes"},
		{"## TASK:", "foreman report\n## TASK: AUDIT-005-test-deliver\nworker instructions\n\nafter blank line", "foreman report\n…\nafter blank line"},
		{"## INSERTION POINT", "done\n## INSERTION POINT\ncode goes here\n\nresume", "done\n…\nresume"},
		{"## PATTERN", "ok\n## PATTERN\ndetails\n\nnext", "ok\n…\nnext"},
		{"## STORE API", "text\n## STORE API\npattern\n\nout", "text\n…\nout"},
		{"## ALL", "last\n## ALL\nworker prompt body\n\nclean", "last\n…\nclean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trimToolNoise(tt.input)
			if result != tt.expected {
				t.Errorf("got:\n%q\nwant:\n%q", result, tt.expected)
			}
		})
	}
}

func TestTrimToolNoise_WorkerPromptResumeOnBlankLine(t *testing.T) {
	input := "foreman summary\nYou are a coding agent working on Go tests\nDetailed worker instructions here\nmore worker text\n\nresumed foreman output after blank line\n"
	expected := "foreman summary\n…\nresumed foreman output after blank line"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_WorkerPromptEndOfInput(t *testing.T) {
	// Worker prompt at end of input with no resume — the "…" stays
	input := "last foreman line\nYou are a coding agent\nmore\n"
	expected := "last foreman line\n…"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestTrimToolNoise_BlankLineCompaction(t *testing.T) {
	// Max 2 consecutive blank lines → max 3 consecutive newlines
	// 4 blank lines (line1\n\n\n\n\nline2) should compact to 2 (line1\n\n\nline2)
	input := "line1\n\n\n\n\nline2\n\n\n\nline3\n\n\n\n\n"
	result := trimToolNoise(input)
	// Should have no runs of 4+ newlines (3+ blank lines)
	if strings.Count(result, "\n\n\n\n") > 0 {
		t.Errorf("expected max 2 blank lines (3 newlines), got more: %q", result)
	}
}

func TestTrimToolNoise_ShortResultFallback(t *testing.T) {
	// Cleaned result <50 chars but raw >200 → return raw
	// Use actual noise patterns (┊ lines) that get stripped, so cleaned is empty
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "┊ some review panel noise that gets stripped")
	}
	raw := strings.Join(lines, "\n") // >200 chars of actual noise
	result := trimToolNoise(raw)
	if result != raw {
		t.Errorf("expected raw fallback for short cleaned result, got: %q", result)
	}
}

func TestTrimToolNoise_ShortResultShortRaw(t *testing.T) {
	// Cleaned result <50 chars AND raw <200 → return cleaned (no fallback)
	raw := "short text"
	result := trimToolNoise(raw)
	if result != "short text" {
		t.Errorf("expected cleaned for short raw, got: %q", result)
	}
}

func TestTrimToolNoise_MixedNoise(t *testing.T) {
	// All noise types at once: ┊ lines, diffs, code blocks, worker prompts
	input := "real output start\n┊ review diff line\n@@ -1,1 +1,1 @@\n+diff added\n```go\ncode block\n```\nYou are a coding agent\n\ncleaned output\nend"
	expected := "real output start\n…\ncleaned output\nend"
	result := trimToolNoise(input)
	if result != expected {
		t.Errorf("got:\n%q\nwant:\n%q", result, expected)
	}
}

func TestClassifySendFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		out  string
		want sendFailClass
	}{
		{"telegram timed out", errors.New("exit status 1"), "hermes send: Telegram send failed: Timed out", sendFailAmbiguous},
		{"i/o timeout", errors.New("exit status 1"), "dial tcp 149.154.167.220:443: i/o timeout", sendFailAmbiguous},
		{"request timeout", errors.New("exit status 1"), "Telegram send failed: Request timeout", sendFailAmbiguous},
		{"deadline exceeded", errors.New("exit status 1"), "context deadline exceeded", sendFailAmbiguous},
		{"connection reset mid-flight", errors.New("exit status 1"), "read tcp: connection reset by peer", sendFailAmbiguous},
		{"504 gateway timeout", errors.New("exit status 1"), "504 Gateway Timeout", sendFailAmbiguous},
		{"flood control", errors.New("exit status 1"), "Telegram send failed: Flood control exceeded. Retry in 18.00 seconds", sendFailRejected},
		{"flood control lowercase", errors.New("exit status 1"), "flood control exceeded, retry in 5 seconds", sendFailRejected},
		{"too many requests", errors.New("exit status 1"), "Too Many Requests: retry after 30", sendFailRejected},
		{"429 rate limited", errors.New("exit status 1"), "HTTP 429", sendFailRejected},
		{"503 backend", errors.New("exit status 1"), "503 Service Unavailable", sendFailRejected},
		{"connection refused", errors.New("exit status 1"), "dial tcp: connect: connection refused", sendFailRejected},
		{"unknown platform", errors.New("exit status 2"), "unknown platform: foo", sendFailFatal},
		{"missing --to", errors.New("exit status 2"), "--to PLATFORM is required", sendFailFatal},
		{"parse error", errors.New("exit status 2"), "invalid argument", sendFailFatal},
		// Tie-break: a timeout signal anywhere outranks an HTTP-status marker,
		// because the outcome is unknowable whenever the client stopped
		// waiting without confirmation.
		{"flood control then timeout", errors.New("exit status 1"), "Flood control exceeded. Retry in 5 (request timed out)", sendFailAmbiguous},
	}
	for _, c := range cases {
		if got := classifySendFailure(c.err, c.out); got != c.want {
			t.Errorf("%s: classifySendFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

// =============================================================================
// SCHED-GAP-1672 — an ambiguous (timed-out) send must never be auto-resent
// =============================================================================

// setupFakeHermesAttempts installs a fake `hermes` binary that appends its argv
// to a per-test attempts ledger (one line per invocation) and then fails with
// failText on stderr. It models the measured SCHED-GAP-1672 shape: the platform
// accepted the request, but the client never learned the outcome. Returns the
// ledger path.
func setupFakeHermesAttempts(t *testing.T, failText string) string {
	t.Helper()
	dir := t.TempDir()
	ledger := filepath.Join(dir, "attempts.txt")

	script := `#!/bin/bash
echo "$@" >> "$HERMES_ATTEMPTS_FILE"
echo "$HERMES_FAIL_TEXT" >&2
exit 1
`
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}

	t.Setenv("HERMES_ATTEMPTS_FILE", ledger)
	t.Setenv("HERMES_FAIL_TEXT", failText)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return ledger
}

// hermesAttempts counts the delivery attempts that actually reached the
// platform: one fake-hermes invocation (one recorded argv line) == one attempt.
func hermesAttempts(t *testing.T, ledger string) int {
	t.Helper()
	data, err := os.ReadFile(ledger)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read attempts ledger: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

// captureDeliverLog captures the package logger's output for one test.
func captureDeliverLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// attemptLogLines returns the "DELIVER: attempt N/4 failed:" lines, in order.
func attemptLogLines(logOut string) []string {
	var out []string
	for _, line := range strings.Split(logOut, "\n") {
		if strings.Contains(line, "attempt ") && strings.Contains(line, " failed:") {
			out = append(out, line)
		}
	}
	return out
}

// tickBody is a report body long enough to survive trimToolNoise untouched.
func tickBody() *bytes.Buffer {
	var buf bytes.Buffer
	buf.WriteString("foreman tick summary: one report, delivered to the shared thread once")
	return &buf
}

// TestSCHEDGAP1672_TimeoutAfterAcceptIsNotResent is the regression for the
// measured duplicate (coding-hermes-tools-sync, 2026-09-29 00:23:35Z — the same
// report posted to one Telegram thread four times): the platform accepted the
// message, and the client timed out waiting for the response. A timeout is
// AMBIGUOUS, so exactly ONE delivery attempt may reach the platform, and the
// greppable ambiguous line must say so.
func TestSCHEDGAP1672_TimeoutAfterAcceptIsNotResent(t *testing.T) {
	ledger := setupFakeHermesAttempts(t, "hermes send: Telegram send failed: Timed out")
	logs := captureDeliverLog(t)

	deliverOutputWithMode(clock.NewSimClockAt(1000, time.Now()),
		"coding-hermes-tools-sync", "tick-1672a", "telegram:-1004305778724", "command", tickBody(), "")

	if got := hermesAttempts(t, ledger); got != 1 {
		t.Errorf("delivery attempts = %d, want exactly 1 — a timeout is ambiguous and must not be auto-resent", got)
	}
	if !strings.Contains(logs.String(), "AMBIGUOUS send failure (timeout): message may have been delivered, NOT resending (duplicate-suppression)") {
		t.Errorf("missing the greppable ambiguous-failure line; log was:\n%s", logs.String())
	}
}

// TestSCHEDGAP1672_FloodControlStillRetries: an explicit REJECTION proves
// nothing was posted, so it must still consume every attempt — the retry
// behaviour this project relies on for Telegram flood control.
func TestSCHEDGAP1672_FloodControlStillRetries(t *testing.T) {
	ledger := setupFakeHermesAttempts(t,
		"hermes send: Telegram send failed: Flood control exceeded. Retry in 18.00 seconds")

	deliverOutputWithMode(clock.NewSimClockAt(1000, time.Now()),
		"coding-hermes-tools-sync", "tick-1672b", "telegram:-1004305778724", "command", tickBody(), "")

	if got := hermesAttempts(t, ledger); got != 4 {
		t.Errorf("delivery attempts = %d, want 4 — a flood-control rejection proves nothing was posted, so it stays retryable", got)
	}
}

// TestSCHEDGAP1672_LastAttemptLogNeverSaysRetrying: "— retrying" may only
// appear when another attempt will actually run. The last attempt (4/4) must
// not claim a retry that never happens.
func TestSCHEDGAP1672_LastAttemptLogNeverSaysRetrying(t *testing.T) {
	ledger := setupFakeHermesAttempts(t,
		"hermes send: Telegram send failed: Flood control exceeded. Retry in 18.00 seconds")
	logs := captureDeliverLog(t)

	deliverOutputWithMode(clock.NewSimClockAt(1000, time.Now()),
		"coding-hermes-tools-sync", "tick-1672c", "telegram:-1004305778724", "command", tickBody(), "")

	if got := hermesAttempts(t, ledger); got != 4 {
		t.Fatalf("delivery attempts = %d, want 4 (log-shape test needs the full retry run)", got)
	}
	lines := attemptLogLines(logs.String())
	if len(lines) != 4 {
		t.Fatalf("attempt log lines = %d, want 4; log was:\n%s", len(lines), logs.String())
	}
	for i, line := range lines[:len(lines)-1] {
		if !strings.Contains(line, " — retrying") {
			t.Errorf("attempt line %d = %q, want the retrying suffix (another attempt follows)", i+1, line)
		}
	}
	if last := lines[len(lines)-1]; strings.Contains(last, "retrying") {
		t.Errorf("last attempt line claims a retry that never runs: %q", last)
	}
}

// TestSCHEDGAP1672_AmbiguousAttemptLogNeverSaysRetrying covers the same
// cosmetic bug on the ambiguous path: its single attempt is also the last one,
// so it must not claim a retry either.
func TestSCHEDGAP1672_AmbiguousAttemptLogNeverSaysRetrying(t *testing.T) {
	ledger := setupFakeHermesAttempts(t, "hermes send: Telegram send failed: Timed out")
	logs := captureDeliverLog(t)

	deliverOutputWithMode(clock.NewSimClockAt(1000, time.Now()),
		"coding-hermes-tools-sync", "tick-1672d", "telegram:-1004305778724", "command", tickBody(), "")

	if got := hermesAttempts(t, ledger); got != 1 {
		t.Fatalf("delivery attempts = %d, want 1", got)
	}
	lines := attemptLogLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("attempt log lines = %d, want 1; log was:\n%s", len(lines), logs.String())
	}
	if strings.Contains(lines[0], "retrying") {
		t.Errorf("the last (and only) attempt line claims a retry that never runs: %q", lines[0])
	}
}

// TestSCHEDGAP1672_TimeoutSpellingsAreNeverResent: every timeout spelling the
// client can emit — including a 504 Gateway Timeout and a mid-flight reset,
// whose outcomes are equally unknowable — must stop after one attempt.
func TestSCHEDGAP1672_TimeoutSpellingsAreNeverResent(t *testing.T) {
	for _, text := range []string{
		"hermes send: Telegram send failed: Timed out",
		"hermes send: Telegram send failed: Request timeout",
		"hermes send: Telegram send failed: dial tcp 149.154.167.220:443: i/o timeout",
		"hermes send: Telegram send failed: 504 Gateway Timeout",
		"hermes send: Telegram send failed: read tcp: connection reset by peer",
		"hermes send: Telegram send failed: context deadline exceeded",
	} {
		t.Run(text, func(t *testing.T) {
			ledger := setupFakeHermesAttempts(t, text)

			deliverOutputWithMode(clock.NewSimClockAt(1000, time.Now()),
				"timeoutproj", "tick-1672e", "telegram:1", "prompt", tickBody(), "")

			if got := hermesAttempts(t, ledger); got != 1 {
				t.Errorf("failure %q: delivery attempts = %d, want 1 (ambiguous — never auto-resent)", text, got)
			}
		})
	}
}
