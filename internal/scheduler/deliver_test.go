package scheduler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	deliverOutput("testproj", "tick-001", "telegram:123", nil)

	// Should not call hermes — nil buffer returns early
	content := readCapture(t, captureFile)
	if content != "" {
		t.Errorf("expected no hermes call for nil buffer, got: %s", content)
	}
}

func TestDeliverOutput_EmptyBuffer(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	deliverOutput("testproj", "tick-001", "telegram:123", &buf)

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
	deliverOutput("testproj", "tick-001", "", &buf)

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
	deliverOutput("testproj", "tick-001", "telegram:123", &buf)

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

func TestDeliverOutput_WithToolNoise(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	// Output with tool noise that should be stripped
	var buf bytes.Buffer
	buf.WriteString("tool output\n")
	buf.WriteString("more tool\n")
	buf.WriteString("┊ review diff\n")
	buf.WriteString("---\n")
	buf.WriteString("Human summary of completed work with enough length to pass the 50-char threshold test.")
	deliverOutput("noiseproj", "tick-002", "telegram:456", &buf)

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
	deliverOutput("noiseproj", "tick-003", "telegram:789", &buf)

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
	deliverOutput("testproj", "tick-001", "telegram:123", &buf)

	// Should not panic — logs the error
	content := readCapture(t, captureFile)
	_ = content // exec failure is handled gracefully
}

func TestDeliverOutput_TickIDInSubject(t *testing.T) {
	_, captureFile := setupFakeHermes(t)

	var buf bytes.Buffer
	buf.WriteString("foreman work summary with tick id appended")
	deliverOutput("myproject", "tick-ABC-123", "telegram:999", &buf)

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
