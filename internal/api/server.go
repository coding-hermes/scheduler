package api

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/version"
)

// Server is the HTTP API server for the fleet scheduler.
type Server struct {
	// clk is the server's time seam (SCHED-GAP-169). NewServer seeds it from
	// the loop's clock so every timestamp surface shares one timeline; the
	// zero value reads as the wall clock.
	clk     clock.Seam
	db      *sql.DB
	loop    *scheduler.Loop
	started time.Time

	// failureWindow is the number of recent ticks (per project) over which
	// the /api/v1/status per-project failure-rate breakdown is computed.
	// Zero or negative = default of 100.
	failureWindow int

	// duckbrainHealth, when set, is called to include DuckBrain sync health
	// in /api/v1/status. Kept as a func so the API package doesn't import
	// the sync package (no dependency cycle; nil = feature off).
	duckbrainHealth func() map[string]interface{}

	// resolvedConfig is the startup-time snapshot of the active
	// three-layer config served by GET /api/v1/config (SCHED-GAP-034).
	// Populated by main.go via SetResolvedConfig after TOML/env resolution.
	resolvedConfig ResolvedConfig

	// urgencyCalc computes engine-formula urgency scores for
	// GET /api/v1/queue (GAP-054). Built from the resolved interval range
	// (MinInterval/MaxInterval/NumLevels) by SetResolvedConfig; nil when
	// unconfigured or the range is unparseable — listQueue falls back to
	// priority-only scores in that case (tests construct the Server without
	// SetResolvedConfig).
	urgencyCalc *scheduler.UrgencyCalculator

	// blocksStore is the JSONL-backed deploy groups/templates store
	// (internal/blocks). Installed by main.go via SetBlocksStore; when nil
	// the /api/v1/groups* and /api/v1/templates* endpoints answer 503
	// (store not configured) instead of panicking.
	blocksStore *blocks.Store
}

// NewServer creates an API server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	s := &Server{
		db:            db,
		loop:          loop,
		failureWindow: 100, // default; override via SetFailureWindow
	}
	// Share the loop's clock (SCHED-GAP-169): uptime and every "age" surface
	// then measure on the same timeline as scheduling. A nil loop (unit tests)
	// keeps the wall clock.
	if loop != nil {
		s.SetClock(loop.Clock())
	} else {
		s.started = clock.Real().Now()
	}
	return s
}

// SetClock installs the clock this server reads time through (SCHED-GAP-169).
// nil keeps the current clock. Installing a clock re-anchors the uptime origin
// at that clock's instant, so a simulated run reports simulated uptime.
func (s *Server) SetClock(c clock.Clock) {
	if c == nil {
		return
	}
	s.clk.Set(c)
	s.started = c.Now()
}

// clock returns the server's clock, never nil.
func (s *Server) clock() clock.Clock { return s.clk.Get() }

// SetFailureWindow sets the number of recent ticks per project used for the
// /api/v1/status per-project failure-rate breakdown (SCHED-GAP-018).
func (s *Server) SetFailureWindow(n int) {
	if n > 0 {
		s.failureWindow = n
	}
}

// SetDuckBrainHealth registers a provider for DuckBrain sync health so the
// status endpoint can surface fallback state (reachable, spool depth, etc).
func (s *Server) SetDuckBrainHealth(fn func() map[string]interface{}) {
	s.duckbrainHealth = fn
}

// SetBlocksStore installs the JSONL-backed deploy groups/templates store
// served by the /api/v1/groups* and /api/v1/templates* endpoints (including
// the template deploy action). main.go resolves the store paths (--db dir by
// default, --groups-file/--templates-file or [scheduler] TOML overrides) and
// calls this before the HTTP server starts. A Server without a store answers
// 503 on those routes.
func (s *Server) SetBlocksStore(st *blocks.Store) {
	s.blocksStore = st
}

// Handler returns an http.Handler for all API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.health)
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/config", s.config)
	mux.HandleFunc("/api/v1/projects", s.handleProjects)
	mux.HandleFunc("/api/v1/projects/", s.handleProjectByID)
	mux.HandleFunc("/api/v1/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/v1/namespaces/", s.handleNamespaceByID)
	// JSONL-backed deploy groups + templates (internal/blocks).
	mux.HandleFunc("/api/v1/groups", s.handleGroups)
	mux.HandleFunc("/api/v1/groups/", s.handleGroupByID)
	mux.HandleFunc("/api/v1/templates", s.handleTemplates)
	mux.HandleFunc("/api/v1/templates/", s.handleTemplateByID)
	mux.HandleFunc("/api/v1/ticks", s.handleTicks)
	mux.HandleFunc("/api/v1/ticks/", s.handleTickByID)
	mux.HandleFunc("/api/v1/evaluate", s.evaluate)
	mux.HandleFunc("/api/v1/pause", s.pause)
	mux.HandleFunc("/api/v1/resume", s.resume)
	mux.HandleFunc("/api/v1/events", s.events)
	// CTL-002: the live push counterpart of the poll-only event log above —
	// one SSE connection instead of a client poll loop.
	mux.HandleFunc("/api/v1/events/stream", s.eventsStream)
	mux.HandleFunc("/api/v1/queue", s.queue)
	mux.HandleFunc("/api/v1/openapi.json", s.openapi)
	// SCHED-GAP-156: the single read-only fleet-metrics endpoint (one request
	// answers spawns/deferrals/nudges/ticks/durations/drains/outcomes).
	mux.HandleFunc("/api/v1/metrics", s.metrics)
	return mux
}

// health returns server health status.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	activeTicks := countActiveTicks(ctx, s.db)
	dbOK := "connected"
	if err := s.db.PingContext(ctx); err != nil {
		dbOK = "error: " + err.Error()
	}
	lastEval := s.loop.LastEvalTime()
	// last_evaluation is RFC3339. Zero time serializes as "0001-01-01T00:00:00Z"
	// when the loop has never evaluated yet — callers can compare against
	// evaluation_age_seconds (which is 0 in that case) instead.
	var lastEvalStr string
	var evalAge float64
	if lastEval.IsZero() {
		lastEvalStr = ""
		evalAge = 0
	} else {
		lastEvalStr = lastEval.UTC().Format(time.RFC3339)
		evalAge = s.clock().Since(lastEval).Seconds()
	}
	httpCount, execCount := s.loop.SpawnMethodCounts()
	writeJSON(w, 200, map[string]interface{}{
		"status": "ok",
		// SCHED-GAP-148: version + build identity. The human-facing version
		// string stays first in source order as the operator summary;
		// build_sha/build_time are the machine-comparable fields (the
		// freshness guard and any restart script read build_sha). Same
		// resolution seam as version: ldflags injection wins, then vcs
		// build info, then "unknown".
		"version":                version.Current(),
		"build_sha":              version.CurrentCommit(),
		"build_time":             version.CurrentBuildDate(),
		"uptime":                 s.clock().Since(s.started).String(),
		"db":                     dbOK,
		"active_ticks":           activeTicks,
		"last_evaluation":        lastEvalStr,
		"evaluation_age_seconds": evalAge,
		"spawns_http":            httpCount,
		"spawns_exec":            execCount,
		// SCHED-GAP-080: transient gateway spawn failures since restart
		// (auth rejections never counted), alongside the spawn counters.
		"gateway_errors": s.loop.GatewayErrorCount(),
	})
}

// status returns fleet overview.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, s.db, true)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// GAP-047: auto-disable policy comes from the startup resolved-config
	// snapshot. A Server built without SetResolvedConfig (tests) carries the
	// zero value → threshold == 0 → feature off, no panic.
	adThreshold := s.resolvedConfig.AutoDisableFailureRate
	adMinTicks := s.resolvedConfig.AutoDisableMinTicks
	activeTicks := countActiveTicks(ctx, s.db)
	recentOutcomes := countRecentOutcomes(ctx, s.db)
	failureRates := computeProjectFailureRates(ctx, s.db, s.failureWindow, adThreshold, adMinTicks)
	// PERF-001: serve last_evaluation from the loop's in-memory state when a
	// loop is attached. evaluate() sets lastEval immediately BEFORE emitting
	// the 'evaluation started' event (internal/scheduler/tick_process.go), so
	// LastEvalTime() is the exact same timestamp the events-table query would
	// return — zero cost instead of a full scan of the events table (no index
	// on message; measured ~43ms on 254k rows). Serialization matches the
	// health handler (RFC3339 UTC, zero time → empty string). getLastEvalTime
	// remains the no-loop fallback (used by tests).
	var lastEval string
	if s.loop != nil {
		if t := s.loop.LastEvalTime(); !t.IsZero() {
			lastEval = t.UTC().Format(time.RFC3339)
		}
	} else {
		lastEval = getLastEvalTime(ctx, s.db)
	}
	// ADV-R09/G8: the effective budget from the budget authority chain —
	// the Loop the resolved --budget/SCHEDULER_BUDGET/TOML value built.
	budgetTotal := s.effectiveBudget()
	status := map[string]interface{}{
		// SCHED-GAP-148: build identity on the fleet-overview endpoint too.
		// version is the operator-facing summary; build_sha is the field the
		// freshness guard (ops/check-daemon-freshness.sh) compares against
		// the newest commit touching admission/scheduling code, and
		// build_time is the RFC3339 build stamp. All three come from
		// internal/version — never a literal.
		"version":                version.Current(),
		"build_sha":              version.CurrentCommit(),
		"build_time":             version.CurrentBuildDate(),
		"active_projects":        len(projects),
		"active_ticks":           activeTicks,
		"paused":                 s.loop != nil && s.loop.IsPaused(),
		"recent_outcomes":        recentOutcomes,
		"projects_failure_rates": failureRates,
		"failure_window":         s.failureWindow,
		"last_evaluation":        lastEval,
		// ADV-R09/G8 — budget authority chain. budget_total was a literal
		// 100 at this spot (coincidentally equal to the --budget flag
		// DEFAULT, which is what a hardcoded surface pretends to report);
		// it now flows from the loop the flag/env/TOML resolution built.
		// The full chain: TOML [scheduler] weight_budget <
		// SCHEDULER_BUDGET env < --budget flag, resolved in main.go, single
		// source = the Loop. budget_source records the layer that owned the
		// effective value (main.go passes it via SetResolvedConfig;
		// "flag-default:100" means NO layer configured it — the documented
		// unset behavior is "fall back to the --budget default of 100
		// weight units", NOT a money budget: weight units are scheduling
		// admission currency, per-project USD caps live in
		// daily/weekly/final_budget_usd).
		"budget_total":  budgetTotal,
		"budget_source": s.budgetSource(),
		"auto_disable": map[string]interface{}{
			"enabled":   adThreshold > 0,
			"threshold": adThreshold,
			"window":    s.failureWindow,
			"min_ticks": adMinTicks,
		},
	}
	// ADV-R09/G8 — spend reality block: what "recorded spend" actually is.
	// Splits ticks.cost_usd by cost_source so measured/gateway money is
	// never silently blended with the estimate tier, and states the price
	// vintage the USD figures were priced at (as-of + provenance of the
	// sticker maps). One GROUP BY, no per-project loop.
	status["spend"] = s.spendByCostSource(ctx)
	// SCHED-GAP-107: active bump badge + remaining count per project.
	status["bumps"] = listActiveBumps(projects)
	// SCHED-GAP-112 / S12 §9.4: live wave load. ONE indexed query over
	// running ticks (no per-project loop, <2ms budget per S12 §14) —
	// workers NEVER enter active_ticks (W1/W3).
	waves := s.listRunningWaves(ctx)
	status["wave_depth_total"] = waveDepthTotal(waves)
	status["wave_workers_cap_configured"] = waveWorkersCapConfigured(ctx, s.db)
	status["waves"] = waves
	// SCHED-GAP-115 (S12 §11): attributed wave cost — the cost twin of
	// wave_depth_total. Sum of tick_workers.cost_usd over running waves;
	// attribution-only figures (W4), so this is what the replica holds,
	// not an additive fleet total.
	status["wave_cost_total"] = s.runningWaveCostTotal(ctx)
	// GAP-043: zero-select diagnostics — consecutive zero-select evals with
	// eligible projects present, and the eligible count at the last one.
	if s.loop != nil {
		zsCount, zsEligible, zsLast := s.loop.ZeroSelectStats()
		status["zero_select_consecutive"] = zsCount
		status["zero_select_eligible"] = zsEligible
		status["zero_select_last_at"] = zsLast
		// SCHED-GAP-080: transient gateway spawn failures since restart
		// (auth rejections never counted), alongside the health endpoint's
		// spawns_http/spawns_exec counters.
		status["gateway_errors"] = s.loop.GatewayErrorCount()
		// SCHED-GAP-117: the armed per-turn gateway POST deadline (0 =
		// disabled; a stalled POST fails the tick as "stalled" before
		// --tick-timeout).
		status["gateway_response_timeout"] = s.loop.GatewayResponseTimeout().String()
		// SCHED-GAP-155: per-reason admission-decision counters, so an
		// operator can see WHY projects are not spawning without grepping
		// the scheduler log. Every reason in the vocabulary is present
		// (0 = never seen this process), plus "admitted:<namespace>"
		// totals and "passes". Counterpart of the `ADMIT ` log lines.
		status["admission_counters"] = s.loop.AdmissionCounters()
	}
	if s.duckbrainHealth != nil {
		status["duckbrain"] = s.duckbrainHealth()
	}
	// SCHED-GAP-170 (observability): the gateway-health admission gate's armed
	// state and cached verdict. Deliberately OUTSIDE the loop guard above: the
	// gate is package state, and the question this block answers — "is the gate
	// even armed?" — must be answerable on a daemon whose loop never got a
	// gateway client, which is exactly the shape that was silently ungated
	// before.
	status["gateway_health_gate"] = gatewayHealthGateStatusBlock()
	writeJSON(w, 200, status)
}

// evaluate triggers a forced evaluation cycle.
func (s *Server) evaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.ForceEvaluate()
	writeJSON(w, 200, map[string]string{"status": "evaluation triggered"})
}

// activeBump is one entry in the /api/v1/status "bumps" array (SCHED-GAP-107).
type activeBump struct {
	Project        string `json:"project"`
	RemainingTicks int    `json:"remaining_ticks"`
	CooldownS      int    `json:"cooldown_s"`
	Reason         string `json:"reason"`
	StartedAt      string `json:"started_at"`
}

// listActiveBumps extracts the active-bump view from a project list. An
// empty (non-nil) slice is returned when no project is bumped so the status
// JSON carries "bumps": [] rather than null.
func listActiveBumps(projects []database.Project) []activeBump {
	out := make([]activeBump, 0)
	for _, p := range projects {
		if !p.BumpActive {
			continue
		}
		out = append(out, activeBump{
			Project:        p.Name,
			RemainingTicks: p.BumpRemainingTicks,
			CooldownS:      p.BumpCooldownS,
			Reason:         p.BumpReason,
			StartedAt:      p.BumpStartedAt,
		})
	}
	return out
}

// activeWave is one entry in the /api/v1/status "waves" array (SCHED-GAP-112,
// S12 §9.4) — an in-flight tick dispatching worker sessions. Cost is the
// attributed per-worker sum for this wave so far (SCHED-GAP-115, S12 §11):
// 0 until attribution rows exist, attribution-only (W4).
type activeWave struct {
	Project     string  `json:"project"`
	TickID      string  `json:"tick_id"`
	Namespace   string  `json:"namespace"`
	WorkerCount int     `json:"worker_count"`
	Cost        float64 `json:"cost"`
	StartedAt   string  `json:"started_at"`
	AgeS        int64   `json:"age_s"`
}

// listRunningWaves builds the /api/v1/status waves view from the running-wave
// rows (one indexed query). AgeS is seconds since started_at; a started_at
// that fails to parse as RFC3339 yields age 0 rather than an error — the
// status surface must never 500 on a dirty row. An empty (non-nil) slice is
// returned when no wave is running so the JSON carries "waves": [] rather
// than null (the bumps convention).
func (s *Server) listRunningWaves(ctx context.Context) []activeWave {
	rows, err := database.ListRunningWaves(ctx, s.db)
	if err != nil {
		log.Printf("status: list running waves: %v", err)
		return make([]activeWave, 0)
	}
	// SCHED-GAP-115: per-tick attributed cost — one indexed join, shared
	// with runningWaveCostTotal. A failed lookup leaves costs at 0 (the
	// status surface must never 500 on a dirty read).
	costs, _, err := database.RunningWaveCosts(ctx, s.db)
	if err != nil {
		log.Printf("status: running wave costs: %v", err)
		costs = nil
	}
	out := make([]activeWave, 0, len(rows))
	for _, w := range rows {
		var age int64
		if ts, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
			age = int64(s.clock().Since(ts).Seconds())
			if age < 0 {
				age = 0
			}
			// Normalize to RFC3339 UTC, matching the bumps convention
			// (activeBump.StartedAt) and last_evaluation.
			w.StartedAt = ts.UTC().Format(time.RFC3339)
		}
		out = append(out, activeWave{
			Project:     w.Project,
			TickID:      w.TickID,
			Namespace:   w.NamespaceID,
			WorkerCount: w.WorkerCount,
			Cost:        costs[w.TickID],
			StartedAt:   w.StartedAt,
			AgeS:        age,
		})
	}
	return out
}

// runningWaveCostTotal sums the attributed per-worker cost over all running
// waves (SCHED-GAP-115) — /api/v1/status wave_cost_total. A failed query
// returns 0 (attribution is observability; a dirty read must never 500 the
// status surface).
func (s *Server) runningWaveCostTotal(ctx context.Context) float64 {
	_, total, err := database.RunningWaveCosts(ctx, s.db)
	if err != nil {
		log.Printf("status: running wave costs: %v", err)
		return 0
	}
	return total
}

// waveDepthTotal sums worker_count over the running-wave rows — the fleet's
// live worker-process load. It is deliberately NOT active_ticks: a 3-worker
// wave is one tick (W1/W3) but three workers.
func waveDepthTotal(waves []activeWave) int {
	total := 0
	for _, w := range waves {
		total += w.WorkerCount
	}
	return total
}

// waveWorkersCapConfigured reports whether any namespace sets
// wave_workers_cap > 0 (S12 §9.4). DB errors degrade to false, never a 500.
func waveWorkersCapConfigured(ctx context.Context, db *sql.DB) bool {
	ok, err := database.WaveWorkersCapConfigured(ctx, db)
	if err != nil {
		log.Printf("status: wave workers cap configured: %v", err)
		return false
	}
	return ok
}

// pause suspends the scheduler loop.
func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.Pause()
	writeJSON(w, 200, map[string]string{"status": "paused"})
}

// resume unpauses the scheduler loop.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.Resume()
	writeJSON(w, 200, map[string]string{"status": "resumed"})
}
