package scheduler

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// Delivery modes (SCHED-GAP-1607): how a tick report reaches the project's
// deliver target. Resolved by resolveDeliverMode — ” and unknown values
// resolve to DeliverModeFull, so pre-1607 rows and typo'd values behave
// exactly as before.
const (
	DeliverModeFull = "full" // header + body + footer in the message (historical shape)
	DeliverModeFile = "file" // short message + the full report as a .md document attachment
	DeliverModeLink = "link" // short message + one absolute dashboard URL for this tick
)

// publicBaseURL holds the dashboard origin used by DeliverModeLink to build
// tick permalinks. Threaded from --public-url / SCHEDULER_PUBLIC_URL at boot
// (cmd/schedulerd/main.go); package-level because deliverOutputWith is a free
// function in the historical (pre-seam) style — a "" value degrades link mode
// to full and logs why, never an unopenable URL.
var publicBaseURL string

// SetPublicBaseURL arms the tick-permalink base (SCHED-GAP-1607). Empty (the
// zero value) means "not configured" — link mode degrades to full.
func SetPublicBaseURL(u string) { publicBaseURL = strings.TrimRight(u, "/") }

// tickPermalink builds the absolute dashboard URL for one tick's report —
// the SINGLE place link-mode URLs are composed. The /ticks page is the only
// dashboard surface carrying tick rows, so the permalink anchors the page
// (scroll target) rather than promising a per-tick route that does not exist.
func tickPermalink(baseURL, lane, tickID string) string {
	page := strings.TrimRight(baseURL, "/") + "/ticks"
	anchor := tickID
	if lane != "" {
		anchor = lane + "-" + tickID
	}
	v := url.Values{}
	v.Set("page", "1")
	v.Set("tick", anchor)
	return page + "?" + v.Encode()
}

// resolveDeliverMode maps a stored deliver_mode value to a mode constant.
// ” (pre-1607 rows) and any unknown value resolve to full — delivery never
// drops or changes shape because of a mode problem.
func resolveDeliverMode(mode string) string {
	switch mode {
	case DeliverModeFile:
		return DeliverModeFile
	case DeliverModeLink:
		return DeliverModeLink
	default:
		return DeliverModeFull
	}
}

// sendWithRetry runs `hermes send` with the given args, retrying transient
// failures (timeout, connection reset, 429/5xx, "Timed out") with exponential
// backoff. Telegram backend latency spikes are transient — a retry with backoff
// turns a lost delivery into a delivered one. Non-retryable config/usage errors
// (bad --to, unknown platform) fail immediately. Returns the trimmed output of
// the last attempt and whether the send eventually succeeded.
func sendWithRetry(clk clock.Clock, args ...string) ([]byte, bool) {
	const maxAttempts = 4
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

	var lastOut []byte
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cmd := exec.Command("hermes", append([]string{"send"}, args...)...)
		out, err := cmd.CombinedOutput()
		lastOut = out
		if err == nil {
			return out, true
		}
		msg := string(out)
		retryable := isRetryableSendError(err, msg)
		log.Printf("DELIVER: attempt %d/%d failed: %v (%s)%s",
			attempt+1, maxAttempts, err, strings.TrimSpace(msg),
			map[bool]string{true: " — retrying", false: ""}[retryable])
		if !retryable || attempt == maxAttempts-1 {
			return out, false
		}
		clk.Sleep(backoff[attempt])
	}
	return lastOut, false
}

// isRetryableSendError reports whether a hermes-send failure is a transient
// backend/network error worth retrying, vs a permanent config/usage error.
func isRetryableSendError(err error, output string) bool {
	msg := strings.ToLower(err.Error() + " " + output)
	transient := []string{
		"timed out", "timeout", "connection reset", "connection refused",
		"temporary", "econnreset", "econnrefused", "i/o timeout",
		"429", "502", "503", "504", "gateway", "backend", "network",
	}
	for _, t := range transient {
		if strings.Contains(msg, t) {
			return true
		}
	}
	return false
}

// deliverOutput sends tick output to the configured delivery target via Hermes' gateway.
// Strips terminal tool output (diffs, review panels, worker prompts) and delivers
// the foreman's actual summary. No length cap — full detail preserved.
// trigger is "command" (custom command/script spawn) or "prompt" (LLM prompt
// spawn) — carried in the subject (top line) and the footer (bottom line) so
// the thread shows how the run was launched (Bane 2026-08-27).
func deliverOutput(project, tickID, deliver, trigger string, output *bytes.Buffer) {
	deliverOutputWithMode(clock.Real(), project, tickID, deliver, trigger, output, "")
}

// deliverOutputWith is deliverOutput on an explicit clock (SCHED-GAP-169): the
// retry backoff waits on clk, so a simulated run does not sleep in real time.
//
// SCHED-GAP-1607: callers that know the project's deliver_mode should call
// deliverOutputWithMode; this wrapper keeps the historical signature so the
// pre-1607 call shape stays byte-identical (mode "" → full).
func deliverOutputWith(clk clock.Clock, project, tickID, deliver, trigger string, output *bytes.Buffer) {
	deliverOutputWithMode(clk, project, tickID, deliver, trigger, output, "")
}

// deliverOutputWithMode delivers one tick report according to the project's
// deliver_mode (SCHED-GAP-1607):
//
//   - full  — header + report body + footer in one message (historical shape,
//     byte-identical to pre-1607).
//   - file  — the SHORT message (header + footer only) plus the complete
//     report as a .md document attachment (MEDIA:<abs path> in the message
//     text; `hermes send` turns non-image MEDIA: refs into documents with no
//     CLI change).
//   - link  — the SHORT message plus one absolute URL opening this tick's
//     report in the dashboard (built by tickPermalink from --public-url).
//
// Fail-safe contract: ” and unknown modes resolve to full, and any
// mode-specific failure (temp-file, write) falls back to full — a tick report
// is never silently dropped because of a delivery-mode problem. deliverAlert
// is out of scope and unchanged.
func deliverOutputWithMode(clk clock.Clock, project, tickID, deliver, trigger string, output *bytes.Buffer, mode string) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output", project, tickID)
		return
	}

	if deliver == "" {
		log.Printf("DELIVER: %s tick=%s — no delivery target configured", project, tickID)
		return
	}

	resolved := resolveDeliverMode(mode)
	if mode != "" && mode != resolved {
		log.Printf("DELIVER: %s tick=%s — unknown deliver_mode %q, falling back to full", project, tickID, mode)
	}
	if resolved == DeliverModeLink && publicBaseURL == "" {
		log.Printf("DELIVER: %s tick=%s — deliver_mode=link but no public base URL configured (--public-url), falling back to full", project, tickID)
		resolved = DeliverModeFull
	}

	subject := fmt.Sprintf("🤖 %s [%s] · %s", project, tickID, trigger)
	footer := fmt.Sprintf("_%s · %s_", tickID, trigger)

	if resolved == DeliverModeFull {
		body := trimToolNoise(strings.TrimSpace(output.String()))
		body = fmt.Sprintf("%s\n\n%s", body, footer)
		sendReportBody(clk, project, tickID, deliver, subject, body, fmt.Sprintf("chtick-%s-*.txt", tickID))
		return
	}

	// file/link: the thread gets the SHORT message (header + footer);
	// the complete report rides the attachment or the permalink.
	short := fmt.Sprintf("%s\n\n%s", subject, footer)

	if resolved == DeliverModeFile {
		body := trimToolNoise(strings.TrimSpace(output.String()))
		fpath, err := writeTickReportFileFn(tickID, body)
		if err != nil {
			// Fail-safe: a temp-file problem must never drop the report.
			log.Printf("DELIVER: %s tick=%s — file mode (%v), falling back to full", project, tickID, err)
			body = fmt.Sprintf("%s\n\n%s", body, footer)
			sendReportBody(clk, project, tickID, deliver, subject, body, fmt.Sprintf("chtick-%s-*.txt", tickID))
			return
		}
		// SCHED-GAP-1607 ordering contract: the attachment must still exist
		// on disk while hermes send runs, so the removal is deferred until
		// AFTER the send returns (sendWithRetry is fully synchronous).
		defer func() { _ = os.Remove(fpath) }()
		msg := short + "\n\nMEDIA:" + fpath
		sendShort(clk, project, tickID, deliver, msg)
		return
	}

	// DeliverModeLink.
	link := tickPermalink(publicBaseURL, project, tickID)
	msg := fmt.Sprintf("%s\n\nReport: %s", short, link)
	sendShort(clk, project, tickID, deliver, msg)
}

// writeTickReportFileFn is the injection seam for the report writer: the
// file-mode fallback test swaps it to prove the file→full degrade contract
// without OS-level error injection (long names break the full-mode temp file
// too, since both embed the tick id).
var writeTickReportFileFn = writeTickReportFile

// writeTickReportFile writes the (noise-trimmed) full report body to
// <lane>-<tickID>.md in a temp dir and returns the absolute path. The .md
// extension is what makes `hermes send` deliver it as a document.
func writeTickReportFile(tickID, body string) (string, error) {
	dir, err := os.MkdirTemp("", "chtick-md-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	name := fmt.Sprintf("%s.md", sanitizeReportName(tickID))
	fpath := filepath.Join(dir, name)
	if err := os.WriteFile(fpath, []byte(body), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write report: %w", err)
	}
	return fpath, nil
}

// sanitizeReportName strips characters that are unsafe or noisy in a
// filename (a tick id is host-name-safe already, but lane names reach this
// path via the file-mode name and must not smuggle separators).
func sanitizeReportName(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-", "..", ".")
	return r.Replace(s)
}

// sendReportBody is the historical send shape: one temp text file holding the
// composed body, sent with --subject. Kept byte-identical to pre-1607 for
// full mode.
func sendReportBody(clk clock.Clock, project, tickID, deliver, subject, body, tempPattern string) {
	f, err := os.CreateTemp("", tempPattern)
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — temp file: %v", project, tickID, err)
		return
	}
	defer f.Close()
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.WriteString(body); err != nil {
		log.Printf("DELIVER: %s tick=%s — write temp file: %v", project, tickID, err)
		return
	}
	f.Close()

	out, ok := sendWithRetry(clk, "--to", deliver, "--subject", subject, "--file", f.Name())
	if !ok {
		log.Printf("DELIVER: %s tick=%s — hermes send failed after retries (%s)", project, tickID, bytes.TrimSpace(out))
		return
	}
	log.Printf("DELIVER: %s tick=%s → %s", project, tickID, deliver)
}

// sendShort sends the short file/link message. The subject is already the
// message's first line, so no --subject flag is passed (the header stays the
// visible header; the whole short text lives in the body).
func sendShort(clk clock.Clock, project, tickID, deliver, msg string) {
	out, ok := sendWithRetry(clk, "--to", deliver, msg)
	if !ok {
		log.Printf("DELIVER: %s tick=%s — hermes send failed after retries (%s)", project, tickID, bytes.TrimSpace(out))
		return
	}
	log.Printf("DELIVER: %s tick=%s → %s", project, tickID, deliver)
}

// deliverAlert sends a short alert message for timeouts/errors so they
// are visible in the chat rather than silently swallowed.
func deliverAlert(deliver, project, tickID, reason string) {
	deliverAlertWith(clock.Real(), deliver, project, tickID, reason)
}

// deliverAlertWith is deliverAlert on an explicit clock (SCHED-GAP-169).
func deliverAlertWith(clk clock.Clock, deliver, project, tickID, reason string) {
	if deliver == "" {
		log.Printf("ALERT: %s tick=%s — %s (no delivery target configured)", project, tickID, reason)
		return
	}
	msg := fmt.Sprintf("⚠️ %s timed out — %s\nTick: %s", project, reason, tickID)
	f, err := os.CreateTemp("", fmt.Sprintf("chtick-alert-%s-*.txt", tickID))
	if err != nil {
		log.Printf("ALERT: temp file: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(msg)
	f.Close()
	out, ok := sendWithRetry(clk, "--to", deliver, "--subject", fmt.Sprintf("⚠️ %s", project), "--file", f.Name())
	if !ok {
		log.Printf("ALERT: send failed after retries (%s)", bytes.TrimSpace(out))
		return
	}
	log.Printf("ALERT: %s tick=%s → %s", project, tickID, deliver)
}

// trimToolNoise strips terminal/tool output from the foreman's raw stdout,
// keeping only the human-written summary. Handles multiple noise sources:
//
// 1. Final "---" separator — everything before it is tool noise
// 2. "┊" prefixed lines — terminal review panels (review diff, review file)
// 3. Git diff blocks (+/-/@@ lines)
// 4. Worker prompt dumps (long unbroken instruction blocks)
func trimToolNoise(raw string) string {
	// Strategy 1: Final "---" separator is the strongest signal
	if idx := strings.LastIndex(raw, "\n---\n"); idx >= 0 {
		s := strings.TrimSpace(raw[idx+5:])
		if len(s) > 50 {
			return s
		}
	}

	// Strategy 2: Strip tool output lines and compact
	lines := strings.Split(raw, "\n")
	var result []string
	inDiff := false
	inCodeBlock := false
	skippingWorker := false

	// Patterns that indicate non-summary lines
	deltaRe := regexp.MustCompile(`^@@\s+-\d+`)
	pipeRe := regexp.MustCompile(`^\s*┊`)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip tool review panels (┊ review diff, ┊ review file, etc.)
		if pipeRe.MatchString(line) {
			continue
		}

		// Skip git diff blocks
		if deltaRe.MatchString(trimmed) {
			inDiff = true
			continue
		}
		if inDiff {
			if strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") ||
				strings.HasPrefix(trimmed, "a/") || strings.HasPrefix(trimmed, "b/") ||
				strings.HasPrefix(trimmed, "index ") || strings.HasPrefix(trimmed, "---") {
				continue
			}
			inDiff = false
		}

		// Skip code block fences (not useful in delivery)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// Skip worker prompt instructions (long, dense, no blank lines)
		// These are recognizable: start with "You are a coding agent" or
		// "## TASK:" after the foreman's actual report
		if strings.HasPrefix(trimmed, "You are a coding agent") ||
			strings.HasPrefix(trimmed, "## TASK:") ||
			strings.HasPrefix(trimmed, "## INSERTION POINT") ||
			strings.HasPrefix(trimmed, "## PATTERN") ||
			strings.HasPrefix(trimmed, "## STORE API") ||
			strings.HasPrefix(trimmed, "## ALL") {
			skippingWorker = true
			result = append(result, "…") // indicate skipped content
			continue
		}
		if skippingWorker {
			// Stop skipping on blank lines or markdown headings (end of worker prompt)
			if trimmed == "" || strings.HasPrefix(trimmed, "**") {
				skippingWorker = false
				if trimmed != "" {
					result = append(result, line)
				}
			}
			continue
		}

		result = append(result, line)
	}

	// Compact blank lines: max 2 consecutive
	compacted := make([]string, 0, len(result))
	blankCount := 0
	for _, line := range result {
		if strings.TrimSpace(line) == "" {
			blankCount++
			if blankCount <= 2 {
				compacted = append(compacted, line)
			}
		} else {
			blankCount = 0
			compacted = append(compacted, line)
		}
	}

	cleaned := strings.TrimSpace(strings.Join(compacted, "\n"))

	// If the result is suspiciously short, return the raw (don't over-trim)
	if len(cleaned) < 50 && len(raw) > 200 {
		return raw
	}

	return cleaned
}
