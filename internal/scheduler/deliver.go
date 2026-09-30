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
	"sync"
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

// tickPermalink builds the absolute dashboard URL for one tick's report — the
// SINGLE place link-mode URLs are composed.
//
// The link targets the per-tick route GET /ticks/{id} (SCHED-GAP-1593), which
// renders that tick's full report. It deliberately carries NO fragment: the
// pre-1593 shape targeted the lane page with a #tick- anchor no lane page
// ever rendered, so the fragment was inert (SCHED-GAP-1673).
func tickPermalink(baseURL, tickID string) string {
	base := strings.TrimRight(baseURL, "/")
	return base + "/ticks/" + url.PathEscape(tickID)
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

// ---------------------------------------------------------------------------
// Unconfigured delivery target guard (SCHED-GAP-1679)
// ---------------------------------------------------------------------------

// localPlatformName is Hermes' reserved non-messaging platform
// (gateway Platform.LOCAL). No deployment configures it as a delivery target,
// so a target naming it can never resolve.
const localPlatformName = "local"

// deliverWarn gates the once-per-lane-per-day skip warning. IN MEMORY ONLY —
// deliberately not persisted: the warning exists to keep an operator informed
// without filling the tick log, and re-arming it on a daemon restart is the
// cheap direction of the trade (the alternative measured 40 failed deliveries
// per 12h). Keyed by project name → last UTC date warned.
var (
	deliverWarnMu  sync.Mutex
	deliverWarnDay = map[string]string{}
)

// isDeliverableTarget reports whether deliver names a platform-qualified
// delivery target the gateway can resolve.
//
// The grammar is "<platform>:<chat_id>[:<thread_id>]" — stated by this
// codebase itself (internal/database/models.go, docs/api.md) and carried by
// every one of the live fleet's configured rows
// ("telegram:-1003310984808:118900").
//
// A target with no platform qualifier cannot resolve: the gateway answers
// "Platform '<x>' is not configured" in ~0ms, so sendWithRetry burned all 4
// attempts and 2s/5s/15s of backoff on a certain failure — ~1 min of tick slot
// and one recorded failed delivery per report, every report (SCHED-GAP-1679:
// 40 failures/12h from 10 lanes carrying the bare value "local", which IS a
// Hermes Platform enum member but is never a configured messaging platform).
//
// Deliberately NOT stricter than the shape: the platform prefix is not
// validated against a hardcoded name list, because plugin platforms register
// dynamically at runtime and a stale list would silently drop a legitimate
// delivery — the same failure class this guard exists to remove.
func isDeliverableTarget(deliver string) bool {
	platform, chatID, ok := strings.Cut(deliver, ":")
	if !ok || platform == "" || chatID == "" {
		return false
	}
	return platform != localPlatformName
}

// warnUnconfiguredTargetOnce logs the skip line for an unconfigured delivery
// target at most once per project per UTC day, and reports whether this call
// emitted it. Subsequent skips for the same lane in the same UTC day are
// silent — the tick no longer spends a retry sequence, so one line a day is
// enough to keep the misconfiguration visible.
//
// The day is read from clk (the daemon's single time choke point, SCHED-GAP-169)
// so the gate is deterministic under a fixed/sim clock.
func warnUnconfiguredTargetOnce(prefix string, clk clock.Clock, project, tickID, deliver string) bool {
	day := clk.Now().UTC().Format("2006-01-02")

	deliverWarnMu.Lock()
	prev, seen := deliverWarnDay[project]
	alreadyWarnedToday := seen && prev == day
	if !alreadyWarnedToday {
		deliverWarnDay[project] = day
	}
	deliverWarnMu.Unlock()

	if alreadyWarnedToday {
		return false
	}
	log.Printf("%s: %s tick=%s — target '%s' is not a configured platform; skipping (logged once/day)",
		prefix, project, tickID, deliver)
	return true
}

// sendFailClass classifies a failed `hermes send` run by whether a RESEND is
// safe (SCHED-GAP-1672).
//
// Retrying a failure whose outcome is unknown is what posted one tick report
// to the same Telegram thread four times (2026-09-29): the platform accepted
// the message, the client timed out waiting for the response, and the old
// retry policy — which treated every "Timed out" as transient — resent it.
// Sending is not idempotent: only a failure that PROVES nothing was posted may
// be retried.
type sendFailClass int

const (
	// sendFailNone is the zero value: the send did not fail.
	sendFailNone sendFailClass = iota
	// sendFailRejected — an explicit rejection: the platform or transport
	// REFUSED the request, so nothing was posted and a resend cannot
	// duplicate anything (flood control / rate limit, connection refused
	// before the request left, HTTP 429/5xx with no timeout signal).
	sendFailRejected
	// sendFailAmbiguous — a timeout or a mid-flight connection abort. The
	// request may have reached the platform and been delivered before our
	// client gave up, so an automatic resend CAN duplicate the post. Never
	// auto-resent.
	sendFailAmbiguous
	// sendFailFatal — a config/usage error (bad --to, unknown platform,
	// invalid argument). Retrying cannot help.
	sendFailFatal
)

// sendRejectionMarkers are lowercased substrings that prove the request was
// refused before it was accepted — safe to retry.
//
// The flood/rate-limit vocabulary mirrors the gateway's own send-error
// classifier (gateway/platforms/base.py classify_send_error), so the two
// layers agree on what a Telegram rejection looks like.
var sendRejectionMarkers = []string{
	"flood",             // "Flood control exceeded. Retry in 18.00 seconds"
	"too many requests", // the same rejection, other spelling
	"retry after",
	"rate limit",
	"429",
	"502",
	"503",
	"connection refused",
	"econnrefused",
	"temporary", // "temporary failure in name resolution" — never dialled
	"network",   // "network is unreachable" — never dialled
	"gateway",   // "Bad Gateway" spelled out instead of the status code
	"backend",
}

// sendAmbiguityMarkers are lowercased substrings that mean the client stopped
// waiting without learning the outcome. The message may already be posted.
var sendAmbiguityMarkers = []string{
	"timed out",
	"timeout",
	"deadline exceeded",
	"connection reset",
	"econnreset",
	"broken pipe",
	"unexpected eof",
}

// classifySendFailure maps a `hermes send` failure to its resend class.
//
// AMBIGUITY WINS: the timeout markers are tested FIRST, so a failure carrying
// any timeout signal is never auto-resent even when an HTTP status marker is
// also present. That is deliberate — "504 Gateway Timeout" is 5xx but it means
// the upstream never confirmed the request, which is exactly the unknowable
// outcome that duplicated the post. A bare rejection ("Flood control
// exceeded", HTTP 429/503, connection refused) carries no timeout signal and
// still retries.
func classifySendFailure(err error, output string) sendFailClass {
	msg := strings.ToLower(err.Error() + " " + output)
	for _, m := range sendAmbiguityMarkers {
		if strings.Contains(msg, m) {
			return sendFailAmbiguous
		}
	}
	for _, m := range sendRejectionMarkers {
		if strings.Contains(msg, m) {
			return sendFailRejected
		}
	}
	return sendFailFatal
}

// sendResult is the terminal outcome of one sendWithRetry sequence.
type sendResult struct {
	sent     bool          // true when an attempt succeeded
	class    sendFailClass // failure class (sendFailNone when sent)
	attempts int           // delivery attempts actually made
}

// sendWithRetry runs `hermes send` with the given args. A REJECTION (see
// sendFailRejected) is retried with exponential backoff — the platform refused
// the request, so another attempt can still land the report. An AMBIGUOUS
// failure (a bare timeout) is terminal on the FIRST attempt: the message may
// already be posted, and re-sending is what duplicates it (SCHED-GAP-1672).
// Config/usage errors fail immediately. Returns the trimmed output of the last
// attempt and the terminal result.
func sendWithRetry(clk clock.Clock, args ...string) ([]byte, sendResult) {
	const maxAttempts = 4
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

	var lastOut []byte
	res := sendResult{}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cmd := exec.Command("hermes", append([]string{"send"}, args...)...)
		out, err := cmd.CombinedOutput()
		lastOut = out
		res.attempts = attempt + 1
		if err == nil {
			res.class = sendFailNone
			res.sent = true
			return out, res
		}
		class := classifySendFailure(err, string(out))
		res.class = class
		// "— retrying" may only appear when another attempt will actually
		// run: it is derived from the retry decision, never from the class.
		willRetry := class == sendFailRejected && attempt < maxAttempts-1
		suffix := ""
		if willRetry {
			suffix = " — retrying"
		}
		log.Printf("DELIVER: attempt %d/%d failed: %v (%s)%s",
			attempt+1, maxAttempts, err, strings.TrimSpace(string(out)), suffix)
		if !willRetry {
			return out, res
		}
		clk.Sleep(backoff[attempt])
	}
	return lastOut, res
}

// logSendFailure emits the terminal line for a failed send. An ambiguous
// (possibly-already-delivered) failure gets its own distinct, greppable line
// and states that it is deliberately NOT resent; anything else reports how
// many attempts were made.
func logSendFailure(prefix, project, tickID string, out []byte, res sendResult) {
	if res.class == sendFailAmbiguous {
		log.Printf("%s: %s tick=%s — AMBIGUOUS send failure (timeout): message may have been delivered, NOT resending (duplicate-suppression)",
			prefix, project, tickID)
		return
	}
	log.Printf("%s: %s tick=%s — hermes send failed after %d attempt(s) (%s)",
		prefix, project, tickID, res.attempts, bytes.TrimSpace(out))
}

// deliverOutput sends tick output to the configured delivery target via Hermes' gateway.
// Strips terminal tool output (diffs, review panels, worker prompts) and delivers
// the foreman's actual summary. No length cap — full detail preserved.
// trigger is "command" (custom command/script spawn) or "prompt" (LLM prompt
// spawn) — carried in the subject (top line) and the footer (bottom line) so
// the thread shows how the run was launched (Bane 2026-08-27).
func deliverOutput(clk clock.Clock, project, tickID, deliver, trigger string, output *bytes.Buffer) {
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

	// SCHED-GAP-1679: never spend a retry sequence on a target the gateway
	// cannot resolve — see isDeliverableTarget. Checked BEFORE any mode
	// resolution or temp-file work so a misconfigured lane costs nothing.
	if !isDeliverableTarget(deliver) {
		warnUnconfiguredTargetOnce("DELIVER", clk, project, tickID, deliver)
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
	link := tickPermalink(publicBaseURL, tickID)
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

	out, res := sendWithRetry(clk, "--to", deliver, "--subject", subject, "--file", f.Name())
	if !res.sent {
		logSendFailure("DELIVER", project, tickID, out, res)
		return
	}
	log.Printf("DELIVER: %s tick=%s → %s", project, tickID, deliver)
}

// sendShort sends the short file/link message. The subject is already the
// message's first line, so no --subject flag is passed (the header stays the
// visible header; the whole short text lives in the body).
func sendShort(clk clock.Clock, project, tickID, deliver, msg string) {
	out, res := sendWithRetry(clk, "--to", deliver, msg)
	if !res.sent {
		logSendFailure("DELIVER", project, tickID, out, res)
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
	// SCHED-GAP-1679: the alert path shares sendWithRetry, so it shares the
	// unconfigured-target guard (and the once-per-day gate, keyed by lane).
	if !isDeliverableTarget(deliver) {
		warnUnconfiguredTargetOnce("ALERT", clk, project, tickID, deliver)
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
	out, res := sendWithRetry(clk, "--to", deliver, "--subject", fmt.Sprintf("⚠️ %s", project), "--file", f.Name())
	if !res.sent {
		logSendFailure("ALERT", project, tickID, out, res)
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
