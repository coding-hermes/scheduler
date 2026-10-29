package scheduler

// SCHED-GAP-16940 — the drop wrapper must state the observed status, not a
// guessed cause.
//
// Measured 2026-10-29 (live scheduler.db, 3h): 13 of 24 failed ticks carried
//
//	gateway unreachable and exec fallback disabled: gateway POST: HTTP 502:
//	{"error": "timed out", "_router": {"complexity_score": ...}}
//
// i.e. the operator was told the gateway was UNREACHABLE by a message whose own
// text proves the gateway answered — twice over: it returned a status AND its
// body shows the model router behind it. 6 more were the gateway's own 500.
//
// The label states what the evidence shows:
//
//	*GatewayStatusError  -> "gateway HTTP <code>"   (answered; code carries)
//	no HTTP answer       -> "gateway unreachable"   (a measurement, not a guess)
//
// and "exec fallback disabled" survives in every variant, because that is the
// real OUTCOME of the drop (--no-exec-fallback), not a cause.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestSCHEDGAP16940_DropLabelNamesTheObservedStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "502 from the router behind the gateway",
			err:  &GatewayStatusError{StatusCode: 502, msg: `gateway POST: HTTP 502: {"error": "timed out"}`},
			want: "gateway HTTP 502",
		},
		{
			name: "500 from the gateway itself",
			err:  &GatewayStatusError{StatusCode: 500, msg: `gateway POST: HTTP 500: {"error": "Internal Server Error"}`},
			want: "gateway HTTP 500",
		},
		{
			name: "refused dial — no HTTP answer at all",
			err: fmt.Errorf("gateway POST: %w", &url.Error{
				Op:  "Post",
				URL: "http://127.0.0.1:9391/v1/responses",
				Err: syscall.ECONNREFUSED,
			}),
			want: "gateway unreachable",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayDropLabel(tc.err)
			if got != tc.want {
				t.Errorf("gatewayDropLabel(%v) = %q, want %q", tc.err, got, tc.want)
			}
			// The composed drop message keeps the real outcome in every
			// variant — runbooks and the consecutive-drop alert read it.
			composed := fmt.Errorf("%s and exec fallback disabled: %w", got, tc.err).Error()
			if !strings.Contains(composed, "exec fallback disabled") {
				t.Errorf("composed drop message %q lost the real outcome 'exec fallback disabled'", composed)
			}
			// Criterion 1: the transport error rides verbatim, so the row can
			// be re-read years later without the original err value.
			if !strings.Contains(composed, tc.err.Error()) {
				t.Errorf("composed drop message %q does not carry the cause verbatim (%q)", composed, tc.err.Error())
			}
			// And the lie is gone: a status-bearing error must never be
			// labelled unreachable.
			var gse *GatewayStatusError
			if errors.As(tc.err, &gse) && strings.Contains(composed, "unreachable") {
				t.Errorf("composed drop message %q calls an ANSWERED gateway unreachable", composed)
			}
		})
	}
}

// TestSCHEDGAP16940_Answered5xxIsHarnessSide pins the ONE-CLASS-ONE-VERDICT
// rule for the answered-5xx shape.
//
// The spawn drop path classifies the RAW gateway error text
// ("gateway POST: HTTP 502: …") to decide whether consecutive_failures may
// move, while every read-back surface sees the WRAPPER text (which carries
// "exec fallback disabled"). Before this fix the raw text matched no harness
// marker, so the same 502 refusal counted against the lane at spawn time and
// was excluded when read from the DB — one class, two verdicts. Measured
// 2026-10-29: 13 of 24 failed ticks in 3h were this shape.
func TestSCHEDGAP16940_Answered5xxIsHarnessSide(t *testing.T) {
	cases := []string{
		`gateway POST: HTTP 502: {"error": "timed out", "_router": {"complexity_score": 0.71}}`,
		`gateway POST: HTTP 500: {"error": "Internal Server Error"}`,
		`gateway unreachable and exec fallback disabled: gateway POST: HTTP 502: {"error": "timed out"}`,
	}
	for _, text := range cases {
		text := text
		t.Run(text[:24], func(t *testing.T) {
			if !harnessFailure(text) {
				t.Errorf("harnessFailure(%q) = false — an ANSWERED 5xx is harness-side, never the lane's", text)
			}
			if got := failureReasonClass(text); got != FailureReasonGateway5xx {
				t.Errorf("failureReasonClass(%q) = %q, want %q", text, got, FailureReasonGateway5xx)
			}
			// The invariant both surfaces depend on: a non-empty class means
			// the harness owns the failure, so no surface may charge the lane.
			if failureReasonClass(text) != "" && !harnessFailure(text) {
				t.Errorf("class/verdict disagree for %q: class non-empty but harnessFailure=false", text)
			}
		})
	}
	// Negative control: a project-side failure must stay chargeable.
	if harnessFailure("worker wave wedged: no worker terminal after 3h") {
		t.Error("a project-side failure must never classify as harness-side")
	}
}
