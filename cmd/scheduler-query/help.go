package main

// REMOTE-011 --help text (deliverable 4). Names the ops from the §2.3 read
// catalogue and every flag, states the degradation/exit-code contract, and
// carries the REMOTE-012 interim-aggregate note. Printed to the writer the
// caller chooses (stderr from flag.Usage, stdout for --help detection) and
// pinned by TestREMOTE011_HelpNamesCatalogue.

import (
	"fmt"
	"io"
)

// usageText is the full help block. The ops list is the §2.3 catalogue —
// keep in lockstep with internal/api's federationCatalogue and the MCP
// tool map (the shared entry point's unknown_op refusal lists the
// authoritative catalogue if the three ever drift).
const usageText = `scheduler-query — federation query client (REMOTE-011, docs/federation-query-spec.md §3 "CLI" row)

Usage:
  scheduler-query <peer> <op> [flags]     ask ONE peer
  scheduler-query --all <op> [flags]      the AUTHORITATIVE aggregate (REMOTE-012,
                                          docs/federation-query-spec.md §5) — delegates
                                          to the daemon's fan-out+merge and prints the
                                          merged §2.2 envelope (every peer's own status;
                                          a silent peer is a named row, never "down")
  scheduler-query --legacy-all <op>       the interim LOCAL bus fan-out, unmerged —
                                          kept as fallback for a daemon without the
                                          aggregate

Positionals:
  <peer>    the answering scheduler's id (as registered in the local peer registry)
  <op>      one of the read-catalogue ops:
              peer.status     identity, version, clock, load, uptime of the peer
              fleet.status    peers, projects, active ticks, budget of the peer
              projects.list   project rows (name, enabled, weight, priority, cooldown)
                              args: filter=<exact name>
              queue.get       the peer's ordered scheduling queue
              ticks.list      tick rows, newest first
                              args: since=<RFC3339> limit=<int>
              events.list     event rows, newest first
                              args: since=<RFC3339> limit=<int> severity=<string>
            (--all fans the chosen op out over every registered peer)

Flags:
  --arg k=v        op argument, repeatable (e.g. --arg filter=alpha --arg limit=5)
  --want MODE      "answer" (default, all-or-error) or "partial" (best-effort with named gaps)
  --budget-ms N    per-peer read budget in ms (0 = peer default; the CLI never waits past the budget)
  --corr-id ID     correlation/idempotency id (default: minted unique per query)
  --all            the authoritative aggregate (REMOTE-012) via the daemon's
                   /api/v1/federation/query surface
  --legacy-all     the interim local fan-out over the registered peers (no merge)
  --daemon-url URL scheduler daemon base URL for --all (env SCHEDULER_URL;
                   default http://127.0.0.1:9090)
  --json           print the raw §2.2 response envelope(s) only, one compact JSON object
                   per line (scripts parse this; --all prints the aggregate envelope)
  --url URL        Crier relay base URL (env CRIER_URL; default http://127.0.0.1:8767)
  --db PATH        scheduler database holding the peer registry
                   (default $HOME/.hermes/coding-hermes/scheduler.db)
  -h, --help       show this help

Environment:
  SCHEDULER_URL      daemon base URL for --all (overridden by --daemon-url)
  SCHEDULER_OPERATOR_TOKEN  operator credential the daemon's gate requires for --all
  CRIER_URL          relay base URL (overridden by --url)
  CRIER_AUTH_TOKEN   relay bearer token (no flag on purpose — credentials never live in argv)
  SCHEDULER_ID       this caller's scheduler id (default: derived from the hostname)

Degradation (spec §5): a peer that does not answer inside the budget renders a
status="error" envelope (code "timeout" for a silent peer) with the peer's
last-contact time from the local registry. A silent peer is never rendered as
"down" and never hangs the query past the budget. The aggregate carries the
same law per peer: every known peer appears with its own status and the named
gaps say which peers were silent and why.

Exit codes:
  0   every answer ok
  1   degraded — at least one peer answered stale/partial/error
  2   hard error — the query could not be attempted (usage, no relay URL,
      no peers registered for --legacy-all, or the --all daemon refused)
`

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usageText)
}
