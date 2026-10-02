package main

// REMOTE-011 (docs/federation-query-spec.md §3, the "CLI" row):
// scheduler-query — the federation query client.
//
//	scheduler-query <peer> <op> [flags]   ask ONE peer (the default)
//	scheduler-query --all <op> [flags]    the AUTHORITATIVE aggregate (REMOTE-012)
//	scheduler-query --legacy-all <op>     the interim LOCAL fan-out (fallback)
//
// THE ONE CONTRACT (spec §2/§3): this binary is a THIN adapter. It parses
// its surface, builds the §2.1 Query envelope, hands it to the SAME
// transport entry the other surfaces consume, and renders the §2.2
// Response envelope VERBATIM plus a human summary. No answer is ever
// computed here — an adapter that computes an answer itself is a bug
// (spec §3; REMOTE-014's battery diffs this surface against the internal
// entry point, the oracle).
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
// --all is the AUTHORITATIVE aggregate (deliverable 5, REMOTE-012): it
// DELEGATES to the daemon's aggregate on /api/v1/federation/query (the §5
// fan-out+merge behind the operator gate, replay window and read audit)
// and renders the §2.2 envelope verbatim — the merge is never duplicated
// here (spec §3: an adapter that computes an answer itself is a bug). A
// daemon that cannot be reached or refuses the credential is a named hard
// error (exit 2). --legacy-all keeps the interim local bus fan-out as the
// fallback for the window where no aggregate-capable daemon is up; its
// output still names the aggregate as authoritative.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// Exit codes (deliverable 2): 0 = ok, 1 = degraded (at least one peer
// stale/partial/error), 2 = hard error (the query could not be attempted).
const (
	exitOK       = 0
	exitDegraded = 1
	exitHard     = 2
)

// Response status vocabulary (spec §2.2). Local spellings — the CLI reads
// statuses off the wire envelope and must not import api's unexported
// constants (the §2.2 vocabulary is the contract; these are its copy).
const (
	respStatusOK      = "ok"
	respStatusPartial = "partial"
	respStatusError   = "error"
	respStatusStale   = "stale"
)

const (
	// cliContractVersion is stamped ONLY on envelopes the CLI builds
	// itself — the degradation envelope for a peer that produced no
	// answer (spec §2.6). An answered envelope carries the PEER's own
	// contract stamp verbatim. Kept in lockstep with api's
	// federationContractVersion ("1.0.0", REMOTE-008); the §6 battery
	// diffs surfaces against the internal entry point, so drift here is
	// catchable rather than silent.
	cliContractVersion = "1.0.0"

	// cliDefaultBudgetMS is the fan-out ceiling's basis when the operator
	// passes no --budget-ms — the same default the bus client waits by
	// and the MCP transport arms (both 10000). The envelope itself stays
	// budget-free when the flag is unset (absent = peer default, §2.1);
	// this number only sizes the aggregate safety deadline.
	cliDefaultBudgetMS = 10000

	// fanoutSlackMS is the extra wall time the --all ceiling grants over
	// the per-peer budget for scheduling the legs. The legs each enforce
	// their own budget inside the transport; the ceiling is the never-hang
	// safety net over the whole fan-out.
	fanoutSlackMS = 5000

	// fanoutConcurrency bounds the --all fan-out (deliverable 3: a
	// BOUNDED concurrent fan-out). Larger registries queue on the
	// semaphore; every leg still gets its full per-peer budget.
	fanoutConcurrency = 8
)

// queryTransport is the ask-side seam: the ONE way an answer enters this
// program. *bus.Client is the production implementation (REMOTE-009's
// Query + NextCorrID); tests inject fakes. The narrowness is the point —
// the thin-adapter law is checkable only if there is exactly one door.
type queryTransport interface {
	Query(ctx context.Context, peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error)
	NextCorrID() string
}

// queryConfig is the parsed command surface (main.go) plus the writers the
// output goes to (tests capture them). lastContacts is the registry's
// last-contact map, resolved ONCE per run (a database open costs real time
// on a loaded host — one read, never one per degraded peer).
type queryConfig struct {
	peer         string
	allPeers     bool
	legacyAll    bool
	op           string
	args         map[string]any
	want         string
	budgetMS     int
	corrID       string
	jsonOut      bool
	dbPath       string
	daemonURL    string
	lastContacts map[string]string
	stdout       io.Writer
	stderr       io.Writer
}

// runQuery executes one CLI invocation and returns the process exit code.
// It never panics and never outlives the budgets: every wait is bounded by
// the transport's budget_ms (bus.Query) or, for --all, the daemon's own
// aggregate ceiling.
func runQuery(ctx context.Context, cfg queryConfig, tr queryTransport) int {
	if cfg.corrID == "" {
		// The §2.1 required member: caller-assigned, unique per
		// (caller, minute). Minted HERE (not left to the transport's
		// internal fill) so the degradation envelope can echo the corr
		// id the peer never answered.
		cfg.corrID = tr.NextCorrID()
	}
	if cfg.allPeers {
		return runAggregate(ctx, cfg)
	}
	if cfg.legacyAll {
		return runFanout(ctx, cfg, tr)
	}
	return runSingle(ctx, cfg, tr, cfg.peer)
}

// runSingle asks ONE peer and renders the answer. Any transport failure
// degrades to a status="error" envelope (exit 1) — a silent peer is
// rendered, named, and given its last-contact time, never dropped and
// never confused with a locally-computed answer.
func runSingle(ctx context.Context, cfg queryConfig, tr queryTransport, peer string) int {
	q := buildEnvelope(cfg)
	resp, err := tr.Query(ctx, peer, q)
	if err != nil {
		resp = degradedEnvelope(q, peer, err)
	}
	// The human summary needs the peer's last contact; one registry read
	// per run, only when a degraded row will actually render it.
	if resp.Status != respStatusOK && cfg.lastContacts == nil && !cfg.jsonOut && cfg.dbPath != "" {
		if contacts, hadRegistry := loadLastContactsIfPresent(ctx, cfg.dbPath); hadRegistry {
			cfg.lastContacts = contacts
		}
	}
	renderPeer(cfg, peer, resp)
	if resp.Status == respStatusOK {
		return exitOK
	}
	return exitDegraded
}

// runFanout is the INTERIM aggregate (deliverable 3): the same §2.1
// envelope fanned out over the local registry's peers, bounded by
// fanoutConcurrency in-flight legs and a whole-run ceiling. Per-peer
// degradation is explicit — every peer renders (a timed-out peer renders
// status=error code=timeout, never an omitted row, spec §5). There is
// deliberately NO merge semantic: the output states the authoritative
// aggregate is REMOTE-012.
func runFanout(ctx context.Context, cfg queryConfig, tr queryTransport) int {
	peers, code := registryPeers(ctx, cfg.dbPath, cfg.stderr)
	if code != exitOK {
		return code
	}
	// Last contacts resolve ONCE with the registry read itself (the map
	// also carries the "was there a registry at all" answer: nil means
	// unknown, not empty).
	contacts, hadRegistry := loadLastContactsIfPresent(ctx, cfg.dbPath)
	if hadRegistry {
		cfg.lastContacts = contacts
	}
	_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: interim LOCAL fan-out over %d registered peer(s); the authoritative aggregate is the daemon's fleet.aggregate (REMOTE-012, docs/federation-query-spec.md §5) — use --all to delegate to it\n", len(peers))

	budget := cfg.budgetMS
	if budget <= 0 {
		budget = cliDefaultBudgetMS
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(budget+fanoutSlackMS)*time.Millisecond)
	defer cancel()

	type answer struct {
		peer string
		resp bus.ResponseEnvelope
	}
	answers := make([]answer, len(peers))
	sem := make(chan struct{}, fanoutConcurrency)
	var wg sync.WaitGroup
	for i, peer := range peers {
		wg.Add(1)
		go func(i int, peer string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			q := buildEnvelope(cfg)
			q.CorrID = tr.NextCorrID() // per-leg id: unique per (caller, minute), §2.1
			resp, err := tr.Query(ctx, peer, q)
			if err != nil {
				resp = degradedEnvelope(q, peer, err)
			}
			answers[i] = answer{peer: peer, resp: resp}
		}(i, peer)
	}
	wg.Wait()

	degraded := false
	for _, a := range answers {
		renderPeer(cfg, a.peer, a.resp)
		if a.resp.Status != respStatusOK {
			degraded = true
		}
	}
	if degraded {
		return exitDegraded
	}
	return exitOK
}

// buildEnvelope assembles the §2.1 Query envelope — the contract member
// set and nothing else: op, args verbatim, corr_id, budget_ms armed only
// when the operator set it (absent = peer default), want only when set
// (absent = "answer"). This is the whole "builds the envelope" step of the
// spec §3 adapter law.
func buildEnvelope(cfg queryConfig) bus.QueryEnvelope {
	q := bus.QueryEnvelope{
		Op:     cfg.op,
		Args:   cfg.args,
		CorrID: cfg.corrID,
		Want:   cfg.want,
	}
	if cfg.budgetMS > 0 {
		b := cfg.budgetMS
		q.BudgetMS = &b
	}
	return q
}

// cliDefaultDaemonURL matches the daemon's --listen default (main.go) —
// where the aggregate (REMOTE-012) lives when the operator changed nothing.
const cliDefaultDaemonURL = "http://127.0.0.1:9090"

// runAggregate is the AUTHORITATIVE --all (deliverable 5, REMOTE-012): it
// DELEGATES to the daemon's aggregate on /api/v1/federation/query — the
// §5 fan-out+merge behind the operator gate, replay window and read audit —
// and renders the §2.2 envelope verbatim. The merge is never duplicated in
// this binary (spec §3: an adapter that computes an answer itself is a
// bug). A degraded aggregate renders the same way a degraded peer does
// (exit 1); a daemon that cannot be reached or refuses is a named hard
// error (exit 2 — the query could not be attempted).
func runAggregate(ctx context.Context, cfg queryConfig) int {
	daemon := strings.TrimRight(cfg.daemonURL, "/")
	corrID := cfg.corrID
	if corrID == "" {
		corrID = "cli-agg-1"
	}
	// The envelope: op=fleet.aggregate + args.op (the read op), the §2.1
	// member set — nothing the surface would not accept from any caller.
	aggArgs := make(map[string]any, len(cfg.args)+1)
	for k, v := range cfg.args {
		aggArgs[k] = v
	}
	aggArgs["op"] = cfg.op
	envelope := map[string]any{
		"op":      "fleet.aggregate",
		"args":    aggArgs,
		"corr_id": corrID,
	}
	if cfg.want != "" {
		envelope["want"] = cfg.want
	}
	if cfg.budgetMS > 0 {
		envelope["budget_ms"] = cfg.budgetMS
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: marshal aggregate envelope: %v\n", err)
		return exitHard
	}

	budget := cfg.budgetMS
	if budget <= 0 {
		budget = cliDefaultBudgetMS
	}
	// The ceiling mirrors the surface's own budget+slack discipline; a
	// wedged daemon holds this client no longer than it holds the answer.
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(budget+fanoutSlackMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, daemon+"/api/v1/federation/query", bytes.NewReader(body))
	if err != nil {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: build aggregate request: %v\n", err)
		return exitHard
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operator-Token", os.Getenv("SCHEDULER_OPERATOR_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: aggregate at %s unreachable: %v — start the daemon or pass --daemon-url\n", daemon, err)
		return exitHard
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: read aggregate answer: %v\n", err)
		return exitHard
	}
	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: aggregate at %s refused (HTTP %d) — set SCHEDULER_OPERATOR_TOKEN; body: %s\n", daemon, resp.StatusCode, strings.TrimSpace(string(raw)))
		return exitHard
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: aggregate at %s answered HTTP %d: %s\n", daemon, resp.StatusCode, strings.TrimSpace(string(raw)))
		return exitHard
	}
	var env bus.ResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		_, _ = fmt.Fprintf(cfg.stderr, "scheduler-query: aggregate answer is not a §2.2 envelope: %v\n", err)
		return exitHard
	}
	renderAggregate(cfg, env)
	if env.Status != respStatusOK {
		return exitDegraded
	}
	return exitOK
}

// renderAggregate prints the authoritative aggregate: the §2.2 envelope
// verbatim (the contract members), plus in human mode the per-peer summary
// lines the --legacy-all view rendered — same vocabulary (stale/error,
// never "down"), one authoritative answer instead of N unmerged ones.
func renderAggregate(cfg queryConfig, env bus.ResponseEnvelope) {
	if cfg.jsonOut {
		writeEnvelopeJSON(cfg.stdout, env)
		return
	}
	_, _ = fmt.Fprintf(cfg.stdout, "scheduler-query: authoritative aggregate (REMOTE-012) op=%s status=%s as_of=%s\n", env.Op, env.Status, env.AsOf)
	if data, ok := env.Data.(map[string]any); ok {
		if rows, ok := data["peers"].([]any); ok {
			for _, r := range rows {
				row, ok := r.(map[string]any)
				if !ok {
					continue
				}
				peer, _ := row["peer"].(string)
				status, _ := row["status"].(string)
				stale, _ := row["stale"].(bool)
				line := fmt.Sprintf("  peer=%s status=%s stale=%t", peer, status, stale)
				if lc, ok := row["last_contact"].(string); ok && lc != "" {
					line += " last_contact=" + lc
				}
				if errObj, ok := row["error"].(map[string]any); ok {
					code, _ := errObj["code"].(string)
					line += " code=" + code
				}
				_, _ = fmt.Fprintln(cfg.stdout, line)
			}
		}
	}
	writeEnvelopeIndent(cfg.stdout, env)
}

// degradedEnvelope renders the status="error" envelope for a peer that
// produced NO answer: silent past budget_ms (code "timeout" — the §5
// degradation vocabulary), relay unreachable, reply stream closed, or the
// transport disabled. The error object names what happened to the
// QUESTION; the op is never answered locally.
func degradedEnvelope(q bus.QueryEnvelope, peer string, qerr error) bus.ResponseEnvelope {
	code := "transport_error"
	switch {
	case errors.Is(qerr, bus.ErrQueryTimeout):
		code = "timeout"
	case errors.Is(qerr, bus.ErrReplyClosed):
		code = "reply_closed"
	case errors.Is(qerr, bus.ErrDisabled):
		code = "bus_disabled"
	}
	return bus.ResponseEnvelope{
		CorrID:   q.CorrID,
		Op:       q.Op,
		Peer:     peer,
		Status:   respStatusError,
		Gaps:     []bus.FederationGap{},
		Error:    &bus.FederationError{Code: code, Message: qerr.Error()},
		Contract: cliContractVersion,
	}
}

// renderPeer writes one peer's answer: the §2.2 envelope VERBATIM (the
// JSON member set is the contract) plus, in human mode, a one-line summary.
// In --json mode the envelope is the only stdout content (compact, one
// line per peer — NDJSON for scripts). The REMOTE-012 note for --all goes
// to stderr in both modes so stdout stays parseable.
func renderPeer(cfg queryConfig, peer string, resp bus.ResponseEnvelope) {
	if cfg.jsonOut {
		writeEnvelopeJSON(cfg.stdout, resp)
		return
	}
	line := fmt.Sprintf("scheduler-query: peer=%s op=%s status=%s as_of=%s age_ms=%d gaps=%d",
		peer, resp.Op, resp.Status, resp.AsOf, resp.AgeMS, len(resp.Gaps))
	if resp.Error != nil {
		// Degradation, honestly: the code plus the peer's last-contact
		// time (from the once-per-run registry read). The vocabulary is
		// stale/error — the summary never claims the peer is gone.
		line += fmt.Sprintf(" code=%s last_contact=%s", resp.Error.Code, lastContactFor(cfg, peer))
	}
	_, _ = fmt.Fprintln(cfg.stdout, line)
	writeEnvelopeIndent(cfg.stdout, resp)
}

// writeEnvelopeJSON writes the raw §2.2 envelope, one compact line
// (NDJSON — the --json contract scripts parse).
func writeEnvelopeJSON(w io.Writer, resp bus.ResponseEnvelope) {
	b, err := json.Marshal(resp)
	if err != nil {
		// The envelope came off the wire as JSON; a re-marshal cannot
		// fail in practice. Fail loud rather than print a lie.
		_, _ = fmt.Fprintf(w, "{\"status\":\"error\",\"error\":{\"code\":\"internal\",\"message\":%q}}\n", err.Error())
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// writeEnvelopeIndent writes the §2.2 envelope verbatim, indented (the
// human mode's verbatim block under the summary line).
func writeEnvelopeIndent(w io.Writer, resp bus.ResponseEnvelope) {
	b, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(w, "{\"status\":\"error\",\"error\":{\"code\":\"internal\",\"message\":%q}}\n", err.Error())
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// registryPeers resolves the --all target set: the peers registered in the
// local REMOTE-003 registry (ListPeers, already ordered by id — the stable
// output order). A missing/unreadable/empty registry is a hard error: the
// aggregate has nothing to fan out over, and inventing a target set would
// be a fabricated answer.
func registryPeers(ctx context.Context, dbPath string, errw io.Writer) ([]string, int) {
	if _, err := os.Stat(dbPath); err != nil {
		_, _ = fmt.Fprintf(errw, "scheduler-query: no peer registry at %s — --all reads the local registry; register peers or target one peer\n", dbPath)
		return nil, exitHard
	}
	db, err := database.InitDB(dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "scheduler-query: open peer registry %s: %v\n", dbPath, err)
		return nil, exitHard
	}
	defer func() { _ = db.Close() }()
	peers, err := database.ListPeers(ctx, db)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "scheduler-query: read peer registry: %v\n", err)
		return nil, exitHard
	}
	if len(peers) == 0 {
		_, _ = fmt.Fprintf(errw, "scheduler-query: no peers registered in %s — the aggregate has nothing to fan out over\n", dbPath)
		return nil, exitHard
	}
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.ID)
	}
	return ids, exitOK
}

// lastContactFor returns the peer's last-contact value from the
// once-per-run map, with the no-reason law applied to every absence: an
// unreadable registry is "unknown", a registered-but-never-heartbeat peer
// is "never".
func lastContactFor(cfg queryConfig, peer string) string {
	if cfg.lastContacts == nil {
		return "unknown"
	}
	if v, ok := cfg.lastContacts[peer]; ok && v != "" {
		return v
	}
	return "never"
}

// loadLastContactsIfPresent reads the registry's last-contact map. The
// bool reports whether a registry EXISTS — false (missing/unreadable)
// leaves the caller's map nil, which lastContactFor renders as the
// honest "unknown". One database open per run.
func loadLastContactsIfPresent(ctx context.Context, dbPath string) (map[string]string, bool) {
	if dbPath == "" {
		return nil, false
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}
	contacts, err := loadLastContacts(ctx, dbPath)
	if err != nil {
		return nil, false
	}
	return contacts, true
}

// loadLastContacts opens the scheduler database and reads every peer's
// most recent heartbeat (REMOTE-003 registry rows) into a map.
func loadLastContacts(ctx context.Context, dbPath string) (map[string]string, error) {
	db, err := database.InitDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open peer registry: %w", err)
	}
	defer func() { _ = db.Close() }()
	peers, err := database.ListPeers(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read peer registry: %w", err)
	}
	out := make(map[string]string, len(peers))
	for _, p := range peers {
		out[p.ID] = p.LastContact
	}
	return out, nil
}
