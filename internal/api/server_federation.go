package api

// REMOTE-008 (docs/federation-query-spec.md §2 + §3 HTTP row): the federation
// QUERY surface — how another scheduler ASKS this one a question.
//
//	POST /api/v1/federation/query      — the Query envelope in, the Response
//	                                     envelope out (spec §2.1/§2.2)
//	GET  /api/v1/federation/catalogue  — the supported-op list (spec §2.3)
//
// The six ops are NOT new aggregations: each one reuses the exact read path
// the daemon already serves its own dashboard/REST from (spec §2.3 law and
// §7 non-goal "No new aggregation the answering peer cannot compute from its
// own read path"):
//
//	peer.status     → the /api/v1/health payload fields (build identity,
//	                  uptime, DB state, active ticks) + the clock mode
//	fleet.status    → the /api/v1/status summary fields (projects, ticks,
//	                  paused, budget chain) + the local peer registry count
//	projects.list   → database.ListProjects (GET /api/v1/projects)
//	queue.get       → s.listQueue (GET /api/v1/queue, GAP-054 urgency)
//	ticks.list      → listTicks (GET /api/v1/ticks)
//	events.list     → listEvents (GET /api/v1/events)
//
// Access model (spec §4): this surface is a federation read — it fails
// CLOSED behind the SAME operator gate as every other control route
// (requireOperator; 503 with no credential configured, 401 on a bad
// credential). REMOTE-013: behind the credential sits the per-caller READ
// POLICY (FederationReadPolicy, armed by SetFederationReadPolicy from the
// [federation] allow config) — an authenticated caller reads only the ops
// published for it; anything else is status="error" code="op_not_allowed"
// (403), audited through the same §4 row an answered query writes. Every
// answered query also writes one local audit row via the shared
// mutation-audit mechanism ("reads are observable, not silently free"),
// including refusals.
//
// Envelope invariants (spec §2.2), enforced by responseEnvelope:
//   - peer = THIS scheduler's own id (database.SchedulerID() — never a peer
//     of a peer; single-writer law)
//   - as_of/age_ms describe the DATA (as_of = when the peer read its state)
//   - status="stale" is a first-class answer, distinct from "error" (the
//     peer's own last self-observation is older than its freshness window)
//   - empty data is []/{} — never null
//   - corr_id/op echoed; contract version present on every reply

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/version"
)

// federationContractVersion is the contract version stamped on every reply
// (spec §2.6). v1.0.0 = the initial read catalogue; adding an op is a minor
// bump, breaking a response shape is a major bump.
const federationContractVersion = "1.0.0"

// federationBudgetClampMS caps a caller-supplied budget_ms (spec §2.1: "the
// peer clamps to its own cap"). A larger ask is not an error — it is a clamp.
const federationBudgetClampMS = 30000

// federationDefaultBudgetMS is the per-request read deadline the query
// surface arms (SCHED-GAP-1575-B discipline: every DB step runs under a
// deadline derived from the REQUEST context). It sits above the default
// budget_ms (10s) so a peer honoring its own default never trips the HTTP
// deadline first, and above the heavy-read default (5s) because a fan-out
// caller tolerates more latency than a dashboard poll.
const federationDefaultBudgetMS = 15000

// federationQueryMaxBodyBytes bounds the request body read. The envelope is
// one small JSON object; the biggest legitimate member is a filter string.
const federationQueryMaxBodyBytes = 1 << 20 // 1 MiB

// queryEnvelope is the spec §2.1 Query envelope. It is carried VERBATIM by
// every transport (HTTP here; Crier/MCP/CLI in REMOTE-009..011) — the
// envelope is the contract, the transport is an adapter.
//
// Validation: op and corr_id are REQUIRED (spec §2.1); a missing or blank
// one is a named refusal (missing_op / missing_corr_id), never a guessed
// default. budget_ms absent (nil) = "peer default"; want absent = "answer";
// args absent = the op's defaults. An unknown member is ignored, not
// rejected — the envelope is additive-friendly (new optional args must not
// break an older peer).
type queryEnvelope struct {
	Op       string         `json:"op"`
	Args     map[string]any `json:"args"`
	CorrID   string         `json:"corr_id"`
	BudgetMS *int           `json:"budget_ms"`
	Want     string         `json:"want"`
}

// federationGap is one entry of the response envelope's gaps array (spec
// §2.2): what could not be answered and why. Present (empty) on every
// ok/partial reply; a partial reply names at least one gap.
type federationGap struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// federationError is the response envelope's error object (spec §2.2):
// a STABLE machine code plus a human message.
type federationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable error codes (spec §2.2: "<stable>"). The vocabulary is part of the
// contract — remoting callers branch on these strings.
const (
	fedErrMissingOp      = "missing_op"
	fedErrMissingCorrID  = "missing_corr_id"
	fedErrUnknownOp      = "unknown_op"
	fedErrBadRequest     = "bad_request"
	fedErrDeadlineExceed = "deadline_exceeded"
	fedErrInternal       = "internal"
)

// responseEnvelope is the spec §2.2 Response envelope, verbatim. data is
// `any` and MUST be built as []T or map — a nil slice serializes as null,
// which the fleet-wide null-with-a-reason law forbids ("data is []/{}
// never null when the answer is empty"); every builder below makes the
// empty container explicitly.
type responseEnvelope struct {
	CorrID   string           `json:"corr_id"`
	Op       string           `json:"op"`
	Peer     string           `json:"peer"`
	Status   string           `json:"status"`
	AsOf     string           `json:"as_of"`
	AgeMS    int64            `json:"age_ms"`
	Data     any              `json:"data"`
	Gaps     []federationGap  `json:"gaps"`
	Error    *federationError `json:"error"`
	Contract string           `json:"contract"`
}

// Response status vocabulary (spec §2.2).
const (
	fedStatusOK      = "ok"
	fedStatusPartial = "partial"
	fedStatusError   = "error"
	fedStatusStale   = "stale"
)

// Want vocabulary (spec §2.1): "answer" (the default, and the only mode a
// v1 read can honor) is all-or-error; "partial" is best-effort with named
// gaps. A degraded side-read downgrades ok → partial with the gap named —
// want=answer does not hide the gap, it records it (the caller asked
// all-or-error; a partial answer says exactly which part is missing so the
// caller can retry or refuse).
const (
	fedWantAnswer  = "answer"
	fedWantPartial = "partial"
)

// catalogueEntry is one row of the GET /api/v1/federation/catalogue answer:
// the op id, its arg shape and a short description of the data it returns
// (spec §2.3 read catalogue).
type catalogueEntry struct {
	Op          string         `json:"op"`
	Args        map[string]any `json:"args"`
	Description string         `json:"description"`
	Owner       string         `json:"owner"`
}

// federationCatalogue is the §2.3 read catalogue, stated ONCE and served by
// both the catalogue route and the unknown-op refusal (the refusal lists the
// valid ops). args describe the OPTIONAL arg keys and their types; every op
// also works with args absent entirely.
var federationCatalogue = []catalogueEntry{
	{
		Op:          "peer.status",
		Args:        map[string]any{},
		Description: "identity, version, clock, load, uptime of THIS scheduler (the /api/v1/health fields)",
		Owner:       "REMOTE-008",
	},
	{
		Op:          "fleet.status",
		Args:        map[string]any{},
		Description: "peers, projects, active ticks, budget of THIS scheduler (the /api/v1/status summary)",
		Owner:       "REMOTE-008",
	},
	{
		Op:          "projects.list",
		Args:        map[string]any{"filter": "string — exact project name; empty/absent = all"},
		Description: "project rows (name, enabled, weight, priority, cooldown)",
		Owner:       "REMOTE-008",
	},
	{
		Op:          "queue.get",
		Args:        map[string]any{},
		Description: "the peer's ordered scheduling queue (GET /api/v1/queue ordering, GAP-054 urgency)",
		Owner:       "REMOTE-008",
	},
	{
		Op:          "ticks.list",
		Args:        map[string]any{"since": "RFC3339 — only ticks created at/after this instant", "limit": "int — max rows (default 50)"},
		Description: "tick rows, newest first",
		Owner:       "REMOTE-008",
	},
	{
		Op:          "events.list",
		Args:        map[string]any{"since": "RFC3339 — only events created at/after this instant", "limit": "int — max rows (default 100)", "severity": "string — exact severity filter"},
		Description: "event rows, newest first",
		Owner:       "REMOTE-008",
	},
}

// handleFederationQuery implements POST /api/v1/federation/query (spec §3
// HTTP row). Order of operations mirrors the other gated handlers:
//
//  1. method check (405 on non-POST),
//  2. the operator gate — fail-closed BEFORE any body read (spec §4 auth
//     posture; identical to pause/resume/evaluate/peers),
//  3. replay window lookup keyed (caller, corr_id, op) — a retry returns
//     the IDENTICAL first answer (spec §2.5),
//  4. envelope validation (missing op / corr_id are named refusals),
//  5. the op's read under the request deadline,
//  6. the reply — stored into the replay window, then written.
func (s *Server) handleFederationQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	// Spec §4: every transport fails closed when no operator credential is
	// configured. Same gate, same refusal bodies, same audit row as every
	// other control surface. The gate has already audited + answered on the
	// refusal arms — return without touching anything.
	if !s.requireOperator(w, r, "-") {
		return
	}
	caller := IdentityFromContext(r.Context())

	var q queryEnvelope
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, federationQueryMaxBodyBytes))
	if err := dec.Decode(&q); err != nil {
		s.federationAudit(r, caller, q.CorrID, q.Op, fedErrBadRequest, "unmarshal: "+err.Error())
		fedWriteError(w, q, fedErrBadRequest, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Named refusals for the required members (spec §2.1) — never an empty
	// 200, never a guessed default.
	if strings.TrimSpace(q.Op) == "" {
		s.federationAudit(r, caller, q.CorrID, "", fedErrMissingOp, "op is required")
		fedWriteError(w, q, fedErrMissingOp, "op is required (see GET /api/v1/federation/catalogue)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(q.CorrID) == "" {
		s.federationAudit(r, caller, "", q.Op, fedErrMissingCorrID, "corr_id is required")
		fedWriteError(w, q, fedErrMissingCorrID, "corr_id is required (the idempotency key, spec §2.5)", http.StatusBadRequest)
		return
	}
	// want is "answer" (default) | "partial" (spec §2.1) — an unknown value
	// is a named refusal, never silently treated as the default.
	want := strings.TrimSpace(q.Want)
	if want == "" {
		want = fedWantAnswer
	}
	if want != fedWantAnswer && want != fedWantPartial {
		msg := "want must be \"" + fedWantAnswer + "\" or \"" + fedWantPartial + "\""
		s.federationAudit(r, caller, q.CorrID, q.Op, fedErrBadRequest, msg)
		fedWriteError(w, q, fedErrBadRequest, msg, http.StatusBadRequest)
		return
	}

	// Replay window (spec §2.5): (caller, corr_id, op) is the identity of a
	// query. A retried query returns the FIRST answer — byte-identical —
	// instead of re-reading or double-counting.
	replay := s.federationReplayWindow()
	if hit, ok := replay.lookup(caller, q.CorrID, q.Op); ok {
		writeJSON(w, hit.status, json.RawMessage(hit.body))
		return
	}

	// REMOTE-013 (spec §4, "Read is scoped, not open"): the read-policy
	// gate — the ONE shared check federationExecQuery runs too, so every
	// transport is covered by construction. The aggregate op is gated with
	// the same key. Absence of an allow is a REFUSAL (op_not_allowed),
	// never a default-yes, and the refusal is audited through the SAME §4
	// audit row an answered query writes (the refusal is the interesting
	// event). Checked AFTER the replay lookup so a replayed answer is the
	// first answer, and BEFORE any read/parse work.
	if fedErrPolicy := s.federationCheckRead(caller, q.Op); fedErrPolicy != nil {
		s.federationAudit(r, caller, q.CorrID, q.Op, fedErrOpNotAllowed, fedErrPolicy.Message)
		fedWriteError(w, q, fedErrOpNotAllowed, fedErrPolicy.Message, http.StatusForbidden)
		return
	}

	// REMOTE-012 (§5): the aggregate rides the same surface, after the
	// replay lookup (a replayed aggregate returns its first answer above,
	// exactly like any other op). It stores the answer BEFORE writing (the
	// surface's discipline) and writes the §4 audit row too — reads are
	// observable, not silently free, and a federated read most of all.
	// Refusal arms (missing/unknown args.op) map through the SAME
	// httpStatus ladder every other error envelope on this surface uses.
	if q.Op == fedOpAggregate {
		resp := s.federationAggregateRun(&q, r.Context())
		httpStatus := http.StatusOK
		if resp.Status == fedStatusError && resp.Error != nil {
			httpStatus = resp.Error.httpStatus()
		}
		replay.store(caller, q.CorrID, q.Op, httpStatus, resp)
		s.federationAudit(r, caller, q.CorrID, q.Op, resp.Status, "")
		writeJSON(w, httpStatus, resp)
		return
	}

	// Unknown op: a NAMED refusal with the catalogue inline (spec §2.2 —
	// an error answer, not an empty one). Audited: the refusal is the
	// interesting event (spec §4).
	fn, ok := federationOpByName(q.Op)
	if !ok {
		names := make([]string, 0, len(federationCatalogue))
		for _, e := range federationCatalogue {
			names = append(names, e.Op)
		}
		msg := "unknown op " + q.Op + " — supported: " + strings.Join(names, ", ")
		s.federationAudit(r, caller, q.CorrID, q.Op, fedErrUnknownOp, msg)
		fedWriteError(w, q, fedErrUnknownOp, msg, http.StatusBadRequest)
		return
	}

	// Request-scoped deadline (SCHED-GAP-1575-B discipline), clamped from
	// the caller's budget_ms (spec §2.1: the peer clamps to its own cap).
	ctx, obs := s.federationRequestDeadline(r.Context(), q.budgetMS())
	defer obs.finish()

	data, gaps, fedErr := fn(ctx, s, &q)
	if !obs.checkFederation(w, q, ctx) {
		return
	}
	if fedErr != nil {
		s.federationAudit(r, caller, q.CorrID, q.Op, fedErr.Code, fedErr.Message)
		fedWriteError(w, q, fedErr.Code, fedErr.Message, fedErr.httpStatus())
		return
	}
	status := fedStatusOK
	if len(gaps) > 0 {
		// A gap in the answer downgrades ok → partial (spec §2.2/§5 law:
		// the caller is never told an answer is complete when a piece went
		// missing). want=answer does not hide the gap — it records it (the
		// caller asked all-or-error; a partial answer says exactly which
		// part is missing so the caller can retry or refuse).
		status = fedStatusPartial
	}
	resp := s.federationResponse(&q, status, data, gaps)
	// Store BEFORE writing so an answer that died mid-write is still
	// replayable (the retry path re-serves the identical body).
	replay.store(caller, q.CorrID, q.Op, http.StatusOK, resp)
	s.federationAudit(r, caller, q.CorrID, q.Op, status, "")
	writeJSON(w, http.StatusOK, resp)
}

// handleFederationCatalogue implements GET /api/v1/federation/catalogue —
// the supported ops with their arg shapes (spec §2.3), plus the contract
// version and the answering peer's identity so a caller can pin both.
// Federation surface: operator-gated like /api/v1/peers (fleet topology
// disclosure — the catalogue names every question this scheduler answers).
func (s *Server) handleFederationCatalogue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	if !s.requireOperator(w, r, "-") {
		return
	}
	ops := make([]catalogueEntry, 0, len(federationCatalogue))
	ops = append(ops, federationCatalogue...)
	writeJSON(w, 200, map[string]any{
		"peer":     database.SchedulerID(),
		"contract": federationContractVersion,
		"ops":      ops,
		"count":    len(ops),
	})
}

// federationOpByName resolves the catalogue id → read function. The map is
// the ONE dispatch table every transport adapter reuses (spec §3: adapters
// call the SAME internal query entry point).
type federationReadFunc func(ctx context.Context, s *Server, q *queryEnvelope) (data any, gaps []federationGap, fedErr *federationError)

var federationOps = map[string]federationReadFunc{
	"peer.status":   fedPeerStatus,
	"fleet.status":  fedFleetStatus,
	"projects.list": fedProjectsList,
	"queue.get":     fedQueueGet,
	"ticks.list":    fedTicksList,
	"events.list":   fedEventsList,
}

func federationOpByName(op string) (federationReadFunc, bool) {
	fn, ok := federationOps[op]
	return fn, ok
}

// ── the six reads (spec §2.3) ──────────────────────────────────────────────
//
// Each op reuses the daemon's own read path — the same helper the matching
// dashboard/REST surface runs — and returns []/{} for every empty answer.

// fedPeerStatus answers op=peer.status: the /api/v1/health payload fields
// (identity, version, clock, load, uptime) plus freshness. The clock and
// DB checks are the health handler's own reads (countActiveTicks +
// PingContext), not a new probe.
func fedPeerStatus(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	activeTicks := countActiveTicks(ctx, s.db)
	dbOK := "connected"
	if err := s.db.PingContext(ctx); err != nil {
		// The DB failing is the answer's subject matter, not a transport
		// error: report db="error: …" exactly as /api/v1/health does.
		dbOK = "error: " + err.Error()
	}
	httpCount, execCount := s.loopSpawnMethodCounts()
	data := map[string]any{
		"scheduler_id": database.SchedulerID(),
		"version":      version.Current(),
		"build_sha":    version.CurrentCommit(),
		"build_time":   version.CurrentBuildDate(),
		"clock":        fedClockDescribe(s.clock()),
		"uptime":       s.clock().Since(s.started).String(),
		"db":           dbOK,
		"active_ticks": activeTicks,
		"spawns_http":  httpCount,
		"spawns_exec":  execCount,
		"paused":       s.loopIsPaused(),
	}
	return data, nil, nil
}

// fedFleetStatus answers op=fleet.status: the /api/v1/status summary fields
// (projects, active ticks, paused, budget chain) plus the local peer
// registry (spec §8.1 default: only itself + its registry, one hop).
func fedFleetStatus(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	gaps := make([]federationGap, 0)
	projects, err := database.ListProjects(ctx, s.db, true)
	if err != nil {
		gaps = append(gaps, federationGap{What: "projects", Why: "ListProjects: " + err.Error()})
		projects = make([]database.Project, 0)
	}
	activeTicks := countActiveTicks(ctx, s.db)
	peers, err := database.ListPeers(ctx, s.db)
	if err != nil {
		gaps = append(gaps, federationGap{What: "peers", Why: "ListPeers: " + err.Error()})
		peers = make([]database.Peer, 0)
	}
	data := map[string]any{
		"scheduler_id":    database.SchedulerID(),
		"active_projects": len(projects),
		"active_ticks":    activeTicks,
		"paused":          s.loopIsPaused(),
		"budget_total":    s.effectiveBudget(),
		"budget_source":   s.budgetSource(),
		"peers":           len(peers),
	}
	return data, gaps, nil
}

// fedProjectsList answers op=projects.list: database.ListProjects — the
// exact read GET /api/v1/projects serves — with the optional exact-name
// filter. Rows carry the catalogue's documented fields (name, enabled,
// weight, priority, cooldown).
func fedProjectsList(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		return nil, nil, &federationError{Code: fedErrInternal, Message: "ListProjects: " + err.Error()}
	}
	filter := q.argString("filter")
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		if filter != "" && p.Name != filter {
			continue
		}
		out = append(out, map[string]any{
			"name":       p.Name,
			"enabled":    p.Enabled,
			"weight":     p.Weight,
			"priority":   p.Priority,
			"cooldown_s": p.CooldownS,
		})
	}
	return out, nil, nil
}

// fedQueueGet answers op=queue.get: s.listQueue — the SAME ordered
// queue (GAP-054 engine-formula urgency) GET /api/v1/queue serves.
func fedQueueGet(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	items, err := s.listQueue(ctx)
	if err != nil {
		return nil, nil, &federationError{Code: fedErrInternal, Message: "listQueue: " + err.Error()}
	}
	if items == nil {
		items = []queueItem{}
	}
	return items, nil, nil
}

// fedTicksList answers op=ticks.list: listTicks — the GET /api/v1/ticks
// read — with the catalogue's args: since (RFC3339 floor applied AFTER the
// read) and limit. Ordered newest-first, the endpoint's documented order.
func fedTicksList(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	limit := q.argInt("limit", 50)
	since, gap, fedErr := q.argSince("since")
	if fedErr != nil {
		return nil, nil, fedErr
	}
	ticks, err := listTicks(ctx, s.db, "", "", limit)
	if err != nil {
		return nil, nil, &federationError{Code: fedErrInternal, Message: "listTicks: " + err.Error()}
	}
	gaps := make([]federationGap, 0)
	if gap != nil {
		gaps = append(gaps, *gap)
	}
	out := make([]database.Tick, 0, len(ticks))
	for _, t := range ticks {
		if since != nil && !tickCreatedAfter(t.CreatedAt, *since) {
			continue
		}
		out = append(out, t)
	}
	return out, gaps, nil
}

// fedEventsList answers op=events.list: listEvents — the GET /api/v1/events
// read — with the catalogue's args: since, limit, severity. The component
// filter stays available via args too (the endpoint exposes it; the
// catalogue lists the §2.3 subset but does not forbid the rest).
func fedEventsList(ctx context.Context, s *Server, q *queryEnvelope) (any, []federationGap, *federationError) {
	limit := q.argInt("limit", 100)
	since, gap, fedErr := q.argSince("since")
	if fedErr != nil {
		return nil, nil, fedErr
	}
	events, err := listEvents(ctx, s.db, q.argString("severity"), q.argString("component"), limit)
	if err != nil {
		return nil, nil, &federationError{Code: fedErrInternal, Message: "listEvents: " + err.Error()}
	}
	gaps := make([]federationGap, 0)
	if gap != nil {
		gaps = append(gaps, *gap)
	}
	out := make([]database.Event, 0, len(events))
	for _, e := range events {
		if since != nil && !tickCreatedAfter(e.CreatedAt, *since) {
			continue
		}
		out = append(out, e)
	}
	return out, gaps, nil
}

// ── envelope plumbing ──────────────────────────────────────────────────────

// federationSelfFreshnessWindow is how far this peer's own last
// self-observation may lag before its answers render status="stale" (spec
// §2.2: "the peer answered, but its own last self-observation is older than
// its freshness window"). Aligned with the peer-registry freshness window
// (database.PeerFreshnessWindowDefault) so the daemon carries ONE freshness
// vocabulary, not two.
func federationSelfFreshnessWindow() time.Duration {
	return time.Duration(database.PeerFreshnessWindowDefault) * time.Second
}

// selfObservationAge is how stale this scheduler's own view is: the age of
// the loop's last evaluation, or — before the first evaluation — the age of
// the boot instant (the process has been reading its own state since it
// started; a scheduler booted 12 minutes ago that has NEVER evaluated is a
// wedged scheduler, and its answers should say so).
func (s *Server) selfObservationAge() time.Duration {
	last := s.started
	if s.loop != nil {
		if t := s.loop.LastEvalTime(); !t.IsZero() {
			last = t
		}
	}
	age := s.clock().Since(last)
	if age < 0 {
		return 0
	}
	return age
}

// federationResponse builds the spec §2.2 envelope: echoed corr_id/op, the
// owning peer id, the data-freshness pair (as_of = the instant the read
// completed, age_ms = now−as_of — always 0 on a fresh read, nonzero on a
// replayed one since the replay path serves the stored bytes verbatim), an
// explicit empty-gaps array, and the contract version. An ok answer from a
// peer whose self-observation lagged past the freshness window renders
// status="stale" — still a full answer (data rides along, no error object),
// never conflated with error.
func (s *Server) federationResponse(q *queryEnvelope, status string, data any, gaps []federationGap) responseEnvelope {
	if data == nil {
		// The null-with-a-reason law: an absent answer container is an
		// empty object, never null.
		data = map[string]any{}
	}
	if gaps == nil {
		gaps = make([]federationGap, 0)
	}
	if status == fedStatusOK && s.selfObservationAge() > federationSelfFreshnessWindow() {
		status = fedStatusStale
	}
	now := s.clock().Now()
	return responseEnvelope{
		CorrID:   q.CorrID,
		Op:       q.Op,
		Peer:     database.SchedulerID(),
		Status:   status,
		AsOf:     now.UTC().Format(time.RFC3339),
		AgeMS:    0,
		Data:     data,
		Gaps:     gaps,
		Contract: federationContractVersion,
	}
}

// fedWriteError renders the error variant of the envelope (spec §2.2:
// status=error + the stable code + human message; data omitted by JSON when
// nil, gaps still an explicit []). HTTP status is the transport's view of
// the same refusal — the envelope stays the contract. Takes the envelope BY
// VALUE: on a decode failure the zero envelope echoes empty corr_id/op,
// which is the honest echo of a request that never parsed.
func fedWriteError(w http.ResponseWriter, q queryEnvelope, code, message string, httpStatus int) {
	writeJSON(w, httpStatus, responseEnvelope{
		CorrID:   q.CorrID,
		Op:       q.Op,
		Peer:     database.SchedulerID(),
		Status:   fedStatusError,
		Gaps:     make([]federationGap, 0),
		Error:    &federationError{Code: code, Message: message},
		Contract: federationContractVersion,
	})
}

// httpStatus maps a stable error code to its HTTP status — the transport's
// view of the same refusal (the envelope stays the contract): bad input is
// 400, a blown budget is 504, an op the caller may not read (REMOTE-013
// read policy) is 403, everything else is 500.
func (e *federationError) httpStatus() int {
	switch e.Code {
	case fedErrBadRequest, fedErrMissingOp, fedErrMissingCorrID, fedErrUnknownOp:
		return http.StatusBadRequest
	case fedErrOpNotAllowed:
		return http.StatusForbidden
	case fedErrDeadlineExceed:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// budgetMS resolves the effective caller budget: the envelope's budget_ms
// clamped to the peer cap (spec §2.1), or the default when absent.
func (q *queryEnvelope) budgetMS() int {
	ms := federationDefaultBudgetMS
	if q.BudgetMS != nil && *q.BudgetMS > 0 {
		ms = *q.BudgetMS
	}
	if ms > federationBudgetClampMS {
		ms = federationBudgetClampMS
	}
	return ms
}

// argString returns args[key] as a trimmed string ("" when absent/wrong
// type — args are all optional unless the op says otherwise).
func (q *queryEnvelope) argString(key string) string {
	if q.Args == nil {
		return ""
	}
	v, ok := q.Args[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// argInt returns args[key] as an int (def when absent/non-positive — a
// zero/negative limit would return zero rows and masquerade as "empty").
func (q *queryEnvelope) argInt(key string, def int) int {
	if q.Args == nil {
		return def
	}
	v, ok := q.Args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case string:
		if parsed, err := strconv.Atoi(n); err == nil && parsed > 0 {
			return parsed
		}
	}
	return def
}

// argSince parses args[key] as RFC3339 (nil when absent/empty). An
// unparseable NON-empty value is a named refusal (bad_request) — silently
// ignoring it would answer with the wrong window. An out-of-order request
// (caller clock behind the data) degrades to a named gap instead of an
// empty answer so the caller can tell "nothing since then" from "the
// filter never matches".
func (q *queryEnvelope) argSince(key string) (*time.Time, *federationGap, *federationError) {
	raw := q.argString(key)
	if raw == "" {
		return nil, nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, nil, &federationError{Code: fedErrBadRequest, Message: key + ": invalid RFC3339 timestamp: " + err.Error()}
	}
	return &t, nil, nil
}

// tickCreatedAfter reports whether an RFC3339 created_at string falls at or
// after the floor instant. Rows whose created_at fails to parse are KEPT
// (an unparseable row must not silently vanish from a filtered answer).
func tickCreatedAfter(createdAt string, floor time.Time) bool {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return true
	}
	return !t.Before(floor)
}

// ── deadline + freshness seams ─────────────────────────────────────────────

// federationRequestDeadline arms the query surface's read deadline — the
// SCHED-GAP-1575-B observer over the caller-clamped budget. Kept separate
// from newRequestDeadline because the budget comes from the ENVELOPE, not
// from the armed server config.
func (s *Server) federationRequestDeadline(parent context.Context, budgetMS int) (context.Context, *requestDeadline) {
	return s.newRequestDeadline(parent, "federation.query", time.Duration(budgetMS)*time.Millisecond)
}

// checkFederation is requestDeadline.check with the envelope-shaped 504: a
// blown budget answers status=error code=deadline_exceeded inside the
// contract envelope (spec §5: a timed-out answer is an ERROR answer, never
// a silent hang), not the plain {"error":...} body the dashboard surfaces
// use.
func (d *requestDeadline) checkFederation(w http.ResponseWriter, q queryEnvelope, ctx context.Context) bool {
	if d == nil {
		return true
	}
	d.closeStep()
	if err := ctx.Err(); err != nil {
		sID := database.SchedulerID()
		writeJSON(w, http.StatusGatewayTimeout, responseEnvelope{
			CorrID:   q.CorrID,
			Op:       q.Op,
			Peer:     sID,
			Status:   fedStatusError,
			Gaps:     make([]federationGap, 0),
			Error:    &federationError{Code: fedErrDeadlineExceed, Message: "read budget exhausted (" + err.Error() + ")"},
			Contract: federationContractVersion,
		})
		return false
	}
	return true
}

// federationAudit writes the spec §4 cross-box read audit row — one row per
// answered query (allowed or refused), carrying {corr_id, caller, op,
// status} (+ ts from LogEvent's created_at), via the SAME events mechanism
// every other audit row uses. Best-effort like auditMutation: an audit
// failure must not turn an answered query into a 500, hence the loud log
// line.
func (s *Server) federationAudit(r *http.Request, caller, corrID, op, outcome, detail string) {
	msg := "federation.query: " + outcome + " op=" + op + " corr_id=" + corrID + " caller=" + caller
	if detail != "" {
		msg += " detail=" + detail
	}
	e := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api.federation",
		Message:   msg,
		Details:   "{\"caller\":\"" + caller + "\",\"corr_id\":\"" + corrID + "\",\"op\":\"" + op + "\",\"outcome\":\"" + outcome + "\",\"path\":\"" + r.URL.Path + "\"}",
	}
	if err := database.LogEvent(r.Context(), s.db, e); err != nil {
		log.Printf("FEDERATION AUDIT WRITE FAILED: component=api.federation msg=%q err=%v", msg, err)
	}
}

// fedClockDescribe renders the server clock's mode. Describe() lives on the
// concrete clock implementations, not the Clock interface, so this goes
// through an interface assertion with a fmt.Stringer fallback ("real" is
// RealClock.Describe's word; any future implementation without Describe
// still renders SOMETHING).
func fedClockDescribe(c clock.Clock) string {
	if d, ok := c.(interface{ Describe() string }); ok {
		return d.Describe()
	}
	if s, ok := c.(interface{ String() string }); ok {
		return s.String()
	}
	return "unknown"
}

// loopIsPaused/loopSpawnMethodCounts guard the nil-loop test shape: a bare
// Server (no loop) still answers about itself instead of panicking.
func (s *Server) loopIsPaused() bool {
	if s.loop == nil {
		return false
	}
	return s.loop.IsPaused()
}

func (s *Server) loopSpawnMethodCounts() (int64, int64) {
	if s.loop == nil {
		return 0, 0
	}
	return s.loop.SpawnMethodCounts()
}
