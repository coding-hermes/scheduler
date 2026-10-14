package scheduler

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SCHED-GAP-1679 acceptance: a delivery target the gateway cannot resolve must
// never reach sendWithRetry, and the skip must be logged at most once per lane
// per UTC day.
//
// The defect (measured 2026-09-30 on the live daemon): 10 enabled lanes carried
// deliver='local' in the scheduler DB. 'local' IS a Hermes Platform enum member
// (gateway/config.py — Platform.LOCAL) but no deployment configures it as a
// messaging platform, so every report delivery ran sendWithRetry: 4 attempts
// with 2s/5s/15s backoff, each answered "Platform 'local' is not configured" in
// ~0ms. Every such delivery burned ~1 min of tick slot and recorded a failed
// delivery — 40 failures/12h. The '' target already skipped with its own log
// line; the unconfigured-platform target did not.
//
// The target grammar is "<platform>:<chat_id>[:<thread_id>]" — the shape this
// repo already states (internal/database/models.go, docs/api.md) and the one
// all 510 configured rows of the live DB carry
// ("telegram:-1003310984808:118900"). Zero rows ever used a literal
// "platform:chat:" prefix, so the guard is a SHAPE gate plus the reserved
// 'local' platform name — never a prefix literal that would skip every
// telegram lane in the fleet.

// resetDeliverWarnState clears the once-per-lane-per-day gate so one test can
// never make another's lane look "already warned today".
func resetDeliverWarnState(t *testing.T) {
	t.Helper()
	deliverWarnMu.Lock()
	deliverWarnDay = map[string]string{}
	deliverWarnMu.Unlock()
}

// deliverWarnCount counts the unconfigured-target skip lines in a captured log.
func deliverWarnCount(logOut string) int {
	return strings.Count(logOut, "is not a configured platform; skipping")
}

// tickReport is a non-empty tick report buffer (delivery is skipped before the
// send only for a target problem, never for an empty body).
func tickReport() *bytes.Buffer {
	var buf bytes.Buffer
	buf.WriteString("tick report body\n\n_tick-1679 · command_\n")
	return &buf
}

// TestSchedGap1679UnconfiguredTargetSkipsSend proves the measured value is
// refused: no `hermes send` process is ever started (the fake binary on PATH
// never records an invocation), no retry attempt is logged, and the skip line
// names the lane, the tick and the offending target.
func TestSchedGap1679UnconfiguredTargetSkipsSend(t *testing.T) {
	resetDeliverWarnState(t)
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	clk := clock.NewFixed(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	deliverOutput(clk, "axiom-sync", "tick-1679-a", localPlatformName, "command", tickReport())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("hermes send was invoked for an unconfigured target (captured %q); want no invocation", got)
	}
	if n := len(attemptLogLines(logOut.String())); n != 0 {
		t.Fatalf("retry attempts were logged for an unconfigured target: %d\nlog:\n%s", n, logOut.String())
	}
	want := "DELIVER: axiom-sync tick=tick-1679-a — target 'local' is not a configured platform; skipping (logged once/day)"
	if !strings.Contains(logOut.String(), want) {
		t.Fatalf("skip line missing.\nwant:\n%s\nlog:\n%s", want, logOut.String())
	}
}

// TestSchedGap1679WarnFiresOncePerUTCDay pins the in-memory gate: two skips for
// the same lane in one UTC day produce ONE line, and the next UTC day re-arms
// it. Neither attempt may reach the send path.
func TestSchedGap1679WarnFiresOncePerUTCDay(t *testing.T) {
	resetDeliverWarnState(t)
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	// Both instants are the same UTC calendar day; the second is a different
	// (later) day, one minute past midnight.
	day1 := clock.NewFixed(time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC))
	day2 := clock.NewFixed(time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC))

	deliverOutput(day1, "blog-sync", "tick-1679-b1", localPlatformName, "command", tickReport())
	deliverOutput(day1, "blog-sync", "tick-1679-b2", localPlatformName, "command", tickReport())
	if n := deliverWarnCount(logOut.String()); n != 1 {
		t.Fatalf("warn fired %d time(s) within one UTC day; want 1\nlog:\n%s", n, logOut.String())
	}

	// A DIFFERENT lane in the same day gets its own warning (the gate is
	// per-lane, not global).
	deliverOutput(day1, "eduos-sync", "tick-1679-b3", localPlatformName, "command", tickReport())
	if n := deliverWarnCount(logOut.String()); n != 2 {
		t.Fatalf("second lane got no warning (count=%d, want 2)\nlog:\n%s", n, logOut.String())
	}

	// The next UTC day re-arms the warning for the first lane.
	deliverOutput(day2, "blog-sync", "tick-1679-b4", localPlatformName, "command", tickReport())
	if n := deliverWarnCount(logOut.String()); n != 3 {
		t.Fatalf("warn did not re-arm on the next UTC day (count=%d, want 3)\nlog:\n%s", n, logOut.String())
	}

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("hermes send was invoked for an unconfigured target (captured %q); want no invocation", got)
	}
}

// TestSchedGap1679PlatformQualifiedTargetStillSends pins the other half of the
// contract: every platform-qualified shape still reaches sendWithRetry. The
// first case is the live fleet's real value; the rest guard against the guard
// being tightened into a hardcoded platform-name allowlist, which would drop a
// legitimate delivery from a runtime-registered plugin platform.
func TestSchedGap1679PlatformQualifiedTargetStillSends(t *testing.T) {
	resetDeliverWarnState(t)
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	clk := clock.NewFixed(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	for _, target := range []string{
		"telegram:-1003310984808:118900", // the live fleet's real shape
		"telegram:123",                   // the shape the existing tests use
		"platform:chat:123",              // generic <platform>:<chat>:<thread>
		"signal:+15551234567",
	} {
		deliverOutput(clk, "reports-sync", "tick-1679-c", target, "command", tickReport())

		got := readCapture(t, capture)
		if !strings.Contains(got, "--to "+target) {
			t.Fatalf("target %q never reached sendWithRetry (captured %q)", target, got)
		}
		if want := "DELIVER: reports-sync tick=tick-1679-c → " + target; !strings.Contains(logOut.String(), want) {
			t.Fatalf("success line missing for %q.\nwant:\n%s\nlog:\n%s", target, want, logOut.String())
		}
	}
	if n := deliverWarnCount(logOut.String()); n != 0 {
		t.Fatalf("a platform-qualified target was refused as unconfigured (count=%d)\nlog:\n%s", n, logOut.String())
	}
}

// TestSchedGap1679AlertPathGuard proves the alert send shares the guard: the
// alert path calls the same sendWithRetry, so an unconfigured target there must
// also skip, and the once-per-day gate is shared (keyed by lane, not by path).
func TestSchedGap1679AlertPathGuard(t *testing.T) {
	resetDeliverWarnState(t)
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	clk := clock.NewFixed(time.Date(2026, 9, 30, 9, 30, 0, 0, time.UTC))
	deliverAlertWith(clk, localPlatformName, "my-project-qa", "tick-1679-alert-1", "timeout after 2h")
	deliverAlertWith(clk, localPlatformName, "my-project-qa", "tick-1679-alert-2", "timeout after 2h")

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("alert send was invoked for an unconfigured target (captured %q); want no invocation", got)
	}
	want := "ALERT: my-project-qa tick=tick-1679-alert-1 — target 'local' is not a configured platform; skipping (logged once/day)"
	if !strings.Contains(logOut.String(), want) {
		t.Fatalf("alert skip line missing.\nwant:\n%s\nlog:\n%s", want, logOut.String())
	}
	if n := deliverWarnCount(logOut.String()); n != 1 {
		t.Fatalf("alert warn fired %d time(s) within one UTC day; want 1\nlog:\n%s", n, logOut.String())
	}

	// A valid alert target is untouched by the guard.
	deliverAlertWith(clk, "telegram:-1003310984808:5", "my-project-qa", "tick-1679-alert-3", "timeout")
	if got := readCapture(t, capture); !strings.Contains(got, "--to telegram:-1003310984808:5") {
		t.Fatalf("valid alert target never reached sendWithRetry (captured %q)", got)
	}
}

// TestSchedGap1679EmptyTargetPathUnchanged pins that step 3's DB fix (all junk
// targets rewritten to ”) lands on the pre-existing honest path: the empty
// target keeps its own line and never touches the unconfigured-platform gate.
func TestSchedGap1679EmptyTargetPathUnchanged(t *testing.T) {
	resetDeliverWarnState(t)
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	clk := clock.NewFixed(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	deliverOutput(clk, "gitreins-sync", "tick-1679-d", "", "command", tickReport())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("hermes send was invoked for an empty target (captured %q)", got)
	}
	want := "DELIVER: gitreins-sync tick=tick-1679-d — no delivery target configured"
	if !strings.Contains(logOut.String(), want) {
		t.Fatalf("empty-target line changed.\nwant:\n%s\nlog:\n%s", want, logOut.String())
	}
	if n := deliverWarnCount(logOut.String()); n != 0 {
		t.Fatalf("the empty target hit the unconfigured-platform gate (count=%d)", n)
	}
}

// TestSchedGap1679IsDeliverableTarget pins the predicate itself, including the
// boundary shapes the call sites rely on.
func TestSchedGap1679IsDeliverableTarget(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"telegram:-1003310984808:118900", true}, // live fleet shape
		{"telegram:123", true},                   // pre-existing test shape
		{"platform:chat:123", true},              // generic platform-qualified shape
		{"telegram:", false},                     // platform with no chat id
		{":123", false},                          // chat id with no platform
		{"local", false},                         // the measured defect (no qualifier)
		{"local:123", false},                     // reserved non-messaging platform
		{"", false},                              // handled earlier by the empty check
		{"   ", false},
	}
	for _, tc := range cases {
		if got := isDeliverableTarget(tc.target); got != tc.want {
			t.Errorf("isDeliverableTarget(%q) = %v; want %v", tc.target, got, tc.want)
		}
	}
}
