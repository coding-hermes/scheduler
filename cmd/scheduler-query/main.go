// REMOTE-011 (docs/federation-query-spec.md §3, the "CLI" row):
// scheduler-query — the federation query client.
//
//	scheduler-query <peer> <op> [flags]   ask ONE peer (the default)
//	scheduler-query --all <op> [flags]    interim thin fan-out over the registered peers
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
	"flag"
	"fmt"
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

func main() {
	// The spec §3 CLI row shows flags AFTER the positionals
	// (`scheduler query <peer> <op> [--args k=v] [--want partial]`), so
	// positionals are hoisted wherever they appear before flag.Parse —
	// Go's flag stops at the first non-flag token, which would otherwise
	// turn every trailing flag into a bogus positional.
	positional, argv := splitPositionals(os.Args[1:])
	queryArgs := argMap{}
	allPeers := flag.Bool("all", false, "Query EVERY peer in the local peer registry (interim thin fan-out; the authoritative aggregate is REMOTE-012)")
	want := flag.String("want", "", `Response mode: "answer" (default, all-or-error) or "partial" (best-effort with named gaps)`)
	budgetMS := flag.Int("budget-ms", 0, "Per-peer read budget in ms (0 = peer default; the CLI never waits past the budget)")
	corrID := flag.String("corr-id", "", "Correlation/idempotency id (spec §2.5; default: minted unique per query)")
	jsonOut := flag.Bool("json", false, "Print the raw §2.2 response envelope(s) only — scripts parse this (--all prints one envelope per line)")
	relayURL := flag.String("url", "", "Crier relay base URL (env CRIER_URL; default "+cliDefaultRelayURL+")")
	dbPath := flag.String("db", "", "Scheduler database holding the peer registry (default "+defaultDBPath()+")")
	flag.Var(queryArgs, "arg", "Op argument k=v, repeatable (e.g. --arg filter=alpha --arg limit=5)")
	flag.Usage = func() { printUsage(os.Stderr) }
	_ = flag.CommandLine.Parse(argv)

	rest := positional
	var peer, op string
	switch {
	case *allPeers:
		if len(rest) != 1 {
			usageExit("--all takes exactly one positional: <op>")
		}
		op = rest[0]
	default:
		if len(rest) != 2 {
			usageExit("expected: scheduler-query <peer> <op> [flags]  (or --all <op>)")
		}
		peer, op = rest[0], rest[1]
	}
	if strings.TrimSpace(op) == "" {
		usageExit("op is required (catalogue: peer.status, fleet.status, projects.list, queue.get, ticks.list, events.list)")
	}
	if !*allPeers && strings.TrimSpace(peer) == "" {
		usageExit("peer id is required (or pass --all)")
	}
	if w := strings.TrimSpace(*want); w != "" && w != "answer" && w != "partial" {
		usageExit(`--want must be "answer" or "partial"`)
	}

	// Env layers mirror the daemon's bus resolution (main.go): CRIER_URL /
	// CRIER_AUTH_TOKEN. There is deliberately NO token flag — credentials
	// never live in argv (GAP-038).
	effectiveURL := *relayURL
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
	effectiveDB := *dbPath
	if effectiveDB == "" {
		effectiveDB = defaultDBPath()
	}
	if effectiveURL == "" {
		fmt.Fprintln(os.Stderr, "scheduler-query: no relay URL configured (--url or CRIER_URL) — nothing to query")
		os.Exit(exitHard)
	}

	cfg := queryConfig{
		peer:     peer,
		allPeers: *allPeers,
		op:       op,
		args:     queryArgs,
		want:     strings.TrimSpace(*want),
		budgetMS: *budgetMS,
		corrID:   strings.TrimSpace(*corrID),
		jsonOut:  *jsonOut,
		dbPath:   effectiveDB,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}
	// The ONE transport entry: bus.Client.Query (REMOTE-009's ask side) —
	// the same library entry the REMOTE-012 aggregate consumes.
	client := bus.NewClient(true, effectiveURL, token, caller)
	os.Exit(runQuery(context.Background(), cfg, client))
}

// usageExit prints the usage text and exits with the hard-error code. A
// usage mistake is a hard error: the query was never attempted.
func usageExit(msg string) {
	fmt.Fprintln(os.Stderr, "scheduler-query: "+msg)
	printUsage(os.Stderr)
	os.Exit(exitHard)
}

// splitPositionals hoists positional arguments out of argv regardless of
// position (the spec's CLI row puts flags after them), returning the
// positional list and the flag-only argv for flag.Parse. Value-taking
// flags are known by name so a space-separated value stays attached
// (`--db /path/db` keeps its value; `--arg k=v` does not hoist k=v as a
// positional). `--` ends flag parsing: everything after is positional
// (the stdlib termination token). Unknown `-*` tokens are left to
// flag.Parse to refuse.
func splitPositionals(argv []string) (positional, rest []string) {
	valueFlags := map[string]bool{
		"want": true, "budget-ms": true, "corr-id": true,
		"url": true, "db": true, "arg": true,
	}
	noMoreFlags := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case noMoreFlags:
			positional = append(positional, a)
		case a == "--":
			noMoreFlags = true
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				name = name[:eq]
			} else if valueFlags[name] && i+1 < len(argv) {
				// Go's flag consumes the next token as the value
				// unconditionally — mirror that so the token never
				// leaks into the positional list.
				i++
				rest = append(rest, argv[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	return positional, rest
}
