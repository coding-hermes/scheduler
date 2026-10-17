package mcp

// SCHED-GAP-1741 — the `peer` argument on the MCP federation query tools.
//
// The MCP transport answers about THIS scheduler (docs/federation-query-spec.md
// §3 MCP row: "one tool per catalogue op" served by the scheduler the agent is
// talking to). `peer` is therefore an ASSERTION, not a selector, and the
// catalogue splits by what the op is ABOUT: peer.status is about one identity,
// so naming it is mandatory (a missing peer used to be accepted, ignored by the
// op and answered from the LOCAL read — a wrong answer the caller could not
// see); the other ops are reads about this scheduler, where the peer member of
// the answer names the default. These cells pin the pure decision
// (`fedPeerArgsError`) over every input shape; the behavioural arms (refusal
// over the real JSON-RPC surface, schema wording) live in
// federation_query_test.go.

import (
	"strings"
	"testing"
)

func TestFedPeerArgsError(t *testing.T) {
	const answering = "sched-self-01"
	cases := []struct {
		name        string
		op          string
		args        map[string]interface{}
		wantErr     bool
		wantMessage []string
	}{
		// peer.status: the assertion is REQUIRED.
		{
			name:        "peer.status without args",
			op:          "peer.status",
			args:        nil,
			wantErr:     true,
			wantMessage: []string{"peer is required in args", answering},
		},
		{
			name:        "peer.status without peer",
			op:          "peer.status",
			args:        map[string]interface{}{},
			wantErr:     true,
			wantMessage: []string{"peer is required in args", answering},
		},
		{name: "peer.status naming this scheduler", op: "peer.status", args: map[string]interface{}{"peer": answering}},
		{name: "peer.status padded identity", op: "peer.status", args: map[string]interface{}{"peer": " " + answering + "\t"}},
		{
			name:        "peer.status naming another scheduler",
			op:          "peer.status",
			args:        map[string]interface{}{"peer": "sched-other-02"},
			wantErr:     true,
			wantMessage: []string{"sched-other-02", "not this scheduler", answering, "scheduler-query"},
		},
		{
			name:        "peer.status with an empty peer",
			op:          "peer.status",
			args:        map[string]interface{}{"peer": ""},
			wantErr:     true,
			wantMessage: []string{"args.peer", "non-empty string"},
		},
		{
			name:        "peer.status with a non-string peer",
			op:          "peer.status",
			args:        map[string]interface{}{"peer": 42},
			wantErr:     true,
			wantMessage: []string{"args.peer", "must be a string"},
		},
		{
			name:        "peer.status with a null peer",
			op:          "peer.status",
			args:        map[string]interface{}{"peer": nil},
			wantErr:     true,
			wantMessage: []string{"args.peer", "must be a string"},
		},
		// Every other op defaults to this scheduler.
		{name: "queue.get without args", op: "queue.get", args: nil},
		{name: "queue.get with unrelated args", op: "queue.get", args: map[string]interface{}{"limit": 1}},
		{name: "projects.list naming this scheduler", op: "projects.list", args: map[string]interface{}{"filter": "alpha", "peer": answering}},
		{
			name:        "queue.get naming another scheduler",
			op:          "queue.get",
			args:        map[string]interface{}{"peer": "sched-other-02"},
			wantErr:     true,
			wantMessage: []string{"sched-other-02", "not this scheduler"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fedPeerArgsError(tc.op, tc.args, answering)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a named refusal, got none (the caller's peer selection would be silently ignored)")
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lost %q", err.Error(), want)
				}
			}
		})
	}
}

// TestFedPeerArgsErrorNoAnsweringIdentity pins the fail-closed arm: a process
// with no installed scheduler identity cannot verify an explicit peer claim,
// so it refuses instead of guessing (the daemon always installs one at boot —
// cmd/schedulerd/main.go). The peer.status requirement is identity-independent.
func TestFedPeerArgsErrorNoAnsweringIdentity(t *testing.T) {
	if err := fedPeerArgsError("peer.status", map[string]interface{}{"peer": "sched-other-02"}, ""); err == nil {
		t.Fatal("an explicit peer against an uninstalled answering identity must be refused, not answered locally")
	}
	if err := fedPeerArgsError("queue.get", map[string]interface{}{}, ""); err != nil {
		t.Fatalf("the absent-peer default must not depend on the answering identity: %v", err)
	}
	if err := fedPeerArgsError("peer.status", map[string]interface{}{}, ""); err == nil {
		t.Fatal("peer.status must require a peer even before the identity is installed")
	}
}

// TestFedAnsweringPeerIDFallsBackToHostname pins the resolution the refusal
// message names: the installed daemon identity first, the host-derived default
// (exactly what cmd/schedulerd installs) otherwise.
func TestFedAnsweringPeerIDFallsBackToHostname(t *testing.T) {
	got := fedAnsweringPeerID()
	if strings.TrimSpace(got) == "" {
		t.Fatal("answering peer identity resolved empty — the refusal message would name nothing")
	}
}
