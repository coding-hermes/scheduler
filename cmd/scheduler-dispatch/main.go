// scheduler-dispatch — the operator surface of the DISPATCH verb
// (SCHED-GAP-1665 / the dispatch leg, docs/dispatch-spec.md).
//
//	scheduler-dispatch <agent-id> --lane <lane> (--board <ref> | --workdir <ref>) [flags]
//
// The binary is a THIN adapter, exactly like cmd/scheduler-query (REMOTE-011
// §3): it parses its surface, builds a bus.WorkItem, and hands it to the
// SAME library entry the daemon consumes (bus.Client.Dispatch). It computes
// nothing — no target resolution, no lane lookup, no retry policy. Those
// belong to the scheduler; an adapter that invents them is a bug.
//
// The work item carries the LANE the scheduler picked, a board/workdir
// REFERENCE the agent resolves to its own checkout, and a correlation id.
// It never carries a task id: the scheduler picks the LANE, the foreman
// picks the TASK.
//
// Exit codes (mirroring cmd/scheduler-query's discipline):
//
//	0  the relay ACCEPTED the delivery (a durable hand-out; not execution)
//	1  not accepted — refused, unknown target, unreachable relay, or any
//	   other non-accept: the job is NOT handed out
//	2  usage / configuration error (nothing was attempted)
//
// SECURITY: the relay credential comes from CRIER_AUTH_TOKEN or the
// configured env only. There is deliberately NO --token flag — credentials
// in argv leak through ps (GAP-038).
//
// EVIDENCE BEFORE ACTION: --dry-run renders the endpoint and the exact
// request body (byte-identical to the wire body) without opening a
// connection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
)

// defaultRelayURL matches the daemon's [crier] default and
// cmd/scheduler-query's cliDefaultRelayURL: a local relay when nothing is
// configured.
const defaultRelayURL = "http://127.0.0.1:8767"

// envOr returns the first non-empty environment value named by keys.
func envOr(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// defaultSchedulerID resolves the bus identity: SCHEDULER_ID, else the
// short hostname (the scheduler's own default, docs/remote-spec.md §1).
func defaultSchedulerID() string {
	if id := envOr("SCHEDULER_ID"); id != "" {
		return id
	}
	if h, err := os.Hostname(); err == nil {
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
		if h != "" {
			return h
		}
	}
	return "scheduler"
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `usage: scheduler-dispatch <agent-id> --lane <lane> (--board <ref> | --workdir <ref>) [flags]

Hand ONE unit of work to ONE named agent over the Crier relay's durable
inbox. The scheduler picks the LANE; the foreman picks the TASK — the
payload never names a task.

flags:
  --lane <lane>          lane whose work this is (required)
  --board <ref>          board reference the agent resolves (path, or path@host)
  --workdir <ref>        workdir reference the agent resolves
  --corr-id <id>         correlation id (default: minted unique; reused as the
                         relay's request id + idempotency key, so retrying the
                         SAME id is safe)
  --url <url>            Crier relay base URL (env CRIER_URL; default %s)
  --scheduler-id <id>    sender identity (env SCHEDULER_ID; default %s)
  --dry-run              print the endpoint + exact body, send nothing
  --json                 print the receipt as JSON

env: CRIER_URL, CRIER_AUTH_TOKEN (the credential — never a flag), SCHEDULER_ID

exit: 0 accepted; 1 not accepted (refused / unknown target / unreachable); 2 usage
`, defaultRelayURL, defaultSchedulerID())
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can drive the whole surface
// (usage, dry run, dry-run-vs-wire identity) without spawning a process.
func run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scheduler-dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lane := fs.String("lane", "", "lane whose work this is (required)")
	board := fs.String("board", "", "board reference the agent resolves (path or path@host)")
	workdir := fs.String("workdir", "", "workdir reference the agent resolves")
	corrID := fs.String("corr-id", "", "correlation id (default minted)")
	relayURL := fs.String("url", "", "Crier relay base URL (env CRIER_URL)")
	schedulerID := fs.String("scheduler-id", "", "sender scheduler identity (env SCHEDULER_ID)")
	dryRun := fs.Bool("dry-run", false, "print the endpoint + exact body, send nothing")
	jsonOut := fs.Bool("json", false, "print the receipt as JSON")
	fs.Usage = func() { usage(stderr) }

	positional, flags := splitPositionals(argv)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	args := append(positional, fs.Args()...)
	if len(args) != 1 {
		usage(stderr)
		return 2
	}
	agent := strings.TrimSpace(args[0])

	url := firstNonEmpty(*relayURL, envOr("CRIER_URL"), defaultRelayURL)
	sfid := firstNonEmpty(*schedulerID, defaultSchedulerID())
	token := envOr("CRIER_AUTH_TOKEN")

	item := bus.WorkItem{
		Lane:    *lane,
		Board:   *board,
		Workdir: *workdir,
		CorrID:  *corrID,
	}

	// Render (and validate) ONCE. An incomplete work item is a usage error —
	// nothing was attempted (exit 2) — and the correlation id minted here is
	// the one the real request carries, so the dry run is byte-identical to
	// the wire body.
	endpoint, body, corr, err := bus.BuildDispatch(url, sfid, agent, item, time.Now())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduler-dispatch: %v\n", err)
		return 2
	}
	if *dryRun {
		_, _ = fmt.Fprintf(stdout, "DRY RUN (nothing sent)\n  POST %s\n  corr_id: %s\n  body: %s\n", endpoint, corr, body)
		return 0
	}
	// Reuse the rendered correlation id so a retry of this exact command is
	// the SAME idempotency key (one key, one message).
	item.CorrID = corr

	client := bus.NewClient(true, url, token, sfid)
	receipt, err := client.Dispatch(context.Background(), agent, item)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduler-dispatch: NOT dispatched: %v\n", err)
		return 1
	}
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(receipt)
		return 0
	}
	transport := receipt.Transport
	if transport == "" {
		transport = "accepted"
	}
	_, _ = fmt.Fprintf(stdout, "DISPATCHED agent=%s lane=%s corr=%s transport=%s message=%s replay=%t\n",
		receipt.AgentID, item.Lane, receipt.CorrID, transport, dash(receipt.MessageID), receipt.IdempotentReplay)
	return 0
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// dash renders an empty id readably in the human line.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitPositionals hoists positional arguments out of argv wherever they
// appear, so flags may follow the agent id (the same courtesy
// cmd/scheduler-query extends, because Go's flag package stops at the first
// non-flag token). It mirrors the flag package's value consumption, so a
// `--lane auger` value never leaks into the positional list.
func splitPositionals(argv []string) (positional, flags []string) {
	valueFlags := map[string]bool{
		"lane": true, "board": true, "workdir": true,
		"corr-id": true, "url": true, "scheduler-id": true,
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
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	return positional, flags
}
