package scheduler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// =============================================================================
// SCHED-GAP-1607: tick-report delivery modes (full | file | link)
//
// The seam is the composed `hermes send` argv. The shared fake in
// deliver_test.go space-joins "$@", which is ambiguous for message TEXT that
// itself contains spaces — so these tests use a base64-per-line fake that
// round-trips every argument exactly. The fake also snapshots every --file
// body and MEDIA: attachment into a keep dir AT SEND TIME: deliverOutputWith
// removes its temp files on return, so "the attachment existed when hermes
// send ran" is only provable from inside the send.
// =============================================================================

// setupFakeHermesB64 installs a fake `hermes` that captures every argv
// element base64-encoded, one per line, and copies send-time file payloads
// (--file values, MEDIA: targets) into a keep dir.
func setupFakeHermesB64(t *testing.T) (capture, keepDir string) {
	t.Helper()
	dir := t.TempDir()
	capture = filepath.Join(dir, "capture.txt")
	keepDir = filepath.Join(dir, "keep")
	if err := os.MkdirAll(keepDir, 0o755); err != nil {
		t.Fatalf("mkdir keep dir: %v", err)
	}

	script := `#!/bin/bash
: > "$HERMES_CAPTURE_FILE"
prev=""
for a in "$@"; do
  printf '%s\n' "$(printf '%s' "$a" | base64 -w0)" >> "$HERMES_CAPTURE_FILE"
  if [ "$prev" = "--file" ] && [ -f "$a" ]; then
    cp "$a" "$HERMES_KEEP_DIR/body" || true
  fi
  case "$a" in
    *'MEDIA:'*)
      p="${a##*MEDIA:}"
      if [ -f "$p" ]; then
        cp "$p" "$HERMES_KEEP_DIR/attachment" || true
      fi
      ;;
  esac
  prev="$a"
done
if [ -n "$HERMES_EXIT_CODE" ]; then
  exit "$HERMES_EXIT_CODE"
fi
exit 0
`
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}

	t.Setenv("HERMES_CAPTURE_FILE", capture)
	t.Setenv("HERMES_KEEP_DIR", keepDir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	return capture, keepDir
}

// captureArgs decodes the captured argv exactly (args[0] is "send").
func captureArgs(t *testing.T, capture string) []string {
	t.Helper()
	raw, err := os.ReadFile(capture)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatalf("no argv captured")
	}
	var args = make([]string, 0, 16)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("decode argv element %q: %v", line, err)
		}
		args = append(args, string(b))
	}
	return args
}

// findArg returns the value that follows flag in args ("" when absent).
func findArg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// resetPublicBaseURL arms the link base and restores "" after the test.
func resetPublicBaseURL(t *testing.T, u string) {
	t.Helper()
	SetPublicBaseURL(u)
	t.Cleanup(func() { SetPublicBaseURL("") })
}

// reportBuffer builds a foreman-shaped body long enough that trimToolNoise
// keeps it verbatim (the over-trim guard returns raw only under 50 chars).
func reportBuffer() *bytes.Buffer {
	return bytes.NewBufferString(strings.Repeat("Summary line of the tick report.\n", 8))
}

// readKept reads a send-time snapshot written by the fake ("" when absent —
// meaning the payload did not exist on disk when hermes send ran).
func readKept(t *testing.T, keepDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(keepDir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestSCHEDGAP1607_DeliverModes_Table(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		publicURL   string
		wantSubject bool // full shape: --subject + --file, no MEDIA
		wantMedia   bool // file shape: message text ends with MEDIA:<abs .md>
		wantLink    bool // link shape: message text carries exactly one URL
	}{
		{name: "full", mode: "full", wantSubject: true},
		{name: "empty mode is full", mode: "", wantSubject: true},
		{name: "unknown mode falls back to full", mode: "carrier-pigeon", wantSubject: true},
		{name: "file", mode: "file", wantMedia: true},
		{name: "link", mode: "link", publicURL: "https://sched.example.com", wantLink: true},
		{name: "link without base URL degrades to full", mode: "link", publicURL: "", wantSubject: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture, keep := setupFakeHermesB64(t)
			resetPublicBaseURL(t, tc.publicURL)

			clk := clock.NewManualSimClock(time.Unix(0, 0))
			deliverOutputWithMode(clk, "modetest", "tick-1607", "telegram:123", "prompt", reportBuffer(), tc.mode)

			args := captureArgs(t, capture)
			if args[0] != "send" {
				t.Fatalf("argv[0] = %q, want send", args[0])
			}
			if findArg(args, "--to") != "telegram:123" {
				t.Errorf("--to = %q, want telegram:123", findArg(args, "--to"))
			}

			switch {
			case tc.wantSubject:
				// full shape (also the fail-safe shape): header via --subject,
				// body+footer via --file, no MEDIA marker anywhere.
				if got := findArg(args, "--subject"); got != "🤖 modetest [tick-1607] · prompt" {
					t.Errorf("--subject = %q, want the historical header", got)
				}
				bodyPath := findArg(args, "--file")
				if bodyPath == "" {
					t.Fatalf("no --file arg in argv: %q", args)
				}
				body := readKept(t, keep, "body")
				if body == "" {
					t.Fatalf("body file %q did not exist when hermes send ran", bodyPath)
				}
				s := body
				if !strings.HasSuffix(s, "_tick-1607 · prompt_") {
					t.Errorf("body does not end with the footer: %q", s[max(0, len(s)-60):])
				}
				if !strings.Contains(s, "Summary line of the tick report.") {
					t.Errorf("full body lost the report text")
				}
				if strings.Contains(s, "MEDIA:") {
					t.Errorf("full body must not carry a MEDIA: marker")
				}
			case tc.wantMedia:
				// file shape: short message (header + footer + MEDIA:), no
				// --subject, no --file flag.
				if findArg(args, "--subject") != "" {
					t.Errorf("file mode must not pass --subject (header is the message's first line)")
				}
				if findArg(args, "--file") != "" {
					t.Errorf("file mode must not pass --file (body rides MEDIA:)")
				}
				msg := args[len(args)-1]
				idx := strings.Index(msg, "MEDIA:")
				if idx < 0 {
					t.Fatalf("message has no MEDIA: marker: %q", msg)
				}
				fpath := msg[idx+len("MEDIA:"):]
				if !strings.HasPrefix(fpath, "/") {
					t.Errorf("MEDIA: path is not absolute: %q", fpath)
				}
				if !strings.HasSuffix(fpath, "tick-1607.md") {
					t.Errorf("attachment name = %q, want *tick-1607.md", fpath)
				}
				// The attachment existed on disk when hermes send ran…
				att := readKept(t, keep, "attachment")
				if att == "" {
					t.Fatalf("attachment %q did not exist when hermes send ran", fpath)
				}
				if !strings.Contains(att, "Summary line of the tick report.") {
					t.Errorf("attachment does not carry the full report")
				}
				if strings.Contains(att, "_tick-1607 · prompt_") {
					t.Errorf("attachment must hold the report body, not the footer")
				}
				// …and is removed after the send returned.
				if _, err := os.Stat(fpath); !os.IsNotExist(err) {
					t.Errorf("attachment %q still exists after the send returned", fpath)
				}
				// Header + footer live in the SHORT message.
				if !strings.HasPrefix(msg, "🤖 modetest [tick-1607] · prompt\n\n_tick-1607 · prompt_") {
					t.Errorf("short message missing header/footer: %q", msg)
				}
			case tc.wantLink:
				// link shape: short message + exactly one absolute URL.
				if findArg(args, "--subject") != "" {
					t.Errorf("link mode must not pass --subject")
				}
				msg := args[len(args)-1]
				wantURL := tickPermalink("https://sched.example.com", "modetest", "tick-1607")
				if !strings.HasPrefix(wantURL, "https://sched.example.com/ticks?") {
					t.Fatalf("permalink not absolute against base: %q", wantURL)
				}
				if !strings.Contains(msg, wantURL) {
					t.Errorf("message missing permalink %s: %q", wantURL, msg)
				}
				if strings.Count(msg, "https://") != 1 {
					t.Errorf("link message must carry exactly one URL: %q", msg)
				}
				if !strings.HasPrefix(msg, "🤖 modetest [tick-1607] · prompt\n\n_tick-1607 · prompt_") {
					t.Errorf("short message missing header/footer: %q", msg)
				}
			}
		})
	}
}

// TestSCHEDGAP1607_FullModeArgsUnchanged pins the historical full-mode argv:
// send --to X --subject H --file F — exactly the pre-1607 argument set.
func TestSCHEDGAP1607_FullModeArgsUnchanged(t *testing.T) {
	capture, _ := setupFakeHermesB64(t)
	resetPublicBaseURL(t, "")

	deliverOutputWithMode(clock.NewManualSimClock(time.Unix(0, 0)),
		"modetest", "tick-1608", "telegram:9", "command", reportBuffer(), "")

	args := captureArgs(t, capture)
	want := []string{"send", "--to", "telegram:9", "--subject", "🤖 modetest [tick-1608] · command", "--file"}
	if len(args) != len(want)+1 { // +1: the --file value
		t.Fatalf("argv = %q, want shape %v <body-path> (full mode must stay byte-identical)", args, want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Fatalf("argv[%d] = %q, want %q", i, args[i], w)
		}
	}
	if !strings.HasPrefix(args[len(args)-1], "/tmp/chtick-tick-1608-") {
		t.Errorf("body path = %q, want a chtick temp file", args[len(args)-1])
	}
}

// TestSCHEDGAP1607_FileModeRemovalAfterSend proves the ordering contract in
// isolation: the .md attachment is gone once deliverOutputWithMode returned —
// i.e. the deferred os.Remove runs AFTER sendWithRetry.
func TestSCHEDGAP1607_FileModeRemovalAfterSend(t *testing.T) {
	capture, _ := setupFakeHermesB64(t)
	resetPublicBaseURL(t, "")

	deliverOutputWithMode(clock.NewManualSimClock(time.Unix(0, 0)),
		"modetest", "tick-1609", "telegram:7", "prompt", reportBuffer(), "file")

	args := captureArgs(t, capture)
	msg := args[len(args)-1]
	idx := strings.Index(msg, "MEDIA:")
	if idx < 0 {
		t.Fatalf("no MEDIA: marker: %q", msg)
	}
	fpath := msg[idx+len("MEDIA:"):]
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("attachment %q still exists after the send returned", fpath)
	}
}

// TestSCHEDGAP1607_FileModeFallbackOnWriterFailure: a mode-specific failure
// must degrade to full, never drop the report. The report-writer seam is
// stubbed to fail, so file mode cannot produce an attachment; the capture
// must then show the historical full-mode argv with the complete report.
func TestSCHEDGAP1607_FileModeFallbackOnWriterFailure(t *testing.T) {
	capture, keep := setupFakeHermesB64(t)
	resetPublicBaseURL(t, "")

	orig := writeTickReportFileFn
	writeTickReportFileFn = func(tickID, body string) (string, error) {
		return "", errors.New("injected report-write failure")
	}
	t.Cleanup(func() { writeTickReportFileFn = orig })

	deliverOutputWithMode(clock.NewManualSimClock(time.Unix(0, 0)),
		"modetest", "tick-1611", "telegram:5", "prompt", reportBuffer(), "file")

	args := captureArgs(t, capture)
	if findArg(args, "--subject") != "🤖 modetest [tick-1611] · prompt" || findArg(args, "--file") == "" {
		t.Fatalf("fallback must use the full shape (--subject + --file), got %q", args)
	}
	for _, a := range args {
		if strings.Contains(a, "MEDIA:") {
			t.Errorf("fallback argv must not carry MEDIA: (%q)", a)
		}
	}
	// The report text itself must still reach the thread (not dropped).
	if body := readKept(t, keep, "body"); !strings.Contains(body, "Summary line of the tick report.") {
		t.Errorf("fallback body lost the report text: %q", body)
	}
}

// TestSCHEDGAP1607_TickPermalink pins URL construction behind the one helper:
// absolute, points at /ticks, carries the lane-tick anchor, tolerates a
// trailing slash on the base.
func TestSCHEDGAP1607_TickPermalink(t *testing.T) {
	cases := []struct {
		base, lane, tick, wantPrefix string
	}{
		{"https://sched.example.com", "trouble", "2026-09-24-101530", "https://sched.example.com/ticks?"},
		{"https://sched.example.com/", "trouble", "2026-09-24-101530", "https://sched.example.com/ticks?"},
		{"http://127.0.0.1:9090", "", "2026-09-24-101530", "http://127.0.0.1:9090/ticks?"},
	}
	for _, c := range cases {
		got := tickPermalink(c.base, c.lane, c.tick)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("tickPermalink(%q,%q,%q) = %q, want prefix %q", c.base, c.lane, c.tick, got, c.wantPrefix)
		}
		if c.lane != "" && !strings.Contains(got, "tick="+c.lane+"-"+c.tick) {
			t.Errorf("permalink missing lane-tick anchor: %q", got)
		}
		if c.lane == "" && !strings.Contains(got, "tick="+c.tick) {
			t.Errorf("permalink missing tick anchor: %q", got)
		}
	}
}

// TestSCHEDGAP1607_ResolveDeliverMode: only exact file/link leave the full
// default; case and whitespace are not special-cased (typo ⇒ full).
func TestSCHEDGAP1607_ResolveDeliverMode(t *testing.T) {
	cases := map[string]string{
		"":          DeliverModeFull,
		"full":      DeliverModeFull,
		"file":      DeliverModeFile,
		"link":      DeliverModeLink,
		"FILE":      DeliverModeFull,
		" link ":    DeliverModeFull,
		"unknown":   DeliverModeFull,
		"carrier":   DeliverModeFull,
		"fileslink": DeliverModeFull,
	}
	for in, want := range cases {
		if got := resolveDeliverMode(in); got != want {
			t.Errorf("resolveDeliverMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSCHEDGAP1607_LinkModeNoURLWithoutBase is a second guard on the degrade
// rule at the delivery level: an empty base must never let an unopenable
// relative URL reach the message — the shape degrades to full instead.
func TestSCHEDGAP1607_LinkModeNoURLWithoutBase(t *testing.T) {
	capture, _ := setupFakeHermesB64(t)
	resetPublicBaseURL(t, "")

	deliverOutputWithMode(clock.NewManualSimClock(time.Unix(0, 0)),
		"modetest", "tick-1610", "telegram:3", "prompt", reportBuffer(), "link")

	args := captureArgs(t, capture)
	for _, a := range args {
		if strings.Contains(a, "http") || strings.Contains(a, "Report:") {
			t.Errorf("degraded link mode must not carry a URL: %q", a)
		}
	}
	if findArg(args, "--subject") == "" {
		t.Errorf("degraded link mode must use the full shape, got %q", args)
	}
}
