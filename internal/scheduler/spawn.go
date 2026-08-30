package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Cost estimation constants for real ticks where session export is unavailable.
// These are conservative estimates based on typical foreman tick usage.
const (
	estTokensIn    = 8000     // estimated input tokens per tick
	estTokensOut   = 2000     // estimated output tokens per tick
	estCostPerIn   = 0.000002 // foreman-model input $/token (set via env)
	estCostPerOut  = 0.000008 // foreman-model output $/token (set via env)
	estCostPerTick = float64(estTokensIn)*estCostPerIn + float64(estTokensOut)*estCostPerOut
)

// estimateTickCost returns estimated token counts and cost for a real tick.
// Real session export (hermes sessions export) is a future task; for now we
// use fixed estimates so cost aggregation works from day one.
func estimateTickCost() (tokensIn, tokensOut int, costUSD float64) {
	return estTokensIn, estTokensOut, estCostPerTick
}

// Spawner launches coding-hermes foreman processes.
type Spawner struct {
	db            *sql.DB
	maxConcurrent int
	active        map[string]*exec.Cmd // tickID -> running process
	mu            sync.Mutex
	timeout       time.Duration
	model         string
	provider      string
	// SCHED-GAP-064: global (env) fallback tier for the spawn model/provider
	// chain. Applied AFTER the project's primary and fallback tiers; skipped
	// entirely when a project sets NoGlobalFallback.
	fallbackModel    string
	fallbackProvider string
	// SCHED-GAP-065: idle-lane tiers for ticks on boards with ZERO pending
	// tasks. The project's idle tier (fleet.toml idle_model/idle_provider)
	// is prepended to the regular chain; the env tier below is the global
	// idle lane behind it — gated by NoGlobalFallback like the other
	// spawner-level (env) tiers. Empty = no idle lane (idle ticks resolve
	// exactly like work ticks).
	idleModel    string
	idleProvider string
	// router resolves the spawn model/provider to the task router's
	// cheapest HEALTHY head at spawn time (TASK-ROUTER-001). Wired from
	// SCHEDULER_ROUTER_CMD in NewSpawner; nil (the default in tests and
	// on hosts without the env var) = router disabled, spawns resolve
	// exactly as before the router (fail-open).
	router *RouterClient
	// circuit records (provider, model) spawn outcomes into the shared
	// circuit-breaker state via router_circuit.py (TASK-ROUTER-002).
	// Wired from SCHEDULER_CIRCUIT_CMD in NewSpawner; nil (the default
	// in tests and on hosts without the env var) = recording disabled,
	// spawns behave exactly as before the breaker (fail-open — the
	// circuit script is a side effect, never a gate).
	circuit *CircuitClient
	// pendingCounter reads board pending counts for idle-tick routing
	// (SCHED-GAP-065). Defaults to the package-level shared instance; nil
	// (tests, tooling) biases every spawn to the work chain — the
	// conservative default that never cheap-routes real work.
	pendingCounter *PendingTaskCounter
	skills         string
	foremanHome    string         // HERMES_HOME for foreman config
	gateway        *GatewayClient // HTTP API client (nil = use exec.Command)
	noExecFallback bool           // disable exec.Command fallback on gateway failure

	// events is an optional EventLogger. When set, terminal gateway-key
	// rejections (GAP-035) emit a HIGH event so a key regression is
	// immediately visible instead of producing thousands of silent failed
	// ticks. Nil is safe — tests and tooling construct Spawners standalone.
	events *EventLogger

	// heartbeatInterval is the cadence at which a running tick's row gets its
	// heartbeat_at refreshed (S-GAP-003). The gateway zombie reaper treats a
	// pid=0 row whose heartbeat is stale as orphaned. Tests shrink this.
	heartbeatInterval time.Duration

	// Prometheus-style spawn counters since last restart.
	spawnCountHTTP int64
	spawnCountExec int64

	// spawnGatewayErrors counts transient gateway spawn failures (HTTP 5xx,
	// network/timeout, read/unmarshal) since last restart (SCHED-GAP-080).
	// Auth rejections (401/403, ErrGatewayKeyRejected) are NEVER counted —
	// they are terminal, not transient. Exposed via GatewayErrorCount().
	spawnGatewayErrors int64
}

// NewSpawner creates a spawner with the given concurrency limit and defaults.
func NewSpawner(db *sql.DB, maxConcurrent int, timeout ...time.Duration) *Spawner {
	to := 30 * time.Minute
	if len(timeout) > 0 {
		to = timeout[0]
	}
	return &Spawner{
		db:               db,
		maxConcurrent:    maxConcurrent,
		active:           make(map[string]*exec.Cmd),
		timeout:          to,
		model:            getEnvOrDefault("SCHEDULER_FOREMAN_MODEL", "your-model-name"),
		provider:         getEnvOrDefault("SCHEDULER_FOREMAN_PROVIDER", "your-provider-name"),
		fallbackModel:    getEnvOrDefault("SCHEDULER_FOREMAN_FALLBACK_MODEL", ""),
		fallbackProvider: getEnvOrDefault("SCHEDULER_FOREMAN_FALLBACK_PROVIDER", ""),
		idleModel:        getEnvOrDefault("SCHEDULER_FOREMAN_IDLE_MODEL", ""),
		idleProvider:     getEnvOrDefault("SCHEDULER_FOREMAN_IDLE_PROVIDER", ""),
		// TASK-ROUTER-001: the task router is OPT-IN via env — hosts
		// without SCHEDULER_ROUTER_CMD (and every test) keep the
		// pre-router resolution exactly (fail-open default). The command
		// is a full vector so the binary + flags are overridable; the
		// project and --format json are appended at resolve time.
		router: routerFromEnv(),
		// TASK-ROUTER-002: the circuit recorder is OPT-IN via env —
		// hosts without SCHEDULER_CIRCUIT_CMD (and every test) keep the
		// pre-breaker behavior exactly (fail-open default). Same full
		// command vector contract as the router; the subcommand + pair
		// are appended at record time.
		circuit:           circuitFromEnv(),
		pendingCounter:    defaultPendingCounter,
		skills:            getEnvOrDefault("SCHEDULER_FOREMAN_SKILLS", "coding-hermes-foreman"),
		foremanHome:       os.ExpandEnv("$HOME/.hermes/foreman"),
		heartbeatInterval: 5 * time.Minute,
	}
}

// getEnvOrDefault returns the value of envVar if set, otherwise fallback.
func getEnvOrDefault(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return fallback
}

// routerFromEnv wires the task router from SCHEDULER_ROUTER_CMD
// (TASK-ROUTER-001). The variable is a full shell command line
// (e.g. "/home/kara/.hermes/venvs/board/bin/python3
// /home/kara/.hermes/scripts/router_spawn.py"); it is split on spaces so
// binary + flags stay overridable, and the project + --format json are
// appended at resolve time. Unset/empty = router disabled (fail-open
// default). A command that would not survive the split (e.g. an
// argument containing a space) is treated as disabled rather than
// mis-executed.
func routerFromEnv() *RouterClient {
	cmd := os.Getenv("SCHEDULER_ROUTER_CMD")
	if cmd == "" {
		return nil
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return nil
	}
	return NewRouterClient(parts, 0)
}

// noteSpawnFailure increments the project's consecutive spawn-failure counter,
// which drives the exponential selection backoff (S-GAP-001). Best-effort:
// a DB error here must never mask the real spawn error.
func (s *Spawner) noteSpawnFailure(project string) {
	if _, err := s.db.Exec(
		`UPDATE projects SET consecutive_failures = consecutive_failures + 1 WHERE name = ?`,
		project); err != nil {
		log.Printf("WARN: consecutive_failures increment for %s: %v", project, err)
	}
}

// recordCircuitFailure opens (or extends) the circuit for the pair that
// just failed, via router_circuit.py record-failure (TASK-ROUTER-002). The
// breaker cooldown (5m → double per consecutive failure → 1h cap) becomes
// the cross-tick backoff: router_spawn.py excludes the pair from chain
// heads while open_until is in the future. Fire-and-forget: a missing or
// broken circuit script only logs a WARN — it must NEVER fail or stall a
// spawn. Empty pairs (custom-command spawns) are skipped.
func (s *Spawner) recordCircuitFailure(provider, model, reason string) {
	if s.circuit == nil || !s.circuit.Enabled() {
		return
	}
	if provider == "" || model == "" {
		return
	}
	s.circuit.RecordFailure(context.Background(), provider, model, reason)
}

// recordCircuitSuccess closes the circuit for the pair that just succeeded,
// via router_circuit.py record-success (TASK-ROUTER-002). No-op for pairs
// without recorded failures. Fire-and-forget, same fail-open contract as
// recordCircuitFailure. Empty pairs (custom-command spawns) are skipped.
func (s *Spawner) recordCircuitSuccess(provider, model string) {
	if s.circuit == nil || !s.circuit.Enabled() {
		return
	}
	if provider == "" || model == "" {
		return
	}
	s.circuit.RecordSuccess(context.Background(), provider, model)
}

// gatewayKeyRejected is the terminal classification path for GAP-035: a
// gateway 401/403 on either the pre-dispatch probe or SendResponse. It
// (1) logs a distinct GATEWAY KEY REJECTED line, (2) bumps the selection
// backoff so the broken project is not re-picked into a retry flood, and
// (3) emits an immediate HIGH event so a key regression is visible in the
// events table/dashboard instead of vanishing into thousands of generic
// failed ticks. The returned error wraps ErrGatewayKeyRejected so callers
// can classify it with errors.Is.
func (s *Spawner) gatewayKeyRejected(project, tickID string, err error) error {
	log.Printf("GATEWAY KEY REJECTED: %s tick=%s error=%v — failing fast, no dispatch, no exec fallback", project, tickID, err)
	s.noteSpawnFailure(project)
	if s.events != nil {
		s.events.Emit(context.Background(), SeverityHigh, "spawn", "gateway key rejected", map[string]any{
			"project": project,
			"tick_id": tickID,
			"error":   err.Error(),
		})
	}
	return fmt.Errorf("gateway key rejected for %s: %w", project, err)
}

// SetForemanHome overrides the default HERMES_HOME for foreman sessions.
func (s *Spawner) SetForemanHome(path string) {
	s.foremanHome = path
}

// RunningSet returns the set of project names that currently have a spawned
// process (in-memory). This is more accurate than the DB query because spawns
// haven't been committed to the DB yet when the packer queries.
func (s *Spawner) RunningSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(s.active))
	for tickID := range s.active {
		// Extract project name from tick ID: "project-YYYY-MM-DD-HH-MM-SS"
		idx := strings.LastIndex(tickID, "-202")
		if idx > 0 {
			set[tickID[:idx]] = true
		}
	}
	return set
}

// SetGatewayClient configures the HTTP API client. If set, Spawn() prefers
// HTTP over process spawning. Pass nil to disable and fall back to exec.Command.
func (s *Spawner) SetGatewayClient(client *GatewayClient) {
	s.gateway = client
}

// SetNoExecFallback disables the exec.Command fallback when gateway spawns fail.
func (s *Spawner) SetNoExecFallback(v bool) {
	s.noExecFallback = v
}

// SetEventLogger wires an optional EventLogger for HIGH events on terminal
// gateway-key rejections (GAP-035). Nil (the default) disables emission.
func (s *Spawner) SetEventLogger(el *EventLogger) {
	s.events = el
}

// SetPendingCounter overrides the pending-task counter used for idle-tick
// routing (SCHED-GAP-065). Pass nil to force the work chain for every spawn.
func (s *Spawner) SetPendingCounter(c *PendingTaskCounter) {
	s.pendingCounter = c
}

// SetRouterClient installs the task router (TASK-ROUTER-001). Pass nil to
// disable — spawns then resolve exactly as before the router (fail-open).
// Tests inject fake router scripts here; the daemon wires the real router
// from SCHEDULER_ROUTER_CMD in NewSpawner.
func (s *Spawner) SetRouterClient(rc *RouterClient) {
	s.router = rc
}

// SetCircuitClient installs the circuit-breaker recorder (TASK-ROUTER-002).
// Pass nil to disable — spawns then behave exactly as before the breaker
// (fail-open; the circuit script is a side effect, never a gate). Tests
// inject fake circuit scripts here; the daemon wires the real script from
// SCHEDULER_CIRCUIT_CMD in NewSpawner.
func (s *Spawner) SetCircuitClient(cc *CircuitClient) {
	s.circuit = cc
}

// gatewayKeyProbeTimeout bounds the pre-dispatch per-project key validation
// (GAP-035). The probe is a cheap GET /health — it must never add meaningful
// latency to a spawn, and a slow probe must not stall the tick.
const gatewayKeyProbeTimeout = 5 * time.Second

// SCHED-GAP-080: bounded transient-gateway retry. gatewayRetryMaxAttempts is
// the number of retries AFTER the initial attempt (so a persistently-failing
// gateway sees 1 + gatewayRetryMaxAttempts = 4 POSTs). gatewayRetryBackoff
// returns the exponential backoff for retry attempt k: 500ms → 1s → 2s → 4s
// cap (≈3.5s worst-case added latency, far below the tick timeout). The tick
// context is the outer bound: a ctx cancel anywhere in the loop aborts
// immediately, so a persistently-5xx gateway still fails the tick instead of
// hanging it (SCHED-GAP-080 acceptance: bounded attempts, ctx-bounded).
const gatewayRetryMaxAttempts = 3

// gatewayRetryBackoff returns the backoff duration for the given retry
// attempt (1-based), capped at 4s.
func gatewayRetryBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << (attempt - 1)
	if d > 4*time.Second {
		return 4 * time.Second
	}
	return d
}

// GatewayAvailable returns true if the gateway client is configured and reachable.
func (s *Spawner) GatewayAvailable() bool {
	if s.gateway == nil {
		return false
	}
	return s.gateway.Ping(context.Background()) == nil
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (s *Spawner) SpawnMethodCounts() (httpCount, execCount int64) {
	return atomic.LoadInt64(&s.spawnCountHTTP), atomic.LoadInt64(&s.spawnCountExec)
}

// GatewayErrorCount returns the number of transient gateway spawn failures
// since last restart (SCHED-GAP-080). Auth rejections are never counted.
func (s *Spawner) GatewayErrorCount() int64 {
	return atomic.LoadInt64(&s.spawnGatewayErrors)
}

// ActiveCount returns the number of currently running spawns.
func (s *Spawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// startHeartbeat launches a goroutine that refreshes the tick row's
// heartbeat_at every heartbeatInterval until the returned channel is closed.
// S-GAP-003: heartbeat_at is the liveness signal for pid=0 (gateway) ticks —
// both zombie reapers treat a heartbeat older than gatewayZombieMaxAge as an
// orphaned tick. The goroutine never outlives its request/process: callers
// close the stop channel as soon as the spawn returns. A DB error is logged
// and the goroutine keeps beating (a transient error must not kill liveness).
func (s *Spawner) startHeartbeat(tickID string) chan<- struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			// A stop that raced with this tick wins — never write after the
			// spawn returned. At most one in-flight write can land after
			// close, and the next loop iteration exits immediately.
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.db.Exec(`UPDATE ticks SET heartbeat_at = ? WHERE id = ?`,
				time.Now().Format(time.RFC3339), tickID); err != nil {
				log.Printf("WARN: heartbeat refresh for tick %s: %v", tickID, err)
			}
		}
	}()
	return stop
}

// WorkerDefaults returns a prompt suffix with the project's preferred worker
// model and provider. Empty string when neither is configured. Includes
// fallback instructions so the foreman can switch models freely.
func WorkerDefaults(project PackedProject) string {
	if project.WorkerModel == "" && project.WorkerProvider == "" {
		return ""
	}
	m := project.WorkerModel
	p := project.WorkerProvider
	if m == "" {
		m = "(no default)"
	}
	if p == "" {
		p = "(no default)"
	}
	return fmt.Sprintf(
		"Worker model/provider (AUTHORITATIVE, do not change): use model %s with provider %s. "+
			"The board's model column is an advisory routing suggestion only; the configured worker_model here takes precedence and MUST be used for every worker you dispatch. "+
			"Only switch models if this one errors with an actual unavailable/rate-limited failure — do not second-guess based on the board's recommendation. ",
		m, p,
	)
}

// builtinForemanPrompt is the fallback foreman instruction body used when a
// project's namespace has no default_prompt configured. It is the historical
// hardcoded scheduler prompt with the dynamic bits (workdir, worker
// model/provider) removed — those are always appended by buildForemanPrompt.
// Bane 2026-08-27: the prompt is now DATA — namespaces carry a
// default_prompt in fleet.toml ([[namespaces]]), and projects can append
// their own prompt or replace the default entirely (prompt_mode="replace").
const builtinForemanPrompt = "" +
	"Load skills coding-hermes-map and coding-hermes-foreman at start. " +
	"Use the map to pull additional skills LAZILY as each phase needs them " +
	"(board read, worker dispatch, gitreins, debugging, etc.) — never preload the whole toolbox. " +
	"Read the project board: .coding-hermes/board/tasks.jsonl if present (JSONL-canonical), else .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. " +
	"OFF-BY-ONE (pre-solve lab, localhost:8766): BEFORE debugging any error or designing a fix from scratch, discover a pre-verified answer via `curl -s -X POST http://localhost:8766/api/v1/problems/discover -H 'Content-Type: application/json' -d '{\"problem_class\":\"<class>\"}'` or grep the flat corpus data/answers/ (per the off-by-one skill). If you had to debug something non-trivial, submit it (`cadence: post-debug`) so future ticks hit a cached answer. " +
	"IMPORTANT — worker dispatch: You are the FOREMAN. You pick ONE board task, then dispatch a WORKER to implement it. " +
	"Do NOT implement complex tasks yourself. To dispatch a worker, run a BACKGROUND process via your terminal tool: " +
	"`hermes chat -q \"<task brief from the board, plus files-to-modify and acceptance criteria>\" -m <worker_model> --provider <worker_provider> -s coding-hermes-worker --ignore-rules -Q` " +
	"(terminal background=true). The worker shares this same workdir, so it edits files and commits directly. " +
	"Then poll the background process until it exits, verify build/lint/test and the commit landed, update the board, and report. " +
	"MANDATORY PUSH AFTER EVERY COMMIT — do not skip: after ANY commit (worker or yours), run `git push origin <branch>` (or `git push`) and verify `git fetch origin && git rev-list --count origin/<branch>..HEAD` is 0. A tick that ends with unpushed commits is NOT complete. Never rely on the worker having pushed — verify the remote HEAD yourself. On non-fast-forward push, `git pull --rebase`, re-run the gate, push. " +
	"Only implement trivial one-file changes yourself; anything multi-file or architectural goes to a worker. " +
	"MANDATORY GitReins lifecycle — do not skip: (1) BEFORE any implementation, run `gitreins task create <TASK-ID> \"<title>\" \"<criterion>\"` then `gitreins task start <TASK-ID>` for the board task you picked. " +
	"(2) AFTER the worker commits the work (verify the commit exists in git log), ALWAYS run `gitreins task complete <TASK-ID>` — this fires the Tier 2 LLM judge and writes verdict.json. " +
	"NEVER end a tick without running `gitreins task complete` for the picked task — even if the tick is near its timeout, complete the gitreins task FIRST, then update the board. " +
	"(3) Then delete the gitreins task with `gitreins task delete <TASK-ID>` to keep tasks.yaml clean (optional — the fleet default keeps completed tasks for audit). " +
	"If the worker committed but you missed the gitreins lifecycle, run `gitreins task complete` on the committed work before finishing. " +
	"MANDATORY CI-health check — do not skip: run `gh run list --repo <org>/<repo> --limit 3 --json status,conclusion,displayTitle,headBranch,createdAt` (derive org/repo from `git remote -v` — the on-disk folder name may not match the GitHub org). If ANY recent run shows conclusion=failure that YOU did not just create, file a board task for the broken CI (e.g. INT-CI-<n> '<what failed>') before ending the tick, so it does not rot. Report CI health (green or the failure you flagged) in your output. " +
	"Format your final output as clean, well-structured markdown with tables and sections. " +
	"Report result."

// buildForemanPrompt assembles the tick prompt for a project (Bane 2026-08-27).
//
// Prompt resolution order:
//  1. base = namespace default_prompt (fleet.toml [[namespaces]].default_prompt);
//     empty namespace default falls back to the built-in prompt.
//  2. project prompt (fleet.toml [[projects]].prompt):
//     - prompt_mode="replace" → the project prompt REPLACES the base entirely
//     - prompt_mode="append" (default) → the project prompt is APPENDED to the base
//
// The scheduler always injects the dynamic environment footer (tick id,
// workdir, worker model/provider) so no config prompt can lose them.
func buildForemanPrompt(project PackedProject, tickID string) string {
	base := project.NamespacePrompt
	if base == "" {
		base = builtinForemanPrompt
	}
	if project.Prompt != "" {
		if project.PromptMode == "replace" {
			base = project.Prompt
		} else {
			base = base + "\n\n" + project.Prompt
		}
	}
	return "[Scheduler tick: " + tickID + "] " + base +
		"\nWorkdir: " + project.Workdir + "." +
		"\nWorker model/provider: " + WorkerDefaults(project) + "."
}

// chainEntry is one step of the model/provider fallback chain (SCHED-GAP-064).
// An entry is PRESENT when at least one of model/provider is non-empty; a
// present entry contributes its non-empty fields, and any field still empty
// falls through to the next present entry. Model and provider therefore
// resolve INDEPENDENTLY through the chain.
type chainEntry struct {
	model    string
	provider string
}

// present reports whether the entry contributes anything to the chain.
func (e chainEntry) present() bool {
	return e.model != "" || e.provider != ""
}

// resolveChain returns the effective model/provider by walking the given
// chain left-to-right: the first present entry seeds the result, and every
// subsequent present entry back-fills whichever field is still empty. The
// final result is the first non-empty value per field, in chain order.
func resolveChain(entries []chainEntry) (model, provider string) {
	for _, e := range entries {
		if !e.present() {
			continue
		}
		if model == "" {
			model = e.model
		}
		if provider == "" {
			provider = e.provider
		}
	}
	return model, provider
}

// resolveModelProvider resolves the spawn model/provider through the full
// fallback chain for a packed project:
//
//  1. project primary   (project.Model / project.Provider)
//  2. project fallback  (project.FallbackModel / project.FallbackProvider)
//  3. global primary    (spawner defaults: s.model / s.provider — env
//     SCHEDULER_FOREMAN_MODEL / SCHEDULER_FOREMAN_PROVIDER)
//  4. global fallback   (spawner env SCHEDULER_FOREMAN_FALLBACK_MODEL /
//     SCHEDULER_FOREMAN_FALLBACK_PROVIDER)
//
// Steps 3-4 are SKIPPED entirely when the project sets NoGlobalFallback —
// the chain then ends after the project fallback tier, and fields that are
// still empty resolve to empty (the gateway/exec receives an empty
// model/provider rather than a global default). A project with empty project
// tiers and no flag keeps today's behavior: it resolves to the spawner's
// global defaults (step 3), with the env fallback (step 4) only reachable
// when the global primary itself is empty.
func (s *Spawner) resolveModelProvider(project PackedProject) (model, provider string) {
	return resolveChain(s.spawnChain(project))
}

// nextChainResolution returns the model/provider for the chain step AFTER the
// first present entry (the entry that seeded the primary resolution) — the
// values a gateway 401/403 retry should use. ok is false when no further
// present entry exists (chain exhausted → no retry) or when the remainder
// resolves to all-empty (nothing to retry with).
func nextChainResolution(chain []chainEntry) (model, provider string, ok bool) {
	for i, e := range chain {
		if e.present() {
			m, p := resolveChain(chain[i+1:])
			return m, p, m != "" || p != ""
		}
	}
	return "", "", false
}

// spawnChain builds the project's fallback chain. SCHED-GAP-075: when
// project.ModelChain is set (non-empty JSON array), it becomes the
// project-tier entries. Otherwise falls back to the SCHED-GAP-064
// model/provider + fallback_model/fallback_provider pattern.
//
// Chain order: project chain entries → namespace chain (Bane 2026-08-27,
// [[namespaces]].model_chain — the workspace tier, sits between project and
// router so mergedChain keeps it ahead of the router entries) → global tiers
// (unless NoGlobalFallback).
func (s *Spawner) spawnChain(project PackedProject) []chainEntry {
	var chain []chainEntry
	if project.ModelChain != "" {
		chain = parseModelChain(project.ModelChain)
	} else {
		chain = []chainEntry{
			{model: project.Model, provider: project.Provider},
			{model: project.FallbackModel, provider: project.FallbackProvider},
		}
	}
	// Namespace tier: parsed from the namespace model_chain JSON array.
	// Empty/parse-failure contributes nothing (chain falls through).
	if project.NamespaceChain != "" {
		chain = append(chain, parseModelChain(project.NamespaceChain)...)
	}
	// Global tiers appended unless no_global_fallback is set.
	if !project.NoGlobalFallback {
		chain = append(chain,
			chainEntry{model: s.model, provider: s.provider},
			chainEntry{model: s.fallbackModel, provider: s.fallbackProvider},
		)
	}
	return chain
}

// parseModelChain parses a JSON array of "model@provider" strings into
// chainEntry slices. Each "model@provider" entry is split on "@" — entries
// without "@" use the whole string as model with empty provider.
// Invalid JSON or empty input yields nil (caller falls back to legacy chain).
func parseModelChain(raw string) []chainEntry {
	var parts []string
	if err := json.Unmarshal([]byte(raw), &parts); err != nil || len(parts) == 0 {
		return nil
	}
	entries := make([]chainEntry, 0, len(parts))
	for _, p := range parts {
		m, prov, _ := strings.Cut(p, "@")
		entries = append(entries, chainEntry{model: m, provider: prov})
	}
	return entries
}

// tickIsIdle reports whether this spawn is an IDLE tick: the project's board
// has zero pending tasks (SCHED-GAP-065). A nil counter (tests, tooling)
// biases to work — the conservative default that never cheap-routes real
// work. CountPending returns 0 for a missing board, so a project with no
// board file at all is idle too.
func (s *Spawner) tickIsIdle(project PackedProject) bool {
	if s.pendingCounter == nil {
		return false
	}
	return s.pendingCounter.CountPending(project.Workdir) == 0
}

// spawnChainForKind builds the chain for the tick kind (SCHED-GAP-065). For
// idle ticks the idle tiers are PREPENDED to the regular chain:
//
//  1. project idle   (project.IdleModel / project.IdleProvider)
//  2. global idle    (spawner env SCHEDULER_FOREMAN_IDLE_MODEL / _PROVIDER)
//     3+. the regular chain (project primary → project fallback → global
//     primary → global fallback, honoring NoGlobalFallback)
//
// The global idle tier is a spawner-level (env) lane, so it is gated by
// NoGlobalFallback exactly like the global primary/fallback tiers. Empty
// idle fields are not present() and fall through naturally, so a project
// with no idle config resolves EXACTLY as a work tick — no behavior
// regression for fleets without idle lanes. The gateway 401/403 retry walks
// this same chain so a rejected idle tick never retries onto the work lane
// out of order.
func (s *Spawner) spawnChainForKind(project PackedProject, idle bool) []chainEntry {
	regular := s.spawnChain(project)
	if !idle {
		return regular
	}
	idleTiers := []chainEntry{{model: project.IdleModel, provider: project.IdleProvider}}
	if !project.NoGlobalFallback {
		idleTiers = append(idleTiers, chainEntry{model: s.idleModel, provider: s.idleProvider})
	}
	return append(idleTiers, regular...)
}

// mergedChain inserts the router chain entries between the project entries
// and the global tiers of the base chain. The base chain is split at the
// boundary: entries before the first global tier are "project" entries;
// everything from the first global tier onward is "global" entries. The
// router chain is spliced in between. For chains built via model_chain
// (SCHED-GAP-075) the global tiers are at the end (gated by
// NoGlobalFallback); for legacy chains they're the last 2 entries.
func (s *Spawner) mergedChain(project PackedProject, base []chainEntry, routerChain []chainEntry) []chainEntry {
	// Split base into project-portion and global-portion.
	// The global portion starts at the first entry that matches a global tier.
	globalStart := len(base)
	globalTier0 := chainEntry{model: s.model, provider: s.provider}
	for i, e := range base {
		if e == globalTier0 {
			globalStart = i
			break
		}
	}
	projectPart := base[:globalStart]
	globalPart := base[globalStart:]
	// Build merged: project + router + global
	merged := make([]chainEntry, 0, len(projectPart)+len(routerChain)+len(globalPart))
	merged = append(merged, projectPart...)
	merged = append(merged, routerChain...)
	merged = append(merged, globalPart...)
	return merged
}

// canSpawn checks concurrency limits.
func (s *Spawner) canSpawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) < s.maxConcurrent
}

// Spawn launches a foreman for the given project and tick ID.
// Returns an error only if the process fails to start.
// The spawned process is tracked internally and reaped by the lifecycle tracker.
func (s *Spawner) Spawn(project PackedProject, tickID string) (*SpawnedTick, error) {
	if !s.canSpawn() {
		return nil, fmt.Errorf("max concurrency %d reached", s.maxConcurrent)
	}

	var cmd *exec.Cmd

	// model/provider are the resolved (provider, model) pair for this
	// spawn. Custom-command spawns never resolve a pair — they stay
	// empty and the circuit recorder skips them (TASK-ROUTER-002).
	var model, provider string

	// SCHED-GAP-078: routerRes holds the spawn-time router result so the
	// cost of the pair that ACTUALLY runs (head, retry hop, or foreman
	// fallback) can be looked up without a second invocation. rate is the
	// router's PUBLIC price for the current (provider, model); unknown
	// pairs (router down / pair not priced) fall back to the static maps.
	var routerRes RouterResult
	rate := routerRate{}

	if project.Command != "" {
		// Custom command.
		if strings.Contains(project.Command, "bash -c") {
			// Shell one-liner — pass the script string directly to bash -c.
			script := strings.TrimPrefix(project.Command, "bash -c ")
			script = strings.TrimSpace(script)
			// Strip surrounding quotes if present.
			script = strings.Trim(script, "'\"")
			cmd = exec.Command("bash", "-c", script)
		} else {
			parts := splitCommand(project.Command)
			cmd = exec.Command(parts[0], parts[1:]...)
		}
		cmd.Dir = project.Workdir
		// DAGGER-FOREMAN (2026-08-27): custom-command spawns need the same
		// tick identity the Hermes branch gets — the dagger driver derives
		// its board-event tick id from CODING_HERMES_TICK so ledger rows and
		// board events share one id.
		cmd.Env = append(os.Environ(),
			"CODING_HERMES_TICK="+tickID,
			"CODING_HERMES_SOURCE=scheduler",
			"CODING_HERMES_PROJECT="+project.Name,
		)
	} else {
		// SCHED-GAP-064: resolve the spawn model/provider through the
		// fallback CHAIN: project primary → project fallback → global
		// primary (spawner env defaults) → global fallback (env). Model
		// and provider resolve INDEPENDENTLY: an entry counts when at
		// least one of the two is non-empty, and each field falls
		// through to the next entry's non-empty value. The gateway and
		// exec branches below consume the SAME resolved values, so the
		// f3919a7 provider fix (foreman key, not the MAIN key) keeps
		// working through every chain step.
		//
		// SCHED-GAP-065: pick the chain KIND first from the board's
		// pending-task count — zero pending = idle tick, resolved via
		// the idle chain (project idle tier + global idle lane prepended
		// to the regular chain). The gateway 401/403 retry below walks
		// THIS chain, so a rejected idle tick retries within the idle
		// chain instead of jumping straight to the work lane.
		idle := s.tickIsIdle(project)
		chain := s.spawnChainForKind(project, idle)
		model, provider = resolveChain(chain)
		// TASK-ROUTER-001 + SCHED-GAP-075: single router resolve call.
		// The router returns both the head (preferred model/provider) and
		// the full chain (ordered eligible hops). The head overrides the
		// resolved model/provider. The chain is merged INTO the spawn chain
		// between project entries and global tiers, so 401/403 retry walks
		// through all hops including router entries.
		// Fail-open: any router unavailability keeps the chain-resolved values.
		// SCHED-GAP-078: the resolved result is kept in scope so the price of
		// the pair that ACTUALLY runs (head, retry hop, or foreman fallback)
		// feeds the tick's cost — provider-aware, public-price-first.
		if res, ok := s.resolveRouterFull(project); ok {
			routerRes = res
			if m, p, headOK := res.OpenHead(); headOK {
				model, provider = m, p
				log.Printf("ROUTER: %s profile=%s gate=%s head=%s/%s", project.Name, res.Profile, res.Gate, p, m)
			}
			if routerChain := res.OpenChain(); len(routerChain) > 0 {
				chain = s.mergedChain(project, chain, routerChain)
				model, provider = resolveChain(chain)
				// Re-apply router head after merge
				if m, p, headOK := res.OpenHead(); headOK {
					model, provider = m, p
				}
			}
		}
		chainKind := "work"
		if idle {
			chainKind = "idle"
		}
		log.Printf("SPAWN: %s tick=%s chain=%s model=%q provider=%q", project.Name, tickID, chainKind, model, provider)

		prompt := buildForemanPrompt(project, tickID)

		// GAP-048: when the gateway was unreachable at startup and
		// noExecFallback is set, the spawner has no HTTP client (gateway is
		// nil) and must NOT silently degrade to exec.Command. Without this
		// guard the nil-gateway path falls straight through to the exec spawn
		// block below, bypassing the noExecFallback check that only lives
		// inside the gateway-fail branch — the daemon exec-spawns forever
		// even though the flag says to stay idle. The background reconnector
		// will SetGatewayClient when the gateway recovers, re-engaging HTTP.
		if s.gateway == nil && s.noExecFallback {
			log.Printf("SKIPPED: %s tick=%s no gateway client and exec fallback disabled — staying idle", project.Name, tickID)
			s.noteSpawnFailure(project.Name)
			if s.events != nil {
				s.events.Emit(context.Background(), SeverityHigh, "spawn",
					"gateway unavailable and exec fallback disabled — tick dropped", map[string]any{
						"project":          project.Name,
						"tick_id":          tickID,
						"gateway":          "nil",
						"no_exec_fallback": true,
					})
			}
			return nil, fmt.Errorf("no gateway client and exec fallback disabled for %s", project.Name)
		}

		// Try HTTP gateway spawn first (zero process overhead).
		if s.gateway != nil {
			reqStart := time.Now() // SCHED-GAP-029: capture before SendResponse for git window

			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)

			// GAP-035: validate a per-project gateway key BEFORE dispatch.
			// The 2026-08-04 outage had the fleet send revoked fk-* keys
			// blindly — every spawn burned a full gateway cycle, failed, and
			// retried next eval: 8208+ silent failures. A cheap authenticated
			// probe turns that into a fail-fast terminal error with a HIGH
			// event. Non-auth probe errors (network blip, slow gateway) are
			// non-terminal: dispatch proceeds and the SendResponse 401
			// classification backstops.
			if project.GatewayKey != "" {
				vctx, vcancel := context.WithTimeout(ctx, gatewayKeyProbeTimeout)
				verr := s.gateway.ValidateKey(vctx, project.GatewayKey)
				vcancel()
				if errors.Is(verr, ErrGatewayKeyRejected) {
					cancel()
					return nil, s.gatewayKeyRejected(project.Name, tickID, verr)
				}
				if verr != nil {
					log.Printf("GATEWAY KEY PROBE: %s tick=%s error=%v — proceeding to dispatch", project.Name, tickID, verr)
				}
			}

			// S-GAP-003: the gateway call below is synchronous and blocks for
			// the whole tick (up to --tick-timeout). Persist a placeholder
			// session id + first heartbeat BEFORE it starts, so the tick row
			// never sits 'running' with session_id NULL (the zombie
			// signature). The placeholder is the tick's own id — unique and
			// self-describing; the real gateway session id replaces it on
			// success below.
			if _, err := s.db.Exec(`UPDATE ticks SET session_id = ?, heartbeat_at = ? WHERE id = ?`,
				tickID, time.Now().Format(time.RFC3339), tickID); err != nil {
				log.Printf("WARN: placeholder session_id/heartbeat for %s: %v", tickID, err)
			}
			// SCHED-GAP-060: stamp last_tick_started AT SPAWN so a running
			// tick already reports its own spawn time (mirrors the exec
			// branch below). The post-completion UPDATE used to write the
			// COMPLETION time here. reqStart is captured before
			// SendResponse (SCHED-GAP-029) — do NOT use time.Now().
			_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ? WHERE name = ?`,
				reqStart.Format(time.RFC3339), project.Name)
			stopHeartbeat := s.startHeartbeat(tickID)

			// Per-foreman gateway key: project.GatewayKey when set, else the
			// daemon's shared --gateway-key (Bane 2026-07-31).
			// SCHED-GAP-064: when the gateway rejects the PRIMARY chain
			// combination with an auth-class error (HTTP 401/403 — the
			// provider/model combination is rejected, NOT the pre-validated
			// gateway key), retry ONCE with the NEXT entry in the fallback
			// chain. Other errors (network, 5xx, timeout) keep the
			// single-attempt behavior, and a chain with no next entry (the
			// common single-tier project) keeps the legacy GAP-035
			// fail-fast: one dispatch, terminal ErrGatewayKeyRejected, no
			// retry flood. On a successful retry the tick's model/provider
			// (cost lookup) reflect the entry that actually ran.
			// SCHED-GAP-065: `chain` is the kind-appropriate chain computed
			// at resolution above — idle chain for idle ticks, regular
			// chain otherwise — so the retry advances within the SAME chain
			// the tick was spawned with.
			retryModel, retryProvider, hasRetry := nextChainResolution(chain)
			// TASK-ROUTER-002: circuitFailureRecorded tracks whether the
			// closure below already recorded the primary pair (the 401/403
			// path records BEFORE the retry hop). The GATEWAY FAIL block
			// must not record the same pair twice — a double
			// record-failure would double the breaker cooldown.
			circuitFailureRecorded := false
			resp, gwErr := func() (*Response, error) {
				// The heartbeat goroutine must never outlive the request —
				// stop it on every return path (success AND failure).
				defer close(stopHeartbeat)
				defer cancel()
				r, err := s.gateway.SendResponseWithSessionKey(ctx, prompt, model, provider, project.GatewayKey, tickID)
				// SCHED-GAP-080: transient-only bounded retry on the SAME
				// model/provider pair. A gateway HTTP 5xx, network/timeout or
				// read/unmarshal failure is a transport blip — retry it with
				// exponential backoff BEFORE the SKIP decision so a blip that
				// recovers completes the tick instead of silently dropping it
				// (the 2026-08-28 05:01-05:03 + 07:56 corruption burst dropped 7
				// ticks with zero retries). 401/403 (ErrGatewayKeyRejected) never
				// enter this path — auth stays terminal (GAP-035) and the
				// chain-hop logic below is untouched. The tick context bounds the
				// loop, so a persistently-5xx gateway still fails the tick.
				if err != nil && IsTransientGatewayErr(err) {
					atomic.AddInt64(&s.spawnGatewayErrors, 1)
					for attempt := 1; attempt <= gatewayRetryMaxAttempts; attempt++ {
						log.Printf("GATEWAY RETRY: %s tick=%s attempt=%d/%d model=%q provider=%q error=%v",
							project.Name, tickID, attempt, gatewayRetryMaxAttempts, model, provider, err)
						select {
						case <-ctx.Done():
							return r, err
						case <-time.After(gatewayRetryBackoff(attempt)):
						}
						r, err = s.gateway.SendResponseWithSessionKey(ctx, prompt, model, provider, project.GatewayKey, tickID)
						if err == nil {
							break
						}
						if IsTransientGatewayErr(err) {
							atomic.AddInt64(&s.spawnGatewayErrors, 1)
							if ctx.Err() != nil {
								break
							}
							continue
						}
						break
					}
				}
				if err == nil || !errors.Is(err, ErrGatewayKeyRejected) || !hasRetry {
					return r, err
				}
				// TASK-ROUTER-002: the gateway rejected the pair — record
				// the failure in the circuit state BEFORE advancing. The
				// breaker cooldown (open_until, 5m → double per
				// consecutive failure → 1h cap, managed by
				// router_circuit.py) becomes the backoff: the pair is
				// excluded from future router heads until it cools, and
				// this tick never re-sends it (max 1 attempt per hop).
				s.recordCircuitFailure(provider, model, "gateway 401/403 rejected pair")
				circuitFailureRecorded = true
				log.Printf("GATEWAY FALLBACK: %s tick=%s primary model=%q provider=%q rejected (HTTP 401/403) — retrying once with model=%q provider=%q",
					project.Name, tickID, model, provider, retryModel, retryProvider)
				r2, err2 := s.gateway.SendResponseWithSessionKey(ctx, prompt, retryModel, retryProvider, project.GatewayKey, tickID)
				if err2 == nil {
					model, provider = retryModel, retryProvider
					// SCHED-GAP-078: the tick's cost follows the pair that
					// actually ran — look up its router price in the
					// SPAWN-TIME result (no re-invocation; the chain is
					// static for this tick). Pairs the router never priced
					// leave rate unknown → static map fallback.
					if rr, ok := routerRes.HopRate(provider, model); ok {
						rate = rr
					}
				} else if errors.Is(err2, ErrGatewayKeyRejected) {
					// The retry hop is rejected too — record it as well.
					// No further hops: max 1 attempt per hop per tick,
					// and the breaker cooldown IS the cross-tick backoff.
					s.recordCircuitFailure(retryProvider, retryModel, "gateway 401/403 retry hop rejected")
					// Bane 2026-08-27 (final-fallback rule): when the whole
					// chain is exhausted (or empty), make ONE final attempt
					// with the FOREMAN FALLBACK — the config-set ultimate
					// (resolveModelProvider: project primary → project
					// fallback → global env tiers; e.g.
					// deepseek-v4-flash/deepseek-foreman = DeepSeek V4
					// PAYG). Only when that ALSO rejects does the tick fail
					// with the terminal GAP-035 classification. The fallback
					// is skipped when it duplicates a pair already attempted
					// (single-tier projects keep the legacy fail-fast).
					ffModel, ffProvider := s.resolveModelProvider(project)
					if ffModel != "" && ffProvider != "" &&
						(ffModel != model || ffProvider != provider) &&
						(ffModel != retryModel || ffProvider != retryProvider) {
						log.Printf("GATEWAY FOREMAN FALLBACK: %s tick=%s chain exhausted (primary=%q/%q retry=%q/%q) — final attempt model=%q provider=%q",
							project.Name, tickID, model, provider, retryModel, retryProvider, ffModel, ffProvider)
						r3, err3 := s.gateway.SendResponseWithSessionKey(ctx, prompt, ffModel, ffProvider, project.GatewayKey, tickID)
						if err3 == nil {
							model, provider = ffModel, ffProvider
							// SCHED-GAP-078: cost follows the pair that ran.
							if rr, ok := routerRes.HopRate(provider, model); ok {
								rate = rr
							}
							return r3, nil
						}
						if errors.Is(err3, ErrGatewayKeyRejected) {
							s.recordCircuitFailure(ffProvider, ffModel, "gateway 401/403 foreman fallback rejected")
						}
						return r3, err3
					}
				}
				return r2, err2
			}()
			if gwErr == nil && resp != nil {
				atomic.AddInt64(&s.spawnCountHTTP, 1)
				text := resp.ExtractText()
				now := time.Now()

				// SCHED-GAP-079: a gateway 2xx is NOT automatically a
				// completed tick. The gateway accepted the request, but a
				// response whose status is an explicit failure, or that
				// carries neither output text nor a persisted session id
				// (zero-output-with-no-session), is a FAILED tick — recording
				// it completed/committed would be a false success (heading-sync
				// field test 10:06Z 2026-08-28). Gate rule:
				//   (a) explicit failure statuses always fail;
				//   (b) empty output AND empty session id fails (a tool-only
				//       tick has a real resp.ID and stays completed — NEVER
				//       gate on output length alone);
				//   (c) everything else stays completed.
				failureStatuses := map[string]bool{
					"failed":                     true,
					"session_persistence_failed": true,
					"error":                      true,
					"cancelled":                  true,
					"timeout":                    true,
				}
				failed := failureStatuses[resp.Status] || (strings.TrimSpace(text) == "" && resp.ID == "")
				if failed {
					var errText string
					switch {
					case resp.Error != nil && resp.Error.Message != "":
						errText = fmt.Sprintf("gateway response %s (len=%d): %s", resp.Status, len(text), resp.Error.Message)
					case failureStatuses[resp.Status]:
						errText = fmt.Sprintf("gateway response %s (len=%d): no error detail", resp.Status, len(text))
					default:
						// Empty-output-AND-empty-session rule: name the
						// missing session so the row is diagnosable.
						errText = fmt.Sprintf("gateway response %s (len=%d): no session persisted and no output text", resp.Status, len(text))
					}
					log.Printf("GATEWAY FAIL: %s tick=%s status=%s — recorded failed: %s", project.Name, tickID, resp.Status, errText)
					// TASK-ROUTER-002: the pair produced a failed session —
					// cool it so the breaker stops routing it to heads.
					s.recordCircuitFailure(provider, model, "gateway response "+resp.Status)
					s.noteSpawnFailure(project.Name)
					// Corruption / session-persistence failures are fleet-wide
					// signals (gateway state.db broken, sessions not landing):
					// surface a HIGH event with tick id + project immediately.
					if s.events != nil && (strings.Contains(errText, "session_persistence_failed") || strings.Contains(errText, "database disk image is malformed")) {
						s.events.Emit(context.Background(), SeverityHigh, "spawn",
							"gateway tick recorded failed: "+resp.Status, map[string]any{
								"project": project.Name,
								"tick_id": tickID,
								"error":   errText,
							})
					}
					// Return a NON-completed tick: Wait() yields TickFailed and
					// slot_pool's existing lifecycle.Complete path persists
					// status=failed / outcome=failed / error=<gateway text>.
					// Do NOT increment spawnCountHTTP beyond the one above and
					// do NOT reset consecutive_failures — this is a failure.
					return &SpawnedTick{
						TickID:     tickID,
						Project:    project.Name,
						SessionID:  tickID, // placeholder — no real session persisted
						Started:    reqStart,
						Deliver:    project.Deliver,
						spawner:    s,
						completed:  false,
						completeAt: now,
						gwFailErr:  errText,
						usage:      resp.Usage,
						model:      model,
						provider:   provider,
						rate:       rate,
						workdir:    project.Workdir,
						reqStart:   reqStart,
						Trigger:    "prompt",
					}, nil
				}

				// NOTE: tick completion is handled by slot_pool → lifecycle.Complete
				// (correct columns + outcome CHECK). The legacy direct UPDATE here was
				// removed in GAP-002 — it referenced non-existent columns
				// (finished_at, output) and outcome='ok' violated the ticks CHECK, so
				// it silently no-oped on every run.
				// S-GAP-001: a successful spawn also resets the consecutive-failure
				// backoff counter. SCHED-GAP-060: last_tick_started is NO longer
				// written here — it was stamped at spawn time above (writing the
				// completion moment here corrupted the API field).
				_, _ = s.db.Exec(`UPDATE projects SET consecutive_failures = 0 WHERE name = ?`,
					project.Name)

				// TASK-ROUTER-002: the pair that ACTUALLY ran succeeded —
				// close its circuit (record-success is a no-op for pairs
				// with no recorded failures; fire-and-forget).
				s.recordCircuitSuccess(provider, model)

				// S-GAP-003: persist the REAL gateway session id (resp.ID);
				// fall back to the placeholder tick id when the gateway
				// returns none, so the row never goes back to NULL.
				sessionID := resp.ID
				if sessionID == "" {
					sessionID = tickID
				}
				// SCHED-GAP-079: a completed tick with empty text but a REAL
				// persisted session (tool-only tick — DuckBrain writes etc.)
				// is legitimately completed; surface it as INFO so it is
				// distinguishable from the zero-output-no-session failures.
				if strings.TrimSpace(text) == "" && s.events != nil {
					s.events.Emit(context.Background(), SeverityInfo, "spawn",
						"gateway tick completed with no text output (tool-only)", map[string]any{
							"project":    project.Name,
							"tick_id":    tickID,
							"session_id": sessionID,
						})
				}
				log.Printf("GATEWAY: %s tick=%s tokens=%d/%d",
					project.Name, tickID, resp.Usage.InputTokens, resp.Usage.OutputTokens)
				return &SpawnedTick{
					TickID:     tickID,
					Project:    project.Name,
					SessionID:  sessionID,
					Started:    reqStart, // SCHED-GAP-029: use request start, not completion
					Deliver:    project.Deliver,
					Output:     *bytes.NewBufferString(text),
					spawner:    s,
					completed:  true,
					completeAt: now,
					// SCHED-GAP-029: carry real usage + context for outcome metrics.
					usage:    resp.Usage,
					model:    model,
					provider: provider,
					rate:     rate, // SCHED-GAP-078: router PUBLIC price for the pair that ran
					workdir:  project.Workdir,
					reqStart: reqStart,
					// Gateway spawns are always prompt-based (Bane 2026-08-27).
					Trigger: "prompt",
				}, nil
			}
			log.Printf("GATEWAY FAIL: %s tick=%s error=%v — falling back to exec.Command", project.Name, tickID, gwErr)
			// TASK-ROUTER-002: the attempted pair failed (timeout, HTTP
			// error, gateway failure) — record it so the breaker cools it
			// across ticks. The 401/403 path already recorded the primary
			// pair before its retry hop, so skip it here (a double
			// record-failure would double the cooldown).
			if !circuitFailureRecorded {
				s.recordCircuitFailure(provider, model, "gateway failure: "+gwErr.Error())
			}
			// SCHED-GAP-079: when the gateway wraps a session failure in the
			// response error envelope, gateway_client.go (read-only) classifies
			// it as a transport error BEFORE the completion gate — the text
			// still lands in the tick error (slot_pool persists Spawn's error),
			// and the HIGH event below keeps the fleet-wide signal (gateway
			// state.db corruption, sessions not persisting) visible with tick
			// id + project on this path too.
			if s.events != nil && (strings.Contains(gwErr.Error(), "database disk image is malformed") || strings.Contains(gwErr.Error(), "session_persistence_failed")) {
				s.events.Emit(context.Background(), SeverityHigh, "spawn",
					"gateway tick recorded failed: session error", map[string]any{
						"project": project.Name,
						"tick_id": tickID,
						"error":   gwErr.Error(),
					})
			}
			// GAP-035: an AUTH rejection is terminal. Falling back to exec
			// would silently mask a key regression and keep flooding the
			// fleet with disguised failures — fail fast with a classified
			// error + HIGH event instead, regardless of the no-exec-fallback
			// flag (which only governs transient gateway errors).
			if errors.Is(gwErr, ErrGatewayKeyRejected) {
				return nil, s.gatewayKeyRejected(project.Name, tickID, gwErr)
			}
			if s.noExecFallback {
				log.Printf("SKIPPED: %s tick=%s exec fallback disabled, dropping tick", project.Name, tickID)
				s.noteSpawnFailure(project.Name)
				return nil, fmt.Errorf("gateway unreachable and exec fallback disabled: %w", gwErr)
			}
		}

		// SCHED-GAP-074: the exec fallback cannot carry the tick id as a
		// session key — the CLI derives its own session identity. The
		// prompt's "[Scheduler tick: <id>]" prefix remains the linkage
		// marker for exec-spawned sessions (same as historical gateway
		// ticks), which is acceptable: exec fallback is disabled by default
		// (--no-exec-fallback) and only ever fires when the gateway is
		// unreachable, i.e. no /v1/responses row exists to lose.
		args := []string{
			"chat", "-q", prompt,
			"-m", model,
			"--provider", provider,
			"-s", "coding-hermes-foreman",
			"-s", "coding-hermes-board",
			"-s", "coding-hermes-model-router",
			"-s", "coding-hermes-never-done",
			"-s", "coding-hermes-specs",
			"-s", "coding-hermes-testing",
			"-s", "coding-hermes-middle-out",
			"-s", "systematic-debugging",
			"-s", "trust-but-verify",
			"-s", "reality-validation",
			"-s", "github-pr-workflow",
			"-s", "github-repo-management",
			"-s", "claude-design",
			"-s", "popular-web-designs",
			"-s", "hilo-usage",
			"-s", "gitreins",
			"-s", "off-by-one",
			"--ignore-rules", "-Q",
		}

		cmd = exec.Command("hermes", args...)
		cmd.Dir = project.Workdir
		cmd.Env = append(os.Environ(),
			"HERMES_HOME="+s.foremanHome,
			"CODING_HERMES_TICK="+tickID,
			"CODING_HERMES_SOURCE=scheduler",
			"CODING_HERMES_PROJECT="+project.Name,
		)
	}

	// Count the spawn method for /api/v1/health (spawns_exec). Custom-command
	// spawns (tests/probes) are exec spawns too — the HTTP path returned
	// above, so reaching here always means an exec spawn (GAP-049).
	atomic.AddInt64(&s.spawnCountExec, 1)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		s.recordCircuitFailure(provider, model, "exec stdout pipe error")
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		s.recordCircuitFailure(provider, model, "exec stderr pipe error")
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		s.noteSpawnFailure(project.Name)
		s.recordCircuitFailure(provider, model, "exec start error: "+err.Error())
		return nil, fmt.Errorf("start process: %w", err)
	}

	// TASK-ROUTER-002: the exec spawn started with the resolved pair —
	// close its circuit (no-op for pairs without recorded failures).
	s.recordCircuitSuccess(provider, model)

	s.mu.Lock()
	s.active[tickID] = cmd
	s.mu.Unlock()

	st := &SpawnedTick{
		TickID:  tickID,
		Project: project.Name,
		PID:     cmd.Process.Pid,
		Started: time.Now(),
		Deliver: project.Deliver,
		cmd:     cmd,
		stdout:  stdout,
		stderr:  stderr,
		spawner: s,
		// SCHED-GAP-029: carry workdir for potential future metric enrichment.
		workdir: project.Workdir,
		// TASK-ROUTER-002: carry the resolved provider alongside model so
		// the completion path can record (provider, model) failures.
		model:    model,
		provider: provider,
		rate:     rate, // SCHED-GAP-078: router PUBLIC price for the pair that ran
		// Bane 2026-08-27: report the trigger kind in delivered reports.
		Trigger: map[bool]string{true: "command", false: "prompt"}[project.Command != ""],
	}
	// S-GAP-003: keep the tick row's heartbeat fresh for the life of the
	// process; Wait() stops the goroutine. The gateway branch above runs
	// the same heartbeat around its blocking request.
	st.stopHeartbeat = s.startHeartbeat(tickID)

	// Snapshot the repo at spawn so the completion path can count commits and
	// files the foreman added during this tick.
	st.preHead, st.preCommits = gitBaseline(project.Workdir)

	// Tee stdout: scanner reads session_id from one side, buffer captures full output.
	teeReader := io.TeeReader(stdout, &st.Output)

	// Parse session ID from stdout and persist it. The scanner goroutine must
	// exit when the process exits or times out so it cannot leak.
	scanCtx, scanCancel := context.WithTimeout(context.Background(), s.timeout)
	st.scanCancel = scanCancel

	// Close stdout when context expires — unblocks scanner.Scan().
	go func() {
		<-scanCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer scanCancel()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: stdout scanner panic for tick %s: %v", tickID, r)
			}
		}()
		scanner := bufio.NewScanner(teeReader)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "session_id:") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
				st.mu.Lock()
				st.SessionID = id
				st.mu.Unlock()
				// Persist session_id to the database.
				if _, err := s.db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, id, tickID); err != nil {
					log.Printf("ERROR persisting session_id for %s: %v", tickID, err)
				}
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			// Expected on timeout (pipe closed) or process exit — not a leak.
			if !errors.Is(err, io.EOF) {
				log.Printf("WARN: stdout scanner error for tick %s: %v", tickID, err)
			}
		}
	}()

	// Update tick to running with PID for zombie detection.
	// S-GAP-003: also stamp session_id (placeholder = tick id — the stdout
	// scanner above overwrites it with the real parsed session id when a
	// "session_id:" line appears) and the first heartbeat, so no running row
	// ever has NULL session_id and the reapers always have a liveness signal.
	_, err = s.db.Exec(`
		UPDATE ticks SET status = 'running', spawned_at = ?, pid = ?, session_id = ?, heartbeat_at = ?
		WHERE id = ?
	`, st.Started.Format(time.RFC3339), st.PID, tickID, st.Started.Format(time.RFC3339), tickID)
	if err != nil {
		log.Printf("ERROR updating tick %s to running: %v", tickID, err)
	}
	// Also set last_tick_started on the project so cooldown tracking works.
	// S-GAP-001: a successful spawn resets the consecutive-failure backoff
	// counter (atomically with the last_tick_started write).
	_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ?, consecutive_failures = 0 WHERE name = ?`,
		st.Started.Format(time.RFC3339), project.Name)

	log.Printf("SPAWN: %s tick=%s pid=%d workdir=%s", project.Name, tickID, st.PID, project.Workdir)
	return st, nil
}

// SpawnedTick represents a running foreman process.
type SpawnedTick struct {
	TickID     string
	Project    string
	PID        int
	Started    time.Time
	SessionID  string
	Output     bytes.Buffer // full stdout for delivery after completion
	Deliver    string       // delivery target (telegram:chat_id:thread_id)
	cmd        *exec.Cmd
	stdout     interface{ Close() error }
	stderr     interface{ Close() error }
	spawner    *Spawner
	scanCancel context.CancelFunc
	mu         sync.Mutex

	// stopHeartbeat is closed by Wait() to stop the tick-row heartbeat
	// goroutine started in Spawn() (S-GAP-003). Nil for gateway spawns —
	// their heartbeat is stopped inside Spawn() when the request returns.
	stopHeartbeat chan<- struct{}

	// Git baseline captured at spawn (exec path only) so Wait() can measure
	// the commits/files the foreman produced during this tick. preCommits < 0
	// means the workdir was not a usable git repo at spawn.
	preHead    string
	preCommits int

	// completed is true for gateway-spawned ticks that finished in Spawn().
	completed  bool
	completeAt time.Time

	// gwFailErr is set (gateway path only) when the response failed the
	// SCHED-GAP-079 completion gate: an explicit failure status
	// (failed / session_persistence_failed / error / cancelled / timeout)
	// or empty-output-AND-empty-session. Wait() yields TickFailed with
	// this text so slot_pool's lifecycle.Complete records status=failed,
	// outcome=failed and the gateway's error text in the error column —
	// never completed/committed.
	gwFailErr string

	// Trigger records how this tick was launched: "command" for custom
	// command/script spawns (project.Command), "prompt" for LLM prompt
	// spawns (gateway or hermes-chat exec fallback). Carried into the
	// delivered report's top/bottom lines so the thread shows whether
	// the run was command- or prompt-based (Bane 2026-08-27).
	Trigger string

	// SCHED-GAP-029: real usage + context for outcome metrics.
	usage Usage  // gateway response token usage (gateway path only)
	model string // model used for this tick (for cost lookup)
	// TASK-ROUTER-002: provider used for this tick — the completion path
	// records (provider, model) failures into the circuit state on
	// timeout/failed outcomes.
	provider string // provider used for this tick (for circuit recording)
	// SCHED-GAP-078: the task router's PUBLIC price for the (provider,
	// model) pair that actually ran. known=false (no router / pair not
	// priced) falls back to the static maps in computeCostUSD.
	rate     routerRate
	workdir  string    // project workdir for git commit/file counting
	reqStart time.Time // request start time (before SendResponse) for git window
}

// Wait blocks until the process exits and returns the outcome.
// For gateway-completed ticks (HTTP spawn), returns immediately.
func (st *SpawnedTick) Wait() TickOutcome {
	defer func() {
		// S-GAP-003: stop the tick-row heartbeat goroutine started in Spawn()
		// — it must never outlive the process. Nil for gateway spawns (their
		// heartbeat was stopped inside Spawn() when the request returned).
		if st.stopHeartbeat != nil {
			close(st.stopHeartbeat)
			st.stopHeartbeat = nil
		}
		st.spawner.mu.Lock()
		delete(st.spawner.active, st.TickID)
		st.spawner.mu.Unlock()
	}()

	// SCHED-GAP-079: gateway ticks whose response failed the completion
	// gate (explicit failure status, or empty-output-AND-empty-session)
	// carry gwFailErr. Yield TickFailed so the EXISTING lifecycle.Complete
	// path in slot_pool persists status=failed / outcome=failed /
	// error=<gateway text> — never completed/committed. The deferred
	// cleanup (heartbeat stop, active-map delete) still runs via defer.
	if st.gwFailErr != "" {
		tokensIn := st.usage.InputTokens
		tokensOut := st.usage.OutputTokens
		cost := computeCostUSD(st.provider, st.model, st.rate, tokensIn, tokensOut)
		log.Printf("TICK: %s %s → %s (%v): %s",
			st.Project, st.TickID, TickFailed,
			st.completeAt.Sub(st.Started).Round(time.Second), st.gwFailErr)
		return TickOutcome{
			TickID:    st.TickID,
			Project:   st.Project,
			SessionID: st.SessionID,
			Started:   st.Started,
			Finished:  st.completeAt,
			Status:    TickFailed,
			ExitCode:  -1,
			Error:     st.gwFailErr,
			Duration:  st.completeAt.Sub(st.Started),
			TokensIn:  tokensIn,
			TokensOut: tokensOut,
			CostUSD:   cost,
		}
	}

	// Gateway-spawned ticks are already complete — return immediately.
	// SCHED-GAP-029: populate real tokens/cost/commits/files from gateway
	// usage + git. Previously every gateway tick returned zero metrics.
	// SCHED-GAP-078: cost uses the router's PUBLIC price for the
	// (provider, model) pair that actually ran.
	if st.completed {
		tokensIn := st.usage.InputTokens
		tokensOut := st.usage.OutputTokens
		cost := computeCostUSD(st.provider, st.model, st.rate, tokensIn, tokensOut)
		commits, files := countGitChanges(st.workdir, st.reqStart, st.completeAt)
		// SCHED-GAP-085: closure-evidence gate. A row closed within this
		// tick's window with NO reasoning/commit_hash/worker_summary rejects
		// the tick (lifecycle.Complete records status=failed/outcome=failed).
		// Pre-existing violations are flagged (WARN + HIGH board_closure
		// event) and do NOT fail the tick. Runs AFTER countGitChanges so the
		// git metrics are still recorded on rejection; the gwFailErr early
		// return above keeps SCHED-GAP-079 semantics unchanged.
		if err := st.boardClosureGate(st.reqStart, st.completeAt); err != nil {
			log.Printf("TICK: %s %s → %s (%v): %s",
				st.Project, st.TickID, TickFailed,
				st.completeAt.Sub(st.Started).Round(time.Second), err)
			return TickOutcome{
				TickID:       st.TickID,
				Project:      st.Project,
				SessionID:    st.SessionID,
				Started:      st.Started,
				Finished:     st.completeAt,
				Status:       TickFailed,
				ExitCode:     -1,
				Error:        err.Error(),
				Duration:     st.completeAt.Sub(st.Started),
				TokensIn:     tokensIn,
				TokensOut:    tokensOut,
				CostUSD:      cost,
				Commits:      commits,
				FilesChanged: files,
			}
		}
		log.Printf("TICK: %s %s → %s (%v) %s",
			st.Project, st.TickID, TickCompleted,
			st.completeAt.Sub(st.Started).Round(time.Second),
			formatCostSummary(st.provider, st.model, tokensIn, tokensOut, cost, commits, files))
		return TickOutcome{
			TickID:       st.TickID,
			Project:      st.Project,
			SessionID:    st.SessionID,
			Started:      st.Started,
			Finished:     st.completeAt,
			Status:       TickCompleted,
			Duration:     st.completeAt.Sub(st.Started),
			TokensIn:     tokensIn,
			TokensOut:    tokensOut,
			CostUSD:      cost,
			Commits:      commits,
			FilesChanged: files,
		}
	}

	defer st.closePipes()
	if st.scanCancel != nil {
		defer st.scanCancel()
	}

	timer := time.AfterFunc(st.spawner.timeout, func() {
		if st.cmd.Process != nil {
			// Each scheduler-owned worker has its own process group. Killing the
			// group prevents shells, Hermes workers, and test runners from
			// surviving after the tick is marked timed out.
			_ = syscall.Kill(-st.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer timer.Stop()

	err := st.cmd.Wait()
	finished := time.Now()

	outcome := TickOutcome{
		TickID:    st.TickID,
		Project:   st.Project,
		SessionID: st.SessionID,
		Started:   st.Started,
		Finished:  finished,
	}

	if err != nil {
		if strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "killed") {
			outcome.Status = TickTimeout
		} else {
			outcome.Status = TickFailed
			outcome.Error = err.Error()
		}
	} else {
		outcome.Status = TickCompleted
	}

	outcome.ExitCode = st.cmd.ProcessState.ExitCode()
	outcome.Duration = finished.Sub(st.Started)

	// TASK-ROUTER-002: a tick that ran with a (provider, model) pair and
	// ended TIMED OUT or FAILED records the failure into the circuit
	// state — the breaker cooldown then cools the pair across ticks
	// (router_spawn.py excludes open pairs from future heads). Completed
	// ticks already recorded success at spawn time. Empty pairs
	// (custom-command spawns) are skipped by the recorder.
	if (outcome.Status == TickTimeout || outcome.Status == TickFailed) && st.spawner != nil {
		if st.provider != "" || st.model != "" {
			st.spawner.recordCircuitFailure(st.provider, st.model, "tick "+string(outcome.Status))
		}
	}

	// Cost: prefer REAL per-tick cost from the foreman's Hermes state.db
	// (session_model_usage.estimated/actual_cost_usd overlapping this tick's
	// window). Falls back to the flat estimate when telemetry is unavailable.
	// Populated on completed AND timed-out ticks: a timeout runs the full
	// window (killed at the cap), so it consumes a full tick's tokens and has a
	// real cost. Failed ticks that exit early consumed fewer and stay near 0.
	if outcome.Status == TickCompleted || outcome.Status == TickTimeout {
		workdir := ""
		if st.cmd != nil && st.cmd.Dir != "" {
			workdir = st.cmd.Dir
		}
		cost, isReal := resolveRealTickCost(st.spawner.foremanHome, workdir, st.Project, st.Started, finished)
		outcome.CostUSD = cost
		if !isReal {
			// Still record the estimated token counts so aggregation works
			// even when telemetry is missing. SCHED-GAP-078: price the
			// estimate with the router's PUBLIC rate for the pair that ran
			// (then the static maps) instead of the flat constants.
			tin, tout, _ := estimateTickCost()
			outcome.TokensIn = tin
			outcome.TokensOut = tout
			outcome.CostUSD = computeCostUSD(st.provider, st.model, st.rate, tin, tout)
		} else {
			outcome.TokensIn = 0
			outcome.TokensOut = 0
		}
		// Measure real git work the foreman produced this tick (exec path only —
		// gateway spawns have no process/repo baseline). Best-effort: a non-git
		// or unreadable workdir leaves commits/files at 0.
		if st.preCommits >= 0 && st.cmd != nil && st.cmd.Dir != "" {
			outcome.Commits, outcome.FilesChanged = gitWorkDelta(st.cmd.Dir, st.preHead, st.preCommits)
		}
	}

	// SCHED-GAP-085: closure-evidence gate on the EXEC completion path. Runs
	// after the git work was measured and only when the tick otherwise
	// completes — a row closed within this tick's window with NO
	// reasoning/commit_hash/worker_summary turns the completed outcome into a
	// failed one (lifecycle.Complete records status=failed/outcome=failed).
	// Pre-existing violations are flagged inside the gate (WARN + HIGH
	// board_closure event) and leave the outcome untouched. gwFailErr early
	// return above is SCHED-GAP-079's gate — unchanged.
	if outcome.Status == TickCompleted {
		windowEnd := st.completeAt
		if windowEnd.IsZero() {
			windowEnd = finished
		}
		if err := st.boardClosureGate(st.Started, windowEnd); err != nil {
			outcome.Status = TickFailed
			outcome.Error = err.Error()
			outcome.ExitCode = -1
		}
	}

	log.Printf("TICK: %s %s → %s (%v)", st.Project, st.TickID, outcome.Status, outcome.Duration.Round(time.Second))
	return outcome
}

// closePipes closes the process pipes (exec path only).
func (st *SpawnedTick) closePipes() {
	if st.stdout != nil {
		_ = st.stdout.Close()
	}
	if st.stderr != nil {
		_ = st.stderr.Close()
	}
}

// boardClosureGate runs the SCHED-GAP-085 closure-evidence gate on the tick's
// board: rows CLOSED within the tick window [windowStart, windowEnd] (i.e.
// completed_at inside the window) without any of reasoning/commit_hash/
// worker_summary evidence reject the tick. Pre-existing violations
// (completed_at before windowStart) are flagged via WARN log + HIGH
// board_closure event but do NOT fail the tick. Projects without a board file
// (findBoardFile false) are a no-op. Returns a non-empty error when the tick
// must be rejected (status=failed / outcome=failed via lifecycle.Complete).
func (st *SpawnedTick) boardClosureGate(windowStart, windowEnd time.Time) error {
	if st == nil || st.workdir == "" {
		return nil
	}
	boardPath, ok := findBoardFile(st.workdir)
	if !ok {
		return nil // no board file — gate is a no-op
	}
	violations, err := BoardClosureViolations(boardPath)
	if err != nil {
		// Unreadable board: never fail the tick on a side-file read error.
		log.Printf("WARN [board_closure]: cannot read board %s: %v", boardPath, err)
		return nil
	}
	if len(violations) == 0 {
		return nil
	}

	var rejected []ClosureViolation
	var flagged []ClosureViolation
	for _, v := range violations {
		closedAt, ok := parseBoardCompletedAt(v.CompletedAt)
		if !ok || closedAt.Before(windowStart) || closedAt.After(windowEnd) {
			// Unparseable or pre-existing (closed before this tick's window) —
			// flag, never reject: the tick did not cause it.
			flagged = append(flagged, v)
			continue
		}
		rejected = append(rejected, v)
	}

	if len(flagged) > 0 {
		var names []string
		for _, v := range flagged {
			names = append(names, v.ID+"(missing "+strings.Join(v.MissingFields, ",")+")")
		}
		log.Printf("WARN [board_closure]: pre-existing closure-evidence violations on %s (tick %s closed %d row(s) in-window): %s",
			st.Project, st.TickID, len(rejected), strings.Join(names, ", "))
		if st.spawner != nil && st.spawner.events != nil {
			details := make([]map[string]any, 0, len(flagged))
			for _, v := range flagged {
				details = append(details, map[string]any{
					"id":             v.ID,
					"missing_fields": v.MissingFields,
					"completed_at":   v.CompletedAt,
				})
			}
			st.spawner.events.Emit(context.Background(), SeverityHigh, "board_closure",
				"pre-existing board closure-evidence violations", map[string]any{
					"project":    st.Project,
					"tick_id":    st.TickID,
					"violations": details,
				})
		}
	}

	if len(rejected) == 0 {
		return nil
	}
	var names []string
	for _, v := range rejected {
		names = append(names, v.ID+"(missing "+strings.Join(v.MissingFields, ",")+")")
	}
	return fmt.Errorf("board closure-evidence gate: %d row(s) closed within this tick's window without evidence: %s",
		len(rejected), strings.Join(names, ", "))
}

// parseBoardCompletedAt parses the completed_at formats found on this board:
// RFC3339 ("2026-08-14T04:55:29Z", "2026-08-30T05:55:00+00:00") and the
// appender's naive UTC stamps ("2026-08-28 00:15:20", with optional
// fractional seconds). Naive stamps are read as UTC — the appender writes
// UTC (event timestamps match tick IDs, which are UTC by convention).
func parseBoardCompletedAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func splitCommand(cmd string) []string {
	// Simple split for shell commands. Does basic quote handling.
	var parts []string
	var current string
	inQuote := false
	for _, c := range cmd {
		switch c {
		case '"':
			inQuote = !inQuote
		case ' ':
			if inQuote {
				current += string(c)
			} else if current != "" {
				parts = append(parts, current)
				current = ""
			}
		default:
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
