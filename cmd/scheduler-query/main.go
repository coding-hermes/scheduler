// REMOTE-011 (docs/federation-query-spec.md §3, the "CLI" row):
// scheduler-query — the federation query client.
//
//	scheduler-query <peer> <op> [flags]   ask ONE peer (the default)
//	scheduler-query --all <op> [flags]    the authoritative aggregate (REMOTE-012)
//	scheduler-query --legacy-all <op>     the interim LOCAL fan-out (fallback)
//
// Flags may appear BEFORE, BETWEEN, or AFTER the positionals — every order
// the help text documents parses identically (`--all <op> --daemon-url URL`
// and `--daemon-url URL --all <op>` are the same invocation, SCHED-GAP-16932).
// Go's flag package stops parsing at the first non-flag token, so the
// positional hoist below is part of the contract, not an optimisation.
//
// THE ONE CONTRACT (spec §2/§3): this binary is a THIN adapter. It parses
// its surface, builds the §2.1 Query envelope, hands it to the SAME
// ask-side transport entry the aggregate consumes (bus.Client.Query,
// REMOTE-009 — cmd/schedulerd/federation_query.go names this consumer
// explicitly: "The ASK side is the library entry (bus.Client.Query) —
// consumed by the aggregate (REMOTE-012) and the CLI (REMOTE-011)"), and
// renders the §2.2 Response envelope VERBATIM plus a human summary. No
// answer is ever computed here — an adapter that computes an answer itself
// is a bug (spec §3; REMOTE-014's battery diffs this surface against the
// internal entry point, the oracle).
//
// DEGRADATION, HONESTLY (spec §5 + the REMOTE-003 rendering law): a peer
// that never answers inside budget_ms renders as a status="error"
// envelope with a stable code ("timeout" for a silent peer) and the peer's
// last-contact time from the local peer registry. The rendering vocabulary
// for such a peer is stale/error with a last-contact time — the CLI never
// claims a peer is gone. Exit codes: 0 every answer ok; 1 degraded (at
// least one peer stale/partial/error); 2 hard error (the query could not
// be attempted: usage, no relay URL, no peer registry to fan out over).
//
// --all is the INTERIM aggregate (deliverable 3): a bounded, concurrent
// fan-out of the same §2.1 envelope over the peers registered in the local
// REMOTE-003 registry, every peer rendered with its own status (a silent
// peer is a rendered row, never an omitted one). The merge semantic is
// deliberately NOT built here — the authoritative aggregate is REMOTE-012
// (spec §5); the output says so.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// cliDefaultRelayURL matches the daemon's [crier] default (main.go: the bus
// resolves url http://127.0.0.1:8767 when nothing is configured).
const cliDefaultRelayURL = "http://127.0.0.1:8767"

// defaultDBPath is the daemon's default --db (the peer registry lives in
// the scheduler's own database). A var so tests can pin $HOME-dependent
// resolution like cmd/schedulerd does.
var defaultDBPath = func() string {
	return os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db")
}

// argMap collects repeatable --arg k=v flags into the envelope's args
// object. Values are carried as strings — the shared entry point's arg
// readers accept string forms for every catalogue arg (argInt parses
// numeric strings; argSince parses RFC3339 strings).
type argMap map[string]any

func (a argMap) String() string {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

func (a argMap) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("--arg must be k=v (got %q)", s)
	}
	a[k] = v
	return nil
}

// cliParse is the parsed command surface BEFORE env layering: the raw flag
// values, the positional list in the order the operator typed it, and the
// resolved mode. A plain value so the whole parse is testable without a
// process (SCHED-GAP-16932's table tests assert exactly this struct).
type cliParse struct {
	positionals []string
	peer        string
	op          string
	allPeers    bool
	legacyAll   bool
	args        argMap
	want        string
	budgetMS    int
	corrID      string
	jsonOut     bool
	daemonURL   string
	relayURL    string
	dbPath      string
}

// newQueryFlagSet registers the documented surface (help.go) on a fresh
// set and fills p. Separate from main so the value-taking flags can be
// DERIVED from the set (valueFlagNames) instead of hand-listed: the
// hand-kept copy drifted, missed --daemon-url, and broke both orders of
// the documented aggregate shape.
func newQueryFlagSet(p *cliParse) *flag.FlagSet {
	fs := flag.NewFlagSet("scheduler-query", flag.ContinueOnError)
	// Errors are rendered by the caller (writeUsageError prints the message
	// plus the usage block); the set itself stays silent.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	p.args = argMap{}
	fs.BoolVar(&p.allPeers, "all", false, "Query EVERY peer via the daemon's authoritative aggregate (REMOTE-012, docs/federation-query-spec.md §5)")
	fs.BoolVar(&p.legacyAll, "legacy-all", false, "Interim LOCAL fan-out over the registered peers (bus only; no merge) — kept as fallback for a daemon without the aggregate; --all is the authoritative form")
	fs.StringVar(&p.want, "want", "", `Response mode: "answer" (default, all-or-error) or "partial" (best-effort with named gaps)`)
	fs.IntVar(&p.budgetMS, "budget-ms", 0, "Per-peer read budget in ms (0 = peer default; the CLI never waits past the budget)")
	fs.StringVar(&p.corrID, "corr-id", "", "Correlation/idempotency id (spec §2.5; default: minted unique per query)")
	fs.BoolVar(&p.jsonOut, "json", false, "Print the raw §2.2 response envelope(s) only — scripts parse this (--all prints the aggregate envelope)")
	fs.StringVar(&p.daemonURL, "daemon-url", "", "Scheduler daemon base URL for --all (env SCHEDULER_URL; default "+cliDefaultDaemonURL+")")
	fs.StringVar(&p.relayURL, "url", "", "Crier relay base URL (env CRIER_URL; default "+cliDefaultRelayURL+")")
	fs.StringVar(&p.dbPath, "db", "", "Scheduler database holding the peer registry (default "+defaultDBPath()+")")
	fs.Var(p.args, "arg", "Op argument k=v, repeatable (e.g. --arg filter=alpha --arg limit=5)")
	return fs
}

// parseArgs parses argv into the resolved surface. Positionals are hoisted
// wherever they appear (the documented shapes put flags before, between
// and after them), then the flag set parses the flag-only remainder and
// the mode/validation ladder runs. -h/--help surfaces as flag.ErrHelp.
func parseArgs(argv []string) (cliParse, error) {
	var p cliParse
	fs := newQueryFlagSet(&p)
	positionals, flagArgv, err := splitPositionals(argv, valueFlagNames(fs))
	if err != nil {
		return cliParse{}, err
	}
	if err := fs.Parse(flagArgv); err != nil {
		return cliParse{}, err
	}
	p.positionals = positionals
	if err := p.resolveMode(); err != nil {
		return cliParse{}, err
	}
	return p, nil
}

// resolveMode picks the invocation shape and validates it: --all/--legacy-all
// take exactly one positional (the op); the default takes <peer> <op>.
// --daemon-url alone never flips the mode — only --all/--legacy-all do.
func (p *cliParse) resolveMode() error {
	rest := p.positionals
	switch {
	case p.allPeers:
		if len(rest) != 1 {
			return fmt.Errorf("--all takes exactly one positional: <op> (got %d: %q)", len(rest), rest)
		}
		p.op = rest[0]
	case p.legacyAll:
		if len(rest) != 1 {
			return fmt.Errorf("--legacy-all takes exactly one positional: <op> (got %d: %q)", len(rest), rest)
		}
		p.op = rest[0]
	default:
		if len(rest) != 2 {
			return fmt.Errorf("expected: scheduler-query <peer> <op> [flags]  (or --all <op>) — got %d positional(s): %q", len(rest), rest)
		}
		p.peer, p.op = rest[0], rest[1]
	}
	if strings.TrimSpace(p.op) == "" {
		return fmt.Errorf("op is required (catalogue: peer.status, fleet.status, projects.list, queue.get, ticks.list, events.list)")
	}
	if !p.allPeers && !p.legacyAll && strings.TrimSpace(p.peer) == "" {
		return fmt.Errorf("peer id is required (or pass --all)")
	}
	if w := strings.TrimSpace(p.want); w != "" && w != "answer" && w != "partial" {
		return fmt.Errorf(`--want must be "answer" or "partial"`)
	}
	return nil
}

func main() {
	p, err := parseArgs(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/--help is not a usage mistake: the help block is the
			// answer, on stderr with exit 0 (the documented target —
			// docs/reference/cli-tools.md, "capture with 2>&1").
			printUsage(os.Stderr)
			os.Exit(exitOK)
		}
		writeUsageError(err, os.Stderr)
		os.Exit(exitHard)
	}

	// Env layers mirror the daemon's bus resolution (main.go): CRIER_URL /
	// CRIER_AUTH_TOKEN. There is deliberately NO token flag — credentials
	// never live in argv (GAP-038).
	effectiveURL := p.relayURL
	if effectiveURL == "" {
		effectiveURL = strings.TrimSpace(os.Getenv("CRIER_URL"))
	}
	if effectiveURL == "" {
		effectiveURL = cliDefaultRelayURL
	}
	token := strings.TrimSpace(os.Getenv("CRIER_AUTH_TOKEN"))
	caller := strings.TrimSpace(os.Getenv("SCHEDULER_ID"))
	if caller == "" {
		caller = database.ResolveDefaultSchedulerID()
	}
	effectiveDB := p.dbPath
	if effectiveDB == "" {
		effectiveDB = defaultDBPath()
	}
	effectiveDaemon := p.daemonURL
	if effectiveDaemon == "" {
		effectiveDaemon = strings.TrimSpace(os.Getenv("SCHEDULER_URL"))
	}
	if effectiveDaemon == "" {
		effectiveDaemon = cliDefaultDaemonURL
	}
	if effectiveURL == "" {
		fmt.Fprintln(os.Stderr, "scheduler-query: no relay URL configured (--url or CRIER_URL) — nothing to query")
		os.Exit(exitHard)
	}

	cfg := queryConfig{
		peer:      p.peer,
		allPeers:  p.allPeers,
		legacyAll: p.legacyAll,
		op:        p.op,
		args:      p.args,
		want:      strings.TrimSpace(p.want),
		budgetMS:  p.budgetMS,
		corrID:    strings.TrimSpace(p.corrID),
		jsonOut:   p.jsonOut,
		dbPath:    effectiveDB,
		daemonURL: effectiveDaemon,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
	}
	// The ONE transport entry: bus.Client.Query (REMOTE-009's ask side) —
	// the same library entry the REMOTE-012 aggregate consumes.
	client := bus.NewClient(true, effectiveURL, token, caller)
	os.Exit(runQuery(context.Background(), cfg, client))
}

// errMissingValue tags a flag that was given no value: the message names the
// flag and stands on its own. Split out so the renderer can suppress the
// usage block for this one class (SCHED-GAP-16932 acceptance: "a flag that
// is missing its value must fail with a message naming that flag and no
// help dump") — 60 lines of usage bury the single line that matters, and the
// message already says which flag and how to write it.
var errMissingValue = errors.New("flag needs an argument")

// writeUsageError renders a usage mistake to w. A missing flag value prints
// one line and stops; every other usage mistake keeps the full usage block,
// which is where the caller learns the surface.
func writeUsageError(err error, w io.Writer) {
	if _, writeErr := fmt.Fprintln(w, "scheduler-query: "+err.Error()); writeErr != nil {
		return // the writer is gone; the process is about to exit hard anyway
	}
	if errors.Is(err, errMissingValue) {
		return
	}
	printUsage(w)
}

// valueFlagNames reports which REGISTERED flags take a value — everything
// that is not a bool. Derived from the set (flag.Value's IsBoolFlag, the
// same test the flag package itself uses) so a new flag can never be
// forgotten here: that omission is what broke `--daemon-url` in both
// orders (SCHED-GAP-16932).
func valueFlagNames(fs *flag.FlagSet) map[string]bool {
	names := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			return
		}
		names[f.Name] = true
	})
	return names
}

// splitPositionals hoists positional arguments out of argv regardless of
// position and returns the positional list plus the flag-only argv for
// flag.Parse, so flags may be interspersed with positionals in any order
// (Go's flag stops at the first non-flag token, which would otherwise make
// both orders of the documented aggregate shape unreliable).
//
// valueFlags is the set of flags that take a value (valueFlagNames); a
// space-separated value stays attached to its flag (`--daemon-url URL`
// keeps the URL out of the positional list) while `--flag=v` tokens are
// self-contained. A value-taking flag whose next token is missing or is
// itself a flag is refused with the stdlib's "flag needs an argument"
// wording — never bound silently (a `--daemon-url --all` swallow is how
// the aggregate flag used to disappear). A numerically-shaped next token
// is still accepted, so `--budget-ms -1` keeps working. `--` ends flag
// parsing: everything after is positional (the stdlib termination token).
// Unknown `-*` tokens are left to flag.Parse to refuse.
func splitPositionals(argv []string, valueFlags map[string]bool) (positional, rest []string, err error) {
	noMoreFlags := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case noMoreFlags:
			positional = append(positional, a)
		case a == "--":
			noMoreFlags = true
		case strings.HasPrefix(a, "-") && a != "-":
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") || !valueFlags[name] {
				continue
			}
			if i+1 >= len(argv) {
				return nil, nil, fmt.Errorf("%w: -%s", errMissingValue, name)
			}
			next := argv[i+1]
			if strings.HasPrefix(next, "-") && next != "-" && !numericToken(next) {
				return nil, nil, fmt.Errorf("%w: -%s (next token %q is another flag; write -%s=%s for a value that starts with '-')", errMissingValue, name, next, name, next)
			}
			i++
			rest = append(rest, next)
		default:
			positional = append(positional, a)
		}
	}
	return positional, rest, nil
}

// numericToken reports whether s is a bare number (optionally signed) — the
// value shape a negative number needs so `--budget-ms -1` is not mistaken
// for a following flag.
func numericToken(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
