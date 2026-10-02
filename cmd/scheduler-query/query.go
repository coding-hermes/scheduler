package main

// REMOTE-011 query core (docs/federation-query-spec.md §3, the "CLI" row):
// the thin-adapter machinery. Everything here serves the §3 law — parse the
// surface → build the §2.1 envelope → call the SAME transport entry the
// other surfaces consume → render the §2.2 envelope verbatim — and the §5
// degradation model: a peer that does not answer is a rendered envelope
// with a stable code and a last-contact time, never a hang, never a panic,
// never a locally-computed answer to the op itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	op           string
	args         map[string]any
	want         string
	budgetMS     int
	corrID       string
	jsonOut      bool
	dbPath       string
	lastContacts map[string]string
	stdout       io.Writer
	stderr       io.Writer
}

// runQuery executes one CLI invocation and returns the process exit code.
// It never panics and never outlives the budgets: every wait is bounded by
// the transport's budget_ms (bus.Query) or, for --all, the fan-out ceiling.
func runQuery(ctx context.Context, cfg queryConfig, tr queryTransport) int {
	if cfg.corrID == "" {
		// The §2.1 required member: caller-assigned, unique per
		// (caller, minute). Minted HERE (not left to the transport's
		// internal fill) so the degradation envelope can echo the corr
		// id the peer never answered.
		cfg.corrID = tr.NextCorrID()
	}
	if cfg.allPeers {
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
	fmt.Fprintf(cfg.stderr, "scheduler-query: interim thin fan-out over %d registered peer(s); the authoritative aggregate is REMOTE-012 (docs/federation-query-spec.md §5)\n", len(peers))

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
	fmt.Fprintln(cfg.stdout, line)
	writeEnvelopeIndent(cfg.stdout, resp)
}

// writeEnvelopeJSON writes the raw §2.2 envelope, one compact line
// (NDJSON — the --json contract scripts parse).
func writeEnvelopeJSON(w io.Writer, resp bus.ResponseEnvelope) {
	b, err := json.Marshal(resp)
	if err != nil {
		// The envelope came off the wire as JSON; a re-marshal cannot
		// fail in practice. Fail loud rather than print a lie.
		fmt.Fprintf(w, "{\"status\":\"error\",\"error\":{\"code\":\"internal\",\"message\":%q}}\n", err.Error())
		return
	}
	w.Write(append(b, '\n'))
}

// writeEnvelopeIndent writes the §2.2 envelope verbatim, indented (the
// human mode's verbatim block under the summary line).
func writeEnvelopeIndent(w io.Writer, resp bus.ResponseEnvelope) {
	b, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		fmt.Fprintf(w, "{\"status\":\"error\",\"error\":{\"code\":\"internal\",\"message\":%q}}\n", err.Error())
		return
	}
	w.Write(append(b, '\n'))
}

// registryPeers resolves the --all target set: the peers registered in the
// local REMOTE-003 registry (ListPeers, already ordered by id — the stable
// output order). A missing/unreadable/empty registry is a hard error: the
// aggregate has nothing to fan out over, and inventing a target set would
// be a fabricated answer.
func registryPeers(ctx context.Context, dbPath string, errw io.Writer) ([]string, int) {
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(errw, "scheduler-query: no peer registry at %s — --all reads the local registry; register peers or target one peer\n", dbPath)
		return nil, exitHard
	}
	db, err := database.InitDB(dbPath)
	if err != nil {
		fmt.Fprintf(errw, "scheduler-query: open peer registry %s: %v\n", dbPath, err)
		return nil, exitHard
	}
	defer func() { _ = db.Close() }()
	peers, err := database.ListPeers(ctx, db)
	if err != nil {
		fmt.Fprintf(errw, "scheduler-query: read peer registry: %v\n", err)
		return nil, exitHard
	}
	if len(peers) == 0 {
		fmt.Fprintf(errw, "scheduler-query: no peers registered in %s — the aggregate has nothing to fan out over\n", dbPath)
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
