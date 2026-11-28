package api

// SCHED-GAP-17084: the overwrite summary is the sanitized description an
// operator reads in the daemon log, so its vocabulary (and its refusal to quote
// a destination) is pinned directly.

import (
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

func TestDestinationPolicyOverwriteSummarizesExceptionChanges(t *testing.T) {
	prior := &blocks.DestinationPolicy{
		Destination: "telegram:-123:10",
		Approved:    true,
		Exceptions: map[string]blocks.DestinationException{
			"kept":      {Destination: "telegram:-123:11", Approved: true},
			"changed":   {Destination: "telegram:-123:12", Approved: true},
			"dropped":   {Destination: "telegram:-123:13", Approved: true},
			"revoked":   {Destination: "telegram:-123:14", Approved: true},
			"unchanged": {Destination: "telegram:-123:15", Approved: true},
		},
	}
	next := &blocks.DestinationPolicy{
		Destination: "telegram:-123:10",
		Approved:    true,
		Exceptions: map[string]blocks.DestinationException{
			"kept":      {Destination: "telegram:-123:11", Approved: true},
			"changed":   {Destination: "telegram:-123:99", Approved: true},
			"revoked":   {Destination: "telegram:-123:14"},
			"added":     {Destination: "telegram:-123:16", Approved: true},
			"unchanged": {Destination: "telegram:-123:15", Approved: true},
		},
	}
	summary := destinationPolicyOverwrite(prior, next)
	for _, want := range []string{"1 exception(s) added", "1 exception(s) removed", "2 exception(s) redefined"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
	if strings.Contains(summary, "123") {
		t.Fatalf("summary leaked a destination: %q", summary)
	}
	if got := destinationPolicyOverwrite(prior, prior); got != "" {
		t.Fatalf("identical policies produced %q, want empty", got)
	}
	if got := destinationPolicyOverwrite(nil, next); got != "" {
		t.Fatalf("first-time policy produced %q, want empty", got)
	}
	if got := destinationPolicyOverwrite(prior, nil); got != "" {
		t.Fatalf("absent patch produced %q, want empty", got)
	}
}

func TestDestinationPolicyOverwriteNamesAuthorityChanges(t *testing.T) {
	approved := &blocks.DestinationPolicy{Destination: "telegram:-123:10", Approved: true}
	cases := []struct {
		name  string
		prior *blocks.DestinationPolicy
		next  *blocks.DestinationPolicy
		want  string
	}{
		{"destination replaced", approved, &blocks.DestinationPolicy{Destination: "telegram:-123:20", Approved: true}, "approved destination replaced"},
		{"approval withdrawn", approved, &blocks.DestinationPolicy{Destination: "telegram:-123:10"}, "approval withdrawn"},
		{"approval granted", &blocks.DestinationPolicy{Destination: "telegram:-123:10"}, approved, "approval granted"},
		{"destination cleared", approved, &blocks.DestinationPolicy{Approved: true}, "destination cleared"},
		{"destination set", &blocks.DestinationPolicy{Approved: true}, approved, "destination set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := destinationPolicyOverwrite(tc.prior, tc.next)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("summary = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, "123") {
				t.Fatalf("summary leaked a destination: %q", got)
			}
		})
	}
}
