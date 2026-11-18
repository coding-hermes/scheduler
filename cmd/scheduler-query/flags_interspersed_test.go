package main

// SCHED-GAP-16932: the documented flag shape must parse in EVERY order.
// help.go documents `--daemon-url URL` for `--all`, so both
//
//	scheduler-query --all <op> --daemon-url URL
//	scheduler-query --daemon-url URL --all <op>
//
// are the same invocation. Before the fix the first died with "flag needs
// an argument: -daemon-url" and the second bound "--all" as the daemon
// URL, silently falling back to single-peer mode. These tables assert the
// PARSED surface (mode, peer, op, daemon URL, positionals) — the parse,
// not the transport — so a regression cannot hide behind a network error.

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

// TestSCHEDGAP16932_InterspersedFlags pins the accepted orders and the
// parsed result for each. Both orders of the documented aggregate shape
// must resolve to the same surface.
func TestSCHEDGAP16932_InterspersedFlags(t *testing.T) {
	const daemon = "http://127.0.0.1:19090"
	tests := []struct {
		name        string
		argv        []string
		wantAll     bool
		wantPeer    string
		wantOp      string
		wantDaemon  string
		wantPos     []string
		wantWant    string
		wantBudget  int
		wantJSON    bool
		wantArgKeys []string
	}{
		{
			// (a) the shape the row reports as broken.
			name:       "--all <op> --daemon-url URL",
			argv:       []string{"--all", "peer.status", "--daemon-url", daemon},
			wantAll:    true,
			wantOp:     "peer.status",
			wantDaemon: daemon,
			wantPos:    []string{"peer.status"},
		},
		{
			// (b) the reverse order, which used to swallow --all.
			name:       "--daemon-url URL --all <op>",
			argv:       []string{"--daemon-url", daemon, "--all", "peer.status"},
			wantAll:    true,
			wantOp:     "peer.status",
			wantDaemon: daemon,
			wantPos:    []string{"peer.status"},
		},
		{
			name:       "--daemon-url=URL form",
			argv:       []string{"--all", "peer.status", "--daemon-url=" + daemon},
			wantAll:    true,
			wantOp:     "peer.status",
			wantDaemon: daemon,
			wantPos:    []string{"peer.status"},
		},
		{
			// (c) --daemon-url does NOT flip the mode: without --all this
			// is still single-peer.
			name:       "--daemon-url URL <peer> <op> stays single-peer",
			argv:       []string{"--daemon-url", daemon, "peer-a", "queue.get"},
			wantAll:    false,
			wantPeer:   "peer-a",
			wantOp:     "queue.get",
			wantDaemon: daemon,
			wantPos:    []string{"peer-a", "queue.get"},
		},
		{
			// (e) a real op name between flags is never consumed as a value.
			name:       "positionals preserved between flags",
			argv:       []string{"--json", "peer-a", "--daemon-url", daemon, "events.list", "--want", "partial", "--budget-ms", "1500"},
			wantPeer:   "peer-a",
			wantOp:     "events.list",
			wantDaemon: daemon,
			wantWant:   "partial",
			wantBudget: 1500,
			wantJSON:   true,
			wantPos:    []string{"peer-a", "events.list"},
		},
		{
			name:        "aggregate with --arg after the positionals",
			argv:        []string{"--all", "ticks.list", "--arg", "limit=5", "--daemon-url", daemon},
			wantAll:     true,
			wantOp:      "ticks.list",
			wantDaemon:  daemon,
			wantPos:     []string{"ticks.list"},
			wantArgKeys: []string{"limit"},
		},
		{
			name:       "flag before the op, op before the flag",
			argv:       []string{"--daemon-url", daemon, "--all", "projects.list"},
			wantAll:    true,
			wantOp:     "projects.list",
			wantDaemon: daemon,
			wantPos:    []string{"projects.list"},
		},
		{
			name:    "-- terminates flag parsing after the aggregate flag",
			argv:    []string{"--all", "--", "peer.status"},
			wantAll: true,
			wantOp:  "peer.status",
			wantPos: []string{"peer.status"},
		},
		{
			name:       "legacy-all keeps its shape",
			argv:       []string{"--daemon-url", daemon, "--legacy-all", "peer.status"},
			wantOp:     "peer.status",
			wantDaemon: daemon,
			wantPos:    []string{"peer.status"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.argv)
			if err != nil {
				t.Fatalf("parseArgs(%q) = error %v, want success", tc.argv, err)
			}
			if got.allPeers != tc.wantAll {
				t.Errorf("allPeers = %t, want %t", got.allPeers, tc.wantAll)
			}
			if got.peer != tc.wantPeer {
				t.Errorf("peer = %q, want %q", got.peer, tc.wantPeer)
			}
			if got.op != tc.wantOp {
				t.Errorf("op = %q, want %q", got.op, tc.wantOp)
			}
			if got.daemonURL != tc.wantDaemon {
				t.Errorf("daemonURL = %q, want %q", got.daemonURL, tc.wantDaemon)
			}
			if strings.Join(got.positionals, ",") != strings.Join(tc.wantPos, ",") {
				t.Errorf("positionals = %q, want %q", got.positionals, tc.wantPos)
			}
			if got.want != tc.wantWant {
				t.Errorf("want = %q, want %q", got.want, tc.wantWant)
			}
			if got.budgetMS != tc.wantBudget {
				t.Errorf("budgetMS = %d, want %d", got.budgetMS, tc.wantBudget)
			}
			if got.jsonOut != tc.wantJSON {
				t.Errorf("jsonOut = %t, want %t", got.jsonOut, tc.wantJSON)
			}
			for _, k := range tc.wantArgKeys {
				if _, ok := got.args[k]; !ok {
					t.Errorf("args missing %q: %+v", k, got.args)
				}
			}
		})
	}
}

// TestSCHEDGAP16932_FlagErrors pins the refusal ladder: an unknown flag and
// a value-taking flag with no value are still hard errors with a real
// message — including the shape that used to swallow --all.
func TestSCHEDGAP16932_FlagErrors(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantSub string
	}{
		{name: "unknown flag", argv: []string{"--nope", "peer-a", "peer.status"}, wantSub: "flag provided but not defined"},
		{name: "missing value at end", argv: []string{"--all", "peer.status", "--daemon-url"}, wantSub: "flag needs an argument: -daemon-url"},
		{name: "missing value before another flag", argv: []string{"--daemon-url", "--all", "peer.status"}, wantSub: "flag needs an argument: -daemon-url"},
		{name: "missing value for --want", argv: []string{"peer-a", "queue.get", "--want"}, wantSub: "flag needs an argument: -want"},
		{name: "--all without an op", argv: []string{"--all", "--daemon-url", "http://127.0.0.1:19090"}, wantSub: "--all takes exactly one positional"},
		{name: "--all with a stray positional", argv: []string{"--all", "peer.status", "extra"}, wantSub: "--all takes exactly one positional"},
		{name: "single-peer shape with one positional", argv: []string{"peer-a"}, wantSub: "expected: scheduler-query <peer> <op>"},
		{name: "bad --want", argv: []string{"--all", "peer.status", "--want", "bogus"}, wantSub: `--want must be`},
		{name: "empty op", argv: []string{"--all", " "}, wantSub: "op is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.argv)
			if err == nil {
				t.Fatalf("parseArgs(%q) succeeded, want an error", tc.argv)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestSCHEDGAP16932_HelpIsNotAnError pins that -h/--help surfaces as
// flag.ErrHelp (main renders the help block to stdout, exit 0) rather than
// a usage mistake.
func TestSCHEDGAP16932_HelpIsNotAnError(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--all", "peer.status", "-h"}} {
		if _, err := parseArgs(argv); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("parseArgs(%q) error = %v, want flag.ErrHelp", argv, err)
		}
	}
}

// TestSCHEDGAP16932_ValueFlagsDerived drives the derivation itself: every
// registered non-bool flag must be in the value-taking set, so a future
// flag cannot reintroduce the drift that broke --daemon-url.
func TestSCHEDGAP16932_ValueFlagsDerived(t *testing.T) {
	var p cliParse
	fs := newQueryFlagSet(&p)
	valueFlags := valueFlagNames(fs)
	for _, name := range []string{"arg", "want", "budget-ms", "corr-id", "daemon-url", "url", "db"} {
		if !valueFlags[name] {
			t.Errorf("value-taking flag %q missing from the derived set: %v", name, valueFlags)
		}
	}
	for _, name := range []string{"all", "legacy-all", "json"} {
		if valueFlags[name] {
			t.Errorf("bool flag %q must not consume a following token", name)
		}
	}
	// And the derivation covers exactly the registered surface: no stale
	// name left behind that the set no longer defines.
	defined := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { defined[f.Name] = true })
	for name := range valueFlags {
		if !defined[name] {
			t.Errorf("derived value flag %q is not registered on the set", name)
		}
	}
}

// TestSCHEDGAP16932_NegativeNumericValue pins that a signed number is a
// value, not a following flag (--budget-ms -1 still parses).
func TestSCHEDGAP16932_NegativeNumericValue(t *testing.T) {
	got, err := parseArgs([]string{"--all", "peer.status", "--budget-ms", "-1"})
	if err != nil {
		t.Fatalf("parseArgs(--budget-ms -1) = %v, want success", err)
	}
	if got.budgetMS != -1 {
		t.Errorf("budgetMS = %d, want -1", got.budgetMS)
	}
}

// TestSCHEDGAP16932_MissingValueHasNoHelpDump pins the second half of the
// acceptance: a flag missing its value must fail "with a message naming that
// flag and NO help dump". The message is the whole answer; 60 lines of usage
// bury it. Every OTHER usage mistake still carries the usage block, which is
// where a caller learns the surface.
func TestSCHEDGAP16932_MissingValueHasNoHelpDump(t *testing.T) {
	for _, argv := range [][]string{
		{"--all", "peer.status", "--daemon-url"},
		{"--daemon-url", "--all", "peer.status"},
		{"peer-a", "queue.get", "--want"},
	} {
		_, err := parseArgs(argv)
		if err == nil {
			t.Fatalf("parseArgs(%q) succeeded, want a missing-value error", argv)
		}
		if !errors.Is(err, errMissingValue) {
			t.Fatalf("parseArgs(%q) error = %v, want it to wrap errMissingValue", argv, err)
		}
		var buf strings.Builder
		writeUsageError(err, &buf)
		out := buf.String()
		if !strings.Contains(out, "flag needs an argument: -") {
			t.Errorf("parseArgs(%q) rendered %q, want it to name the flag", argv, out)
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("parseArgs(%q) rendered a help dump for a missing value:\n%s", argv, out)
		}
	}

	// The contrast arm: a non-missing-value usage mistake keeps the block.
	_, otherErr := parseArgs([]string{"queue.get"}) // no peer, no --all
	if otherErr == nil {
		t.Fatal("parseArgs(queue.get) succeeded, want a usage error")
	}
	if errors.Is(otherErr, errMissingValue) {
		t.Fatalf("a missing peer must not be classified as a missing flag value: %v", otherErr)
	}
	var buf strings.Builder
	writeUsageError(otherErr, &buf)
	if !strings.Contains(buf.String(), "Usage:") {
		t.Errorf("a non-missing-value usage mistake must keep the usage block, got %q", buf.String())
	}
}
