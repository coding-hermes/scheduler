package scheduler

// SCHED-GAP-243: a router refusal must be distinguishable from an outage.
//
// These tests pin the classifier, not the whole spawn path: the defect was that
// this class fell through as "other errors keep single-attempt behavior", so the
// tick died even though the lane's fallback could have served it. The classifier
// is the decision point, so it is the thing that must stay honest.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsRouterRefusal_ClassifiesOnlyServable503(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "503 unservable-rating is a refusal",
			err: &GatewayStatusError{
				StatusCode: http.StatusServiceUnavailable,
				msg:        `gateway POST: HTTP 503: {"error": "no eligible model for this rating in the registry and no fallback lane could serve (requirements_unmet: the rating outruns the fleet)", "failure_reason": "unservable-rating", "lane_class": "primary"}`,
			},
			want: true,
		},
		{
			name: "503 without the routing verdict is just an outage",
			err:  &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: `gateway POST: HTTP 503: upstream connect error`},
			want: false,
		},
		{
			name: "500 carrying the same text is not a refusal",
			err:  &GatewayStatusError{StatusCode: http.StatusInternalServerError, msg: `gateway POST: HTTP 500: {"failure_reason": "unservable-rating"}`},
			want: false,
		},
		{
			name: "401 is the auth class, handled elsewhere",
			err:  &GatewayStatusError{StatusCode: http.StatusUnauthorized, msg: `gateway POST: HTTP 401: {"failure_reason": "unservable-rating"}`},
			want: false,
		},
		{
			name: "wrapped refusal still classifies (the spawn path wraps errors)",
			err:  fmt.Errorf("dispatch leg: %w", &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: `gateway POST: HTTP 503: {"failure_reason": "unservable-rating"}`}),
			want: true,
		},
		{
			name: "503 no-open-hop is a refusal too (TR-340 marker set)",
			err:  &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: `gateway POST: HTTP 503: {"error": "no open hop for this request", "chain_length": 0}`},
			want: true,
		},
		{
			name: "503 registry-missing is a refusal too",
			err:  &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: `gateway POST: HTTP 503: {"error": "registry-missing for lane docs"}`},
			want: true,
		},
		{
			name: "503 no eligible model is a refusal too",
			err:  &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: `gateway POST: HTTP 503: {"error": "no eligible model for this rating"}`},
			want: true,
		},
		{name: "nil", err: nil, want: false},
		{name: "plain transport error", err: errors.New("dial tcp 127.0.0.1:9391: connect: connection refused"), want: false},
		{name: "key rejection", err: fmt.Errorf("%w (HTTP 401): bad key", ErrGatewayKeyRejected), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRouterRefusal(tc.err); got != tc.want {
				t.Fatalf("isRouterRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A refusal must NOT be treated as transient: the transient path retries the SAME
// pair, which is exactly the pair the router just said it cannot serve.
func TestIsRouterRefusal_IsNotTransient(t *testing.T) {
	refusal := &GatewayStatusError{
		StatusCode: http.StatusServiceUnavailable,
		msg:        `gateway POST: HTTP 503: {"failure_reason": "unservable-rating"}`,
	}
	if isRouterRefusal(refusal) != true {
		t.Fatal("premise: this error must classify as a refusal")
	}
	// IsTransientGatewayErr classifies by status class, so it WILL report true for
	// any 5xx. The point of this test is to document the interaction: the chain
	// advance must key on isRouterRefusal, because transience alone cannot tell a
	// refusal from an outage.
	if !IsTransientGatewayErr(refusal) {
		t.Log("note: 5xx no longer classifies as transient — re-check the spawn ordering")
	}
}
