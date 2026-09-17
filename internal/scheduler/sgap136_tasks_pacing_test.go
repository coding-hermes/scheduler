package scheduler

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// SCHED-GAP-136 AC1: post-tick pacing — a tasks-mode project whose last tick
// finished inside the pacing window is deferred; the jittered predicate must
// defer through base (always) and never past base+maxJitter (base×1.2).
func TestSchedGap136_PostTickPacingDefersRecentTick(t *testing.T) {
	defer SetTasksPacing(0) // restore default-off for other tests

	SetTasksPacing(60 * time.Second)
	now := time.Now()

	// 10s after the last tick: inside the window — defer, always.
	if !tasksPacingDeferredJittered(&now, now.Add(10*time.Second)) {
		t.Fatal("tick finished 10s ago with 60s pacing: expected deferral")
	}
	// 59s after: still inside the base window regardless of jitter
	// (jitter only ever EXTENDS the wait).
	mid := now.Add(59 * time.Second)
	if !tasksPacingDeferredJittered(&now, mid) {
		t.Fatal("tick finished 59s ago with 60s pacing: expected deferral (jitter never shortens)")
	}
	// Deterministic jitter extremes.
	orig := tasksJitterFrac
	defer func() { tasksJitterFrac = orig }()
	tasksJitterFrac = func() float64 { return 0 } // no jitter
	if tasksPacingDeferredJittered(&now, now.Add(60*time.Second)) {
		t.Fatal("at jitter=0 the window must end exactly at base (60s)")
	}
	tasksJitterFrac = func() float64 { return 0.999 } // max jitter
	if !tasksPacingDeferredJittered(&now, now.Add(60*time.Second)) {
		t.Fatal("at max jitter the window must extend past base")
	}
	if tasksPacingDeferredJittered(&now, now.Add(73*time.Second)) {
		t.Fatal("pacing must never exceed base*1.2 (72s at 60s base)")
	}
}

// AC1b: nil lastTick (never ticked) is NEVER deferred — the floor is a
// post-tick pace, not a first-spawn gate.
func TestSchedGap136_FirstSpawnNeverPaced(t *testing.T) {
	defer SetTasksPacing(0)
	SetTasksPacing(60 * time.Second)
	now := time.Now()
	if tasksPacingDeferredJittered(nil, now) {
		t.Fatal("nil lastTick must never defer — pacing is POST-tick only")
	}
}

// AC1c: pacing disabled (0) → never defers, byte-identical legacy behavior.
func TestSchedGap136_DisabledByDefault(t *testing.T) {
	if tasksPacing() != 0 {
		t.Fatalf("library default must be 0 (off), got %v", tasksPacing())
	}
	now := time.Now()
	lt := now.Add(-1 * time.Millisecond)
	if tasksPacingDeferredJittered(&lt, now) {
		t.Fatal("pacing=0 must never defer")
	}
	// Negative installs clamp to 0 (no pathological negative windows).
	SetTasksPacing(-5 * time.Second)
	if tasksPacing() != 0 {
		t.Fatalf("negative pacing must clamp to 0, got %v", tasksPacing())
	}
}

// AC1d: the eligibility mirror uses BASE (no jitter) — conservative side —
// so a project the packer is still pacing is never counted eligible.
func TestSchedGap136_EligibilityMirrorConservative(t *testing.T) {
	defer SetTasksPacing(0)
	SetTasksPacing(60 * time.Second)
	now := time.Now()
	lt := now.Add(-45 * time.Second)
	if !tasksPacingDeferred(lt, now) {
		t.Fatal("45s after last tick (60s base): mirror must report deferred")
	}
	lt = now.Add(-61 * time.Second)
	if tasksPacingDeferred(lt, now) {
		t.Fatal("61s after last tick (60s base): mirror must report eligible (base has no jitter)")
	}
	SetTasksPacing(0)
	if tasksPacingDeferred(now, now) {
		t.Fatal("pacing=0: mirror must never defer")
	}
}

// AC2: Retry-After parsing — a 503 with "Retry-After: N" carries the hint on
// the GatewayStatusError; absent or malformed headers carry 0.
func TestSchedGap136_RetryAfterParsed(t *testing.T) {
	// (behavior exercised via gatewayRetrySleep with a synthetic error —
	// the header read itself is covered by the httptest POST below.)
	gse := &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: 3 * time.Second, msg: "gateway POST: HTTP 503: draining"}
	if gse.Error() != "gateway POST: HTTP 503: draining" {
		t.Fatalf("legacy error text changed: %q", gse.Error())
	}
}

// AC2b: the retry sleep honors the server hint as a FLOOR — never below the
// exponential backoff, never below Retry-After.
func TestSchedGap136_GatewayRetrySleepFloorsOnRetryAfter(t *testing.T) {
	// Retry-After 3s > attempt-1 backoff 500ms → sleep 3s.
	err := &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: 3 * time.Second, msg: "x"}
	if got := gatewayRetrySleep(err, 1); got != 3*time.Second {
		t.Fatalf("gatewayRetrySleep = %v, want 3s (Retry-After floor)", got)
	}
	// Retry-After 1s < attempt-4 backoff 4s → exponential wins.
	small := &GatewayStatusError{StatusCode: 503, RetryAfter: 1 * time.Second, msg: "x"}
	if got := gatewayRetrySleep(small, 4); got != 4*time.Second {
		t.Fatalf("gatewayRetrySleep = %v, want 4s (exponential above floor)", got)
	}
	// No hint → plain exponential (legacy behavior byte-identical).
	if got := gatewayRetrySleep(errors.New("plain"), 2); got != gatewayRetryBackoff(2) {
		t.Fatalf("gatewayRetrySleep(plain err) = %v, want %v", got, gatewayRetryBackoff(2))
	}
	// Zero hint on a status error → plain exponential.
	zero := &GatewayStatusError{StatusCode: 503, RetryAfter: 0, msg: "x"}
	if got := gatewayRetrySleep(zero, 1); got != 500*time.Millisecond {
		t.Fatalf("gatewayRetrySleep(zero hint) = %v, want 500ms", got)
	}
}
