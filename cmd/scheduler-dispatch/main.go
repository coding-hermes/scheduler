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
// REFERENCE the agent resolves to its own checkout, a correlation id, the
// configured reply target, and the rendered directive.
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
	"github.com/coding-hermes/scheduler/internal/clock"

	"github.com/BurntSushi/toml"
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
  --text <directive>     what the node's harness is asked to carry. Without it
                         the payload has no directive member at all and the
                         node receives the bare envelope, so an operator
                         dispatch carries its own prompt here.
  --text-file <path>     read the directive from a file (same member; mutually
                         exclusive with --text — quoting a long prompt in argv
                         is how a shell mangles it)
  --url <url>            Crier relay base URL (env CRIER_URL; config [crier].url; default %s)
  --scheduler-id <id>    sender identity (env SCHEDULER_ID; default %s)
  --reply-to <id>        reply inbox identity (env CRIER_REPLY_TO; config [crier].reply_to)
  --config <path>        TOML config file (env SCHEDULER_CONFIG)
  --dry-run              print the endpoint + exact body, send nothing
  --json                 print the receipt as JSON

env: CRIER_URL, CRIER_AUTH_TOKEN (the credential — never a flag), SCHEDULER_ID, CRIER_REPLY_TO, SCHEDULER_CONFIG

exit: 0 accepted; 1 not accepted (refused / unknown target / unreachable); 2 usage
`, defaultRelayURL, defaultSchedulerID())
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can drive the whole surface
// (usage, dry run, dry-run-vs-wire identity) without spawning a process.
func run(argv []string, stdout, stderr io.Writer) int {
	return runAt(argv, stdout, stderr, clock.Real().Now)
}

func runAt(argv []string, stdout, stderr io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("scheduler-dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lane := fs.String("lane", "", "lane whose work this is (required)")
	board := fs.String("board", "", "board reference the agent resolves (path or path@host)")
	workdir := fs.String("workdir", "", "workdir reference the agent resolves")
	corrID := fs.String("corr-id", "", "correlation id (default minted)")
	text := fs.String("text", "", "directive the node's harness is asked to carry (inline)")
	textFile := fs.String("text-file", "", "read the directive from a file (mutually exclusive with --text)")
	relayURL := fs.String("url", "", "Crier relay base URL (env CRIER_URL; config [crier].url)")
	schedulerID := fs.String("scheduler-id", "", "sender scheduler identity (env SCHEDULER_ID)")
	replyTo := fs.String("reply-to", "", "reply inbox identity (env CRIER_REPLY_TO; config [crier].reply_to)")
	configPath := fs.String("config", "", "TOML config file (env SCHEDULER_CONFIG)")
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

	configFile := firstNonEmpty(*configPath, envOr("SCHEDULER_CONFIG"))
	var fileConfig struct {
		Crier struct {
			URL     string `toml:"url"`
			ReplyTo string `toml:"reply_to"`
		} `toml:"crier"`
	}
	if configFile != "" {
		if _, err := toml.DecodeFile(configFile, &fileConfig); err != nil {
			_, _ = fmt.Fprintf(stderr, "scheduler-dispatch: config %s: %v\n", configFile, err)
			return 2
		}
	}
	url := firstNonEmpty(*relayURL, envOr("CRIER_URL"), fileConfig.Crier.URL, defaultRelayURL)
	sfid := firstNonEmpty(*schedulerID, defaultSchedulerID())
	reply := firstNonEmpty(*replyTo, envOr("CRIER_REPLY_TO"), fileConfig.Crier.ReplyTo)
	token := envOr("CRIER_AUTH_TOKEN")

	directive, code := resolveText(*text, *textFile, stderr)
	if code != 0 {
		return code
	}
	if reply == "" {
		_, _ = fmt.Fprintln(stderr, "scheduler-dispatch: reply target is required (--reply-to, CRIER_REPLY_TO, or [crier].reply_to)")
		return 2
	}

	if strings.TrimSpace(directive) == "" {
		directive = renderDirective(*lane, *board, *workdir)
	}
	item := makeWorkItem(*lane, *board, *workdir, *corrID, reply, directive)

	// Render (and validate) ONCE. An incomplete work item is a usage error —
	// nothing was attempted (exit 2) — and the correlation id minted here is
	// the one the real request carries, so the dry run is byte-identical to
	// the wire body.
	dispatchNow := now()
	endpoint, body, corr, err := bus.BuildDispatch(url, sfid, agent, item, dispatchNow)
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

	client := bus.NewClient(true, url, token, sfid, bus.WithClock(func() time.Time { return dispatchNow }))
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

func makeWorkItem(lane, board, workdir, corrID, replyTo, text string) bus.WorkItem {
	return bus.WorkItem{Lane: lane, Board: board, Workdir: workdir, CorrID: corrID, ReplyTo: replyTo, Text: text}
}

// renderDirective supplies the same compact lane/reference prose used when
// the operator does not provide an explicit prompt.
func renderDirective(lane, board, workdir string) string {
	return fmt.Sprintf("[Scheduler dispatch] Carry out lane %q using board %q and workdir %q.", lane, board, workdir)
}

// resolveText turns the directive flags into the string the work item carries.
// Both flags at once is a usage error: a caller that sets two directives has
// not decided which one the node should run, and a silently-preferred one is
// how an operator ships the wrong prompt. An empty result is replaced by the
// rendered lane/reference directive before dispatch.
func resolveText(inline, path string, stderr io.Writer) (string, int) {
	if strings.TrimSpace(inline) != "" && strings.TrimSpace(path) != "" {
		_, _ = fmt.Fprintln(stderr, "scheduler-dispatch: --text and --text-file are mutually exclusive")
		return "", 2
	}
	if strings.TrimSpace(path) == "" {
		return inline, 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduler-dispatch: --text-file %s: %v\n", path, err)
		return "", 2
	}
	if strings.TrimSpace(string(b)) == "" {
		_, _ = fmt.Fprintf(stderr, "scheduler-dispatch: --text-file %s is empty\n", path)
		return "", 2
	}
	return string(b), 0
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
		"reply-to": true, "config": true,
		// The directive flags take a value too. Omitting them here is not a
		// cosmetic slip: their argument is hoisted into the positional list,
		// the flag package then reports "flag needs an argument", and the
		// surface exits 2 while claiming a usage problem the caller did not
		// have. Every value-taking flag must be mirrored in this map.
		"text": true, "text-file": true,
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
