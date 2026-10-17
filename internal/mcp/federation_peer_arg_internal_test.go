package mcp

// SCHED-GAP-1741 — the `peer` argument on the MCP federation query tools.
//
// The MCP transport answers about THIS scheduler (docs/federation-query-spec.md
// §3 MCP row: "one tool per catalogue op" served by the scheduler the agent is
// talking to). `peer` is therefore an ASSERTION, not a selector: an explicit
// peer that names a different scheduler used to be copied into the §2.1
// envelope's args, ignored by every §2.3 op, and answered from the LOCAL read —
// a wrong answer the caller could not see. These cells pin the pure decision
// (`fedPeerArgError`) over every input shape; the behavioural arms (default
// named in the answer, foreign peer refused over the real JSON-RPC surface)
// live in federation_query_test.go.

import (
	"strings"
	"testing"
)

func TestFedPeerArgError(t *testing.T) {
	const answering = "sched-self-01"
	cases := []struct {
		name        string
		args        map[string]interface{}
		wantErr     bool
		wantMessage []string
	}{
		{name: "nil args (op takes none)", args: nil},
		{name: "empty args", args: map[string]interface{}{}},
		{name: "op args without peer", args: map[string]interface{}{"filter": "alpha"}},
		{name: "peer names this scheduler", args: map[string]interface{}{"peer": answering}},
		{name: "peer padded with whitespace", args: map[string]interface{}{"peer": "  " + answering + "\t"}},
		{
			name:        "peer names another scheduler",
			args:        map[string]interface{}{"peer": "sched-other-02"},
			wantErr:     true,
			wantMessage: []string{"sched-other-02", "not this scheduler", answering, "scheduler-query"},
		},
		{
			name:        "peer is empty",
			args:        map[string]interface{}{"peer": ""},
			wantErr:     true,
			wantMessage: []string{"args.peer", "non-empty string"},
		},
		{
			name:        "peer is whitespace",
			args:        map[string]interface{}{"peer": "   "},
			wantErr:     true,
			wantMessage: []string{"args.peer", "non-empty string"},
		},
		{
			name:        "peer is not a string",
			args:        map[string]interface{}{"peer": 42},
			wantErr:     true,
			wantMessage: []string{"args.peer", "must be a string"},
		},
		{
			name:        "peer is null",
			args:        map[string]interface{}{"peer": nil},
			wantErr:     true,
			wantMessage: []string{"args.peer", "must be a string"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fedPeerArgError(tc.args, answering)
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

// TestFedPeerArgErrorNoAnsweringIdentity pins the fail-closed arm: a process
// with no installed scheduler identity cannot verify an explicit peer claim,
// so it refuses instead of guessing (the daemon always installs one at boot —
// cmd/schedulerd/main.go).
func TestFedPeerArgErrorNoAnsweringIdentity(t *testing.T) {
	if err := fedPeerArgError(map[string]interface{}{"peer": "sched-other-02"}, ""); err == nil {
		t.Fatal("an explicit peer against an uninstalled answering identity must be refused, not answered locally")
	}
	if err := fedPeerArgError(map[string]interface{}{}, ""); err != nil {
		t.Fatalf("the absent-peer default must not depend on the answering identity: %v", err)
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
