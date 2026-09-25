package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof" // registers handlers on DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coding-hermes/scheduler/internal/agentlog"
	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/sync"
	"github.com/coding-hermes/scheduler/internal/version"
)

func main() {
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"), "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:9090", "HTTP listen address")
	// SCHED-GAP-1607: public dashboard origin for link-mode tick-report
	// delivery (deliver_mode=link). Empty = not configured → link mode
	// degrades to full. Also SCHEDULER_PUBLIC_URL env override below.
	publicURL := flag.String("public-url", "", "Public base URL of the dashboard (e.g. https://sched.example.com) used to build tick-report permalinks for deliver_mode=link; empty = link mode falls back to full. Env: SCHEDULER_PUBLIC_URL")
	minInterval := flag.Duration("min-interval", 30*time.Second, "Fastest tick interval")
	maxInterval := flag.Duration("max-interval", 24*time.Hour, "Slowest tick interval")
	numLevels := flag.Int("num-levels", 10, "Number of priority levels")
	weightBudget := flag.Int("budget", 100, "Weight budget")
	maxConcurrent := flag.Int("max-concurrent", 10, "Max concurrent foremen")
	namespaceMode := flag.Bool("namespace-mode", false, "Enable multi-namespace scheduling")
	tickTimeout := flag.Duration("tick-timeout", 7200*time.Second, "Maximum tick duration before timeout (2h)")
	// SCHED-GAP-1575-B: per-request deadline for the heavy DB-backed read
	// surfaces of the HTTP API (/api/v1/status, /projects, /namespaces,
	// /ticks). The daemon opens ONE serialized SQLite connection, so a single
	// stalled helper previously hung the whole handler with no bytes and no
	// status code (measured 2026-09-24: /api/v1/status timed out at 8s while
	// /live answered in 0.6ms). A step that exceeds this deadline now answers
	// 504 naming the stalled helper. 0 or negative = keep the 5s default.
	apiReadTimeout := flag.Duration("api-read-timeout", 5*time.Second, "Per-request deadline for the heavy read API surfaces (/api/v1/status, /projects, /namespaces, /ticks); a stalled DB helper returns 504 naming the helper instead of hanging the handler (SCHED-GAP-1575-B; <= 0 = keep the 5s default)")
	// SCHED-GAP-117: shorter per-turn deadline for a single gateway
	// /v1/responses POST. A hung POST previously consumed the whole
	// --tick-timeout slot undetected; the turn deadline trips first and
	// fails the tick as "stalled". Effective POST deadline is
	// min(--gateway-response-timeout, --tick-timeout).
	gatewayResponseTimeout := flag.Duration("gateway-response-timeout", 30*time.Minute, "Per-turn deadline for a gateway /v1/responses POST; a stalled POST fails the tick before --tick-timeout (SCHED-GAP-117; 0 disables)")
	// ADV-R08/G3: slot-wait patience — how long a spawn waits for a free
	// slot before the project is dropped (the drop emits a MEDIUM
	// slot_pool event). Default keeps the historical hardcoded 5m window.
	slotPatience := flag.Duration("slot-patience", 5*time.Minute, "How long a tick waits for a free slot before being dropped; the drop emits an event (ADV-R08/G3)")
	tasksPacing := flag.Duration("tasks-pacing", 60*time.Second, "Minimum post-tick spacing before a tasks-mode project re-admits, +up to 20% jitter (SCHED-GAP-136); 0 = disabled. Library default 0; the fleet binary ships 60s. Composes with (never replaces) failure backoff")
	loadGateThreshold := flag.Float64("load-gate-threshold", 0, "Defer new spawns while the 1-minute load average is at or above this value (SCHED-GAP-125); 0 = disabled. Work is deferred, not dropped — it runs once load drops. Namespaces opt out via load_gate='off'")
	spawnMemLimitMB := flag.Int64("spawn-mem-limit-mb", 0, "Per-spawn RLIMIT_AS memory cap in MiB applied to spawned foreman processes (ADV-R11, GAP-048 cure); 0 = off (default). NOT an admission gate — every selected project still spawns; the cap constrains the spawned process's resources at spawn time (inherited by its workers). Best-effort: a failed cap WARNs and the spawn continues")
	meteredBudgetEnabled := false
	testVerifyFlag := flag.Int("test-verify", 0, "Run N-cycle correctness verification and exit")
	verifyBoardPath := flag.String("verify-board", "", "Check board closure-evidence violations (SCHED-GAP-085): exit 0 when no closed row is missing all of reasoning/commit_hash/worker_summary, exit 1 when any")
	reapThreshold := flag.Duration("session-reap-threshold", database.DefaultZombieReapThreshold, "Zombie session reaper age threshold (SCHED-GAP-089; default 24h)")
	reapSessionsOnly := flag.Bool("reap-sessions-only", false, "Reap zombie sessions in --db once and exit (SCHED-GAP-089)")
	duckbrainNS := flag.String("duckbrain-ns", "scheduler", "DuckBrain namespace for sync (Bane 2026-08-27: sync consolidated under the scheduler namespace)")
	duckbrainURL := flag.String("duckbrain-url", "http://localhost:3000", "DuckBrain HTTP server URL")
	duckbrainInterval := flag.Duration("duckbrain-interval", 5*time.Minute, "DuckBrain sync interval (spool replay cadence)")
	simulate := flag.Bool("simulate", false, "Run in dry-run/simulation mode (no real spawning)")
	simSuccess := flag.Float64("sim-success", 0.85, "Simulated success rate (0.0-1.0)")
	simIdle := flag.Float64("sim-idle", 0.0, "Fraction of completed sim ticks with zero commits (0-1) — exercises adaptive-cooldown slow-down in dry-runs")
	simCount := flag.Int("sim-count", 0, "Generate N simulated ticks and exit (0 = run loop)")
	gatewayURL := flag.String("gateway-url", "http://127.0.0.1:8642", "Hermes gateway API URL (empty = use exec.Command)")
	gatewayKey := flag.String("gateway-key", os.Getenv("API_SERVER_KEY"), "Hermes gateway API key")
	modelRatesFile := flag.String("model-rates-file", os.Getenv("SCHEDULER_MODEL_RATES_FILE"), "JSON price-sticker file applied over the builtin model rates at startup (ADV-R09/G8): {as_of, models:{name:{in_per_m,out_per_m}}, providers:{...}} — refresh stickers without a rebuild")
	noExecFallback := flag.Bool("no-exec-fallback", true, "Disable exec.Command fallback when gateway fails (default true for safety)")
	foremanHome := flag.String("foreman-home", os.ExpandEnv("$HOME/.hermes/foreman"), "HERMES_HOME path for foreman sessions")
	simSetup := flag.Bool("sim-setup", false, "Create test fixture with 13 dry-run projects (12 enabled + 1 disabled)")
	simTicks := flag.Int("sim-ticks", 10, "Number of evaluation ticks to run in sim-setup mode")
	configFile := flag.String("config", "", "Path to TOML fleet config file")
	failureWindow := flag.Int("failure-window", 100, "Number of recent ticks per project for /api/v1/status per-project failure-rate breakdown")
	autoDisableRate := flag.Float64("auto-disable-failure-rate", 0, "Per-project failure-rate threshold (0.0–1.0) for auto-disable; 0 = off (SCHED-GAP-018)")
	autoDisableWindow := flag.Int("auto-disable-window", 100, "Ticks per project over which auto-disable failure rate is computed")
	autoDisableMinTicks := flag.Int("auto-disable-min-ticks", 50, "Minimum ticks in window before auto-disable can fire")
	groupsFile := flag.String("groups-file", "", "JSONL file for deploy groups (default <db dir>/groups.jsonl when blocks store enabled; empty = default paths)")
	templatesFile := flag.String("templates-file", "", "JSONL file for deploy templates (default <db dir>/templates.jsonl when blocks store enabled; empty = default paths)")
	logFile := flag.String("log-file", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.log"), "Path to append structured tick logs (JSON lines); empty disables")
	showConfigFlag := flag.Bool("show-config", false, "Print resolved config (CLI + env) as TOML and exit")
	schemaFlag := flag.Bool("schema", false, "Output JSON Schema for schedulerd.toml and exit")
	showVersion := flag.Bool("version", false, "Print version/build info and exit")
	flag.Parse()

	// ADV-R09/G8 — budget provenance: which config layer owned the effective
	// --budget value. flag.Visit reports ONLY explicitly-set flags; the env
	// and TOML layers below claim provenance when they apply. The chain is
	// flag > env > TOML > flag-default; no layer setting it is itself the
	// documented unset behavior: the fleet schedules against 100 weight
	// units (admission currency, NOT dollars).
	budgetSource := "flag-default"
	meteredBudgetSource := "default"
	// SCHED-GAP-1602: the operator credential — env/TOML ONLY, never a CLI
	// flag (GAP-038: credentials in argv are visible in ps and shell
	// history). Declared before the env-override block so --show-config and
	// --schema see the resolved value like every other knob. Basic mode
	// (operator_user + operator_password) resolves from TOML only.
	var operatorToken, operatorUser, operatorPassword string
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "budget" {
			budgetSource = "flag"
		}
	})

	// --version exits before any state is touched (DB, gateway, ports).
	if *showVersion {
		fmt.Printf("schedulerd %s (commit: %s, built: %s)\n",
			version.Current(), version.CurrentCommit(), version.CurrentBuildDate())
		return
	}

	// Resolve SCHEDULER_* env-var overrides BEFORE the --schema/--show-config
	// early exits so those commands print EFFECTIVE values (DOGFOOD-012).
	// Previously this block ran after them and --show-config never saw env
	// overrides. Runtime daemon behavior is unchanged — the same resolution
	// happened before the loop was created.
	if os.Getenv("SCHEDULER_NAMESPACE_MODE") == "true" {
		*namespaceMode = true
	}

	// SCHED-GAP-018: auto-disable config — SCHEDULER_* env vars override CLI
	// flag defaults (Layer 2 > Layer 3 per the three-layer model). We resolve
	// before the loop is created.
	if v := os.Getenv("SCHEDULER_FAILURE_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*failureWindow = n
		}
	}
	// ADV-R09/G8: weight budget env override (SCHEDULER_BUDGET), same
	// pattern — applies only when no explicit --budget flag was passed
	// (flag.Visit captured that above); a positive parseable int wins and
	// claims provenance.
	if v := os.Getenv("SCHEDULER_BUDGET"); v != "" && budgetSource == "flag-default" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*weightBudget = n
			budgetSource = "env"
		}
	}
	// SCHED-GAP-127: opt-in metered USD gate. A valid env value outranks
	// the lower-precedence TOML layer, including an explicit false.
	if v := os.Getenv("SCHEDULER_METERED_BUDGET_ENABLED"); v != "" && meteredBudgetSource == "default" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			meteredBudgetEnabled = enabled
			meteredBudgetSource = "env"
		} else {
			log.Printf("WARN: SCHEDULER_METERED_BUDGET_ENABLED=%q invalid — metered budget stays %v", v, meteredBudgetEnabled)
		}
	}
	// SCHED-GAP-117: per-turn gateway deadline env override, resolved BEFORE
	// --show-config/--schema so those print EFFECTIVE values (same pattern as
	// the auto-disable knobs above). Only a positive parseable duration wins.
	if v := os.Getenv("SCHEDULER_GATEWAY_RESPONSE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			*gatewayResponseTimeout = d
		}
	}
	// ADV-R08/G3: slot-wait patience env override — same pattern. Only a
	// positive parseable duration wins (the drop always exists); an
	// invalid value WARNs and keeps the current value.
	if v := os.Getenv("SCHEDULER_SLOT_PATIENCE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			*slotPatience = d
		} else {
			log.Printf("WARN: SCHEDULER_SLOT_PATIENCE=%q invalid — using %v", v, *slotPatience)
		}
	}
	// SCHED-GAP-136: tasks-mode post-tick pacing env override — same
	// pattern. 0 or invalid keeps the flag value (60s in the fleet binary;
	// an explicit SCHEDULER_TASKS_PACING=0 DISABLES pacing — the escape
	// hatch for a lane that must tick continuously).
	if v := os.Getenv("SCHEDULER_TASKS_PACING"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			*tasksPacing = d
		} else {
			log.Printf("WARN: SCHEDULER_TASKS_PACING=%q invalid — using %v", v, *tasksPacing)
		}
	}
	// SCHED-GAP-1575-B: heavy-read API deadline env override — same pattern.
	// Only a positive parseable duration wins; an invalid value WARNs and
	// keeps the current value (the deadline must never silently vanish).
	if v := os.Getenv("SCHEDULER_API_READ_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			*apiReadTimeout = d
		} else {
			log.Printf("WARN: SCHEDULER_API_READ_TIMEOUT=%q invalid — using %v", v, *apiReadTimeout)
		}
	}
	// SCHED-GAP-1602: operator credential env override. Trimmed — a
	// whitespace-only value is treated as unset so the daemon cannot be
	// "armed" with an accidental space (fail-closed either way: with no
	// credential the mutation gate answers 503 on every mutating route).
	// Basic mode has no env layer by design: the password would sit in a
	// dotfile-read env file next to the TOML it exists to complement.
	operatorToken = strings.TrimSpace(os.Getenv("SCHEDULER_OPERATOR_TOKEN"))
	// SCHED-GAP-125: load-gate threshold env override — same pattern. Only a
	// positive parseable float enables the gate; 0/negative/invalid keeps it
	// off. The gate defers spawns while 1m loadavg >= threshold.
	if v := os.Getenv("SCHEDULER_LOAD_GATE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			*loadGateThreshold = f
		} else {
			log.Printf("WARN: SCHEDULER_LOAD_GATE_THRESHOLD=%q invalid — gate stays %v", v, *loadGateThreshold)
		}
	}
	// ADV-R11: per-spawn memory cap env override — same pattern. A positive
	// parseable int arms the RLIMIT_AS cap; 0 keeps it off. Invalid values
	// WARN and keep the current value (the cap must never silently change).
	if v := os.Getenv("SCHEDULER_SPAWN_MEM_LIMIT_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			*spawnMemLimitMB = n
		} else {
			log.Printf("WARN: SCHEDULER_SPAWN_MEM_LIMIT_MB=%q invalid — limit stays %d MiB", v, *spawnMemLimitMB)
		}
	}
	if v := os.Getenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			*autoDisableRate = f
		}
	}
	// SCHED-GAP-1607: public dashboard base URL for link-mode delivery.
	// Env override of --public-url (env wins, matching the other SCHEDULER_*
	// knobs); empty stays empty (link mode degrades to full at delivery).
	if v := os.Getenv("SCHEDULER_PUBLIC_URL"); v != "" {
		*publicURL = v
	}
	if v := os.Getenv("SCHEDULER_AUTO_DISABLE_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*autoDisableWindow = n
		}
	}
	if v := os.Getenv("SCHEDULER_AUTO_DISABLE_MIN_TICKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*autoDisableMinTicks = n
		}
	}

	if *schemaFlag {
		printSchema()
		return
	}
	if *showConfigFlag {
		printConfig(*configFile, *dbPath, *listen, *logFile,
			*minInterval, *maxInterval,
			*numLevels, *weightBudget, *maxConcurrent, *namespaceMode,
			*tickTimeout, *gatewayResponseTimeout, *slotPatience, *tasksPacing,
			*gatewayURL, *gatewayKey, *foremanHome, *noExecFallback,
			*duckbrainNS, *duckbrainURL,
			*autoDisableRate, *autoDisableWindow, *autoDisableMinTicks, *failureWindow,
			*spawnMemLimitMB,
			*loadGateThreshold, *modelRatesFile)
		return
	}

	// ── SCHED-GAP-169: the process clock ──
	// ONE clock for the whole daemon. Every component below is wired to this
	// value, so there is a single choke point for all clock reads and waits.
	//
	// SCHEDULER_TIME_MODE=sim swaps in the test-time simulator (virtual time at
	// SCHEDULER_TIME_SCALE): virtual now advances the full duration while the
	// real wait costs duration/scale, so a 2h tick timeout is reached in
	// seconds of real time. It is REFUSED unless --simulate is also set — a
	// stray environment variable must never put the live fleet on a fake clock.
	clk, clkErr := clock.FromEnv()
	if clkErr != nil {
		log.Fatalf("FATAL: %v", clkErr)
	}
	if _, isSim := clk.(*clock.SimClock); isSim && !*simulate {
		log.Fatalf("FATAL: %s=sim (%s) requires --simulate — refusing to run the real fleet on a simulated clock",
			clock.EnvMode, clock.Describe(clk))
	}
	log.Printf("TIME: clock %s", clock.Describe(clk))

	// ── Test-verify mode: run correctness checks and exit ──
	// Runs BEFORE the main database is opened: testVerify creates its own
	// temp DB, so requiring the production DB path here would break CI and
	// any host without ~/.hermes/coding-hermes/ (DOGFOOD-002 follow-up).
	if *testVerifyFlag > 0 {
		if err := testVerify(*testVerifyFlag, clk); err != nil {
			log.Fatalf("VERIFY FAILED: %v", err)
		}
		return
	}

	// ── Verify-board mode: check closure evidence on a tasks.jsonl board ──
	// SCHED-GAP-085 machine-checkable pass criterion. Runs BEFORE the main
	// database is opened (no DB needed — board JSONL only). Prints each
	// violation (id + missing fields), exits 0 when none / 1 when any.
	if *verifyBoardPath != "" {
		violations, err := scheduler.BoardClosureViolations(*verifyBoardPath)
		if err != nil {
			log.Fatalf("VERIFY-BOARD FAILED: %v", err)
		}
		for _, v := range violations {
			fmt.Printf("VIOLATION %s: missing [%s] completed_at=%s\n",
				v.ID, strings.Join(v.MissingFields, ", "), v.CompletedAt)
		}
		if len(violations) > 0 {
			log.Fatalf("VERIFY-BOARD FAILED: %d closure-evidence violation(s)", len(violations))
		}
		fmt.Println("VERIFY-BOARD OK: no closure-evidence violations")
		return
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Persist all logs to a file as well as stdout (system-plan-v2 §1.1).
	// Failures to open the log file are non-fatal — the daemon keeps running
	// on stdout only rather than crashing at boot.
	if *logFile != "" {
		lf, lfErr := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if lfErr != nil {
			log.Printf("WARN: cannot open log file %s (%v) — logging to stdout only", *logFile, lfErr)
		} else {
			log.SetOutput(io.MultiWriter(os.Stdout, lf))
			log.Printf("Log file: %s", *logFile)
		}
	}

	// Initialize database.
	db, err := database.InitDB(*dbPath)
	if err != nil {
		log.Fatalf("FATAL: database init: %v", err)
	}
	defer func() { _ = db.Close() }()
	log.Printf("Database: %s (WAL mode)", *dbPath)

	// SCHED-GAP-089: zombie session reaper. Closes api_server sessions whose
	// ended_at is still NULL past a configurable age threshold (default 24h).
	// Runs once at daemon startup; it is idempotent (safe to run repeatedly)
	// and best-effort — a failure is logged as a warning and must never block
	// boot. All work happens against the local SQLite --db via the existing
	// database layer; no external HTTP endpoints are contacted. In
	// --reap-sessions-only mode we reap once against --db and exit.
	if *reapSessionsOnly {
		if _, err := scheduler.ReapZombieSessions(context.Background(), db, *reapThreshold); err != nil {
			log.Fatalf("FATAL: zombie session reaper: %v", err)
		}
		return
	}
	if _, err := scheduler.ReapZombieSessions(context.Background(), db, *reapThreshold); err != nil {
		log.Printf("WARN: zombie session reaper at startup: %v", err)
	}

	// Declarative fleet seeding: if a fleet.toml was supplied, load and
	// apply it before any other subsystem touches the DB. Already-existing
	// rows are skipped (idempotent startup; create-only, never overwrite).
	if *configFile != "" {
		cfg, err := config.LoadFleetConfig(*configFile)
		if err != nil {
			log.Fatalf("FATAL: load fleet config: %v", err)
		}
		if err := config.ApplyFleetConfig(context.Background(), db, cfg); err != nil {
			log.Fatalf("FATAL: apply fleet config: %v", err)
		}
		log.Printf("Loaded %d projects, %d namespaces from %s",
			len(cfg.Projects), len(cfg.Namespaces), *configFile)
	}

	// ── Create the evaluation loop.
	// ADV-R09/G8: price-sticker refresh path. Applied BEFORE the loop is
	// created so every cost computation from the first tick uses the
	// refreshed maps. A bad file is a hard boot error — silently falling
	// back to stale builtin stickers while the operator believes their
	// refresh took effect would be exactly the G8 class of lie.
	if *modelRatesFile != "" {
		if err := scheduler.ApplyModelRatesFile(*modelRatesFile); err != nil {
			log.Fatalf("FATAL: %v", err)
		}
		log.Printf("Model rates: file %s applied (as-of %s)", *modelRatesFile, scheduler.PriceMapAsOf())
	}

	loop := scheduler.NewLoop(db, *minInterval, *maxInterval, *numLevels, *weightBudget, *maxConcurrent, *namespaceMode)
	// SCHED-GAP-169: install the process clock (wall clock by default) and
	// propagate it to every component the loop owns — spawner, slot pool,
	// lifecycle tracker, sim spawner. The API and MCP servers are constructed
	// below, after this call, and inherit the same clock through loop.Clock().
	loop.SetClock(clk)
	// Apply the tick timeout to the real spawner so Wait()/scanner cleanup use it.
	loop.SetTickTimeout(*tickTimeout)
	// SCHED-GAP-117: apply the per-turn gateway deadline AFTER the tick
	// timeout (the turn ctx is a child of the session ctx, so the effective
	// POST deadline is min(flag, tick timeout)). The flag var already
	// carries the env override resolved above; 0 disables the per-turn
	// deadline (POST runs on the tick deadline alone, pre-117 behavior).
	loop.SetGatewayResponseTimeout(*gatewayResponseTimeout)
	// ADV-R08/G3: apply the configured slot-wait patience; the drop emits
	// a MEDIUM slot_pool event. The flag var already carries the env
	// override resolved above; <= 0 keeps the 5m default in the pool.
	loop.SetSlotPatience(*slotPatience)
	// SCHED-GAP-136: tasks-mode post-tick pacing (fleet default 60s + up
	// to 20% jitter via the flag; library default 0). The flag var carries
	// the env override; TOML [scheduler] tasks_pacing applies below only
	// when the flag was never set (same precedence as the load gate).
	scheduler.SetTasksPacing(*tasksPacing)
	// SCHED-GAP-125: load-average gate (opt-in; 0 = disabled). The flag var
	// carries the env override; TOML [scheduler] load_gate_threshold applies
	// below only when the flag was never set (same precedence as budget).
	scheduler.SetLoadGateThreshold(*loadGateThreshold)
	// SCHED-GAP-170: load-scaled WAVE_BUDGET shares the load gate's threshold —
	// "for each load average point below 12 we can launch 1 worker, min 1, up to 12"
	// (Bane 2026-09-19). One knob, one number: when the gate is armed at N, waves
	// scale to N−load workers (floor 1); gate disabled → no load scaling at all.
	scheduler.SetWaveLoadCeiling(*loadGateThreshold)
	// ADV-R11: arm the per-spawn RLIMIT_AS cap (0 = off, the default — no
	// prlimit call at all). The flag var carries the env override; TOML
	// [scheduler] spawn_mem_limit_mb applies below only when the flag sat
	// at its 0 default (same precedence chain as the load gate).
	scheduler.SetSpawnMemLimitMB(*spawnMemLimitMB)
	loop.SetForemanHome(*foremanHome)
	loop.SetNoExecFallback(*noExecFallback)
	if *simulate {
		loop.SetSimulation(*simSuccess)
		loop.SetSimIdleRate(*simIdle)
	}

	// Load blackout windows from scheduler config (same TOML as fleet config).
	if *configFile != "" {
		rootCfg, err := config.LoadRootConfig(*configFile)
		if err == nil && len(rootCfg.Scheduler.BlackoutWindows) > 0 {
			loop.SetBlackoutWindows(rootCfg.Scheduler.BlackoutWindows)
			log.Printf("Blackout: loaded %d windows", len(rootCfg.Scheduler.BlackoutWindows))
		}
	}

	// SCHED-GAP-018: auto-disable + failure-rate window. Apply TOML values
	// first (Layer 1), then CLI/env overrides (already resolved above) win.
	if *configFile != "" {
		if rootCfg, err := config.LoadRootConfig(*configFile); err == nil {
			if rootCfg.Scheduler.AutoDisableWindow > 0 && *autoDisableWindow == 100 {
				*autoDisableWindow = rootCfg.Scheduler.AutoDisableWindow
			}
			if rootCfg.Scheduler.AutoDisableMinTicks > 0 && *autoDisableMinTicks == 50 {
				*autoDisableMinTicks = rootCfg.Scheduler.AutoDisableMinTicks
			}
			if rootCfg.Scheduler.FailureWindow > 0 && *failureWindow == 100 {
				*failureWindow = rootCfg.Scheduler.FailureWindow
			}
			if rootCfg.Scheduler.AutoDisableFailureRate > 0 && *autoDisableRate == 0 {
				*autoDisableRate = rootCfg.Scheduler.AutoDisableFailureRate
			}
			// ADV-R09/G8: TOML layer for the weight budget — the same
			// default-guard pattern (applied only when the flag sits at its
			// 100 default AND no env var set it), so CLI and env keep
			// precedence. Provenance: TOML wins only when no higher layer
			// configured the budget.
			if rootCfg.Scheduler.WeightBudget > 0 && *weightBudget == 100 && budgetSource == "flag-default" {
				*weightBudget = rootCfg.Scheduler.WeightBudget
				budgetSource = "toml"
			}
		}
		// SCHED-GAP-117: TOML layer for the per-turn gateway deadline —
		// applied only when the flag sits at its default (the same
		// default-guard pattern as the auto-disable knobs above), so CLI
		// and env keep precedence.
		if rootCfg, err := config.LoadRootConfig(*configFile); err == nil {
			if rootCfg.Scheduler.GatewayResponseTimeout != "" && *gatewayResponseTimeout == 30*time.Minute {
				if d, derr := time.ParseDuration(rootCfg.Scheduler.GatewayResponseTimeout); derr == nil && d >= 0 {
					*gatewayResponseTimeout = d
				} else {
					log.Printf("WARN: scheduler.gateway_response_timeout=%q invalid — using %v", rootCfg.Scheduler.GatewayResponseTimeout, *gatewayResponseTimeout)
				}
			}
		}
		// SCHED-GAP-1575-B: TOML layer for the heavy-read API deadline
		// ([api] read_timeout) — the same default-guard pattern (applied
		// only while the flag sits at its 5s default, so CLI and env keep
		// precedence). Only a strictly positive duration is accepted: the
		// deadline is the point of the row, and the flag layer treats <= 0
		// as "keep default", so a TOML "0s" would otherwise be a silent
		// no-op.
		if rootCfg, err := config.LoadRootConfig(*configFile); err == nil {
			if rootCfg.API.ReadTimeout != "" && *apiReadTimeout == 5*time.Second {
				if d, derr := time.ParseDuration(rootCfg.API.ReadTimeout); derr == nil && d > 0 {
					*apiReadTimeout = d
				} else {
					log.Printf("WARN: api.read_timeout=%q invalid — using %v", rootCfg.API.ReadTimeout, *apiReadTimeout)
				}
			}
			// SCHED-GAP-1602: TOML layer for the operator credentials —
			// token mode wins when both are configured (one credential to
			// rotate beats two); basic mode is the fallback when no token
			// exists anywhere (env or TOML). There is no flag layer by
			// design (GAP-038).
			if rootCfg.API.OperatorToken != "" && operatorToken == "" {
				operatorToken = strings.TrimSpace(rootCfg.API.OperatorToken)
			}
			if operatorToken == "" {
				if rootCfg.API.OperatorUser != "" {
					operatorUser = strings.TrimSpace(rootCfg.API.OperatorUser)
				}
				if rootCfg.API.OperatorPassword != "" {
					operatorPassword = rootCfg.API.OperatorPassword
				}
			}
		}
		// ADV-R08/G3: TOML layer for the slot-wait patience — same
		// default-guard pattern (applied only when the flag sits at its
		// 5m default, so CLI and env keep precedence). Only a strictly
		// positive duration is accepted: the drop always exists, and the
		// flag layer treats <= 0 as "keep default", so a TOML "0s" would
		// otherwise be a silent no-op.
		if rootCfg, err := config.LoadRootConfig(*configFile); err == nil {
			if rootCfg.Scheduler.SlotPatience != "" && *slotPatience == 5*time.Minute {
				if d, derr := time.ParseDuration(rootCfg.Scheduler.SlotPatience); derr == nil && d > 0 {
					*slotPatience = d
				} else {
					log.Printf("WARN: scheduler.slot_patience=%q invalid — using %v", rootCfg.Scheduler.SlotPatience, *slotPatience)
				}
			}
			// SCHED-GAP-125: TOML layer for the load gate — same
			// default-guard pattern (only when the flag sits at its 0
			// default, so CLI and env keep precedence). Positive values
			// enable the gate; a TOML 0 keeps it off.
			if rootCfg.Scheduler.LoadGateThreshold > 0 && *loadGateThreshold == 0 {
				*loadGateThreshold = rootCfg.Scheduler.LoadGateThreshold
				// SCHED-GAP-220: re-arm the gate AND the wave-load ceiling —
				// the wiring setters at main.go:373/378 ran BEFORE this TOML
				// block, so without the re-arm a [scheduler]
				// load_gate_threshold value armed nothing (the config
				// surface reported the TOML number while the runtime stayed
				// off). Same dead-TOML-layer shape as tasks_pacing below.
				scheduler.SetLoadGateThreshold(*loadGateThreshold)
				scheduler.SetWaveLoadCeiling(*loadGateThreshold)
				log.Printf("LOAD-GATE: enabled from config — threshold=%.1f (1m loadavg; namespaces may opt out via load_gate=\"off\")", *loadGateThreshold)
			}
			// SCHED-GAP-127: TOML is the lowest-precedence layer. Any valid
			// env value (including explicit false) blocks this opt-in.
			if rootCfg.Scheduler.MeteredBudgetEnabled && meteredBudgetSource == "default" {
				meteredBudgetEnabled = true
				meteredBudgetSource = "toml"
			}
			// SCHED-GAP-136: TOML layer for tasks-mode post-tick pacing —
			// same default-guard pattern (only when the flag sits at its
			// 60s default, so CLI and env keep precedence). A TOML 0s
			// DISABLES pacing explicitly.
			if rootCfg.Scheduler.TasksPacing != "" && *tasksPacing == 60*time.Second {
				if d, derr := time.ParseDuration(rootCfg.Scheduler.TasksPacing); derr == nil && d >= 0 {
					*tasksPacing = d
					// SCHED-GAP-220: re-arm the pacing floor. The wiring
					// call at main.go:369 ran BEFORE this TOML block, so a
					// [scheduler] tasks_pacing value was silently DEAD —
					// the config surface reported the TOML number while the
					// running scheduler kept pacing at the flag default.
					scheduler.SetTasksPacing(*tasksPacing)
					log.Printf("TASKS-PACING: set from config — %v (+up to 20%% jitter; 0 = disabled)", d)
				} else {
					log.Printf("WARN: scheduler.tasks_pacing=%q invalid — using %v", rootCfg.Scheduler.TasksPacing, *tasksPacing)
				}
			}
			// ADV-R11: TOML layer for the per-spawn memory cap — same
			// default-guard pattern (only when the flag sits at its 0
			// default, so CLI and env keep precedence). Positive values
			// arm the RLIMIT_AS cap; a TOML 0 keeps it off.
			if rootCfg.Scheduler.SpawnMemLimitMB > 0 && *spawnMemLimitMB == 0 {
				*spawnMemLimitMB = rootCfg.Scheduler.SpawnMemLimitMB
				scheduler.SetSpawnMemLimitMB(*spawnMemLimitMB)
				log.Printf("ADV-R11: spawn mem limit enabled from config — %d MiB RLIMIT_AS per spawned process", *spawnMemLimitMB)
			}
		}
	}
	// SCHED-GAP-127: arm the budget reader only after every config layer has
	// resolved. The dedicated foreman HERMES_HOME is the fleet cash ledger;
	// default false keeps the historical ticks.cost_usd query unchanged.
	scheduler.SetMeteredBudgetEnabled(meteredBudgetEnabled, filepath.Join(*foremanHome, "state.db"))
	log.Printf("METERED-BUDGET: enabled=%v source=%s", meteredBudgetEnabled, meteredBudgetSource)
	loop.SetAutoDisablePolicy(*autoDisableRate, *autoDisableWindow, *autoDisableMinTicks)
	if *autoDisableRate > 0 {
		log.Printf("AUTO-DISABLE: enabled — rate=%.2f window=%d min_ticks=%d", *autoDisableRate, *autoDisableWindow, *autoDisableMinTicks)
	} else {
		log.Printf("AUTO-DISABLE: off (rate=0)")
	}

	// Wire gateway HTTP client with retry (FEAT-003).
	if *gatewayURL != "" && *gatewayKey != "" {
		gwClient := scheduler.NewGatewayClient(*gatewayURL, *gatewayKey, *tickTimeout)
		gwClient.SetClock(clk)
		// Retry gateway connection with backoff — gateway may not be ready
		// when schedulerd starts (systemd ordering). Once connected, keep
		// retrying in the background if it ever drops.
		var gwConnected atomic.Bool
		for attempt := 0; attempt < 10; attempt++ {
			if err := gwClient.Ping(context.Background()); err != nil {
				wait := time.Duration(attempt+1) * 2 * time.Second
				log.Printf("WARN: gateway %s not reachable (attempt %d/10, retry in %v): %v", *gatewayURL, attempt+1, wait, err)
				clk.Sleep(wait)
			} else {
				loop.SetGatewayClient(gwClient)
				log.Printf("GATEWAY: connected to %s — using HTTP API instead of exec.Command", *gatewayURL)
				gwConnected.Store(true)
				break
			}
		}
		// GAP-048: when the gateway is unreachable at startup, the daemon
		// must honor --no-exec-fallback instead of silently degrading to
		// exec.Command spawns. With the flag set (the default), the spawner
		// has no HTTP client and drops ticks until the background reconnector
		// re-engages HTTP. Without the flag, fall back to exec as before.
		if !gwConnected.Load() {
			if *noExecFallback {
				log.Printf("WARN: gateway %s unreachable after 10 retries — exec fallback disabled (--no-exec-fallback), staying idle", *gatewayURL)
				loop.EmitHighEvent("gateway", "gateway unreachable at startup and exec fallback disabled — staying idle", map[string]any{
					"gateway_url":      *gatewayURL,
					"no_exec_fallback": true,
					"retries":          10,
				})
			} else {
				log.Printf("WARN: gateway %s unreachable after 10 retries — falling back to exec.Command", *gatewayURL)
			}
		}
		// Launch background reconnector (GAP-048 fix): keeps trying if the
		// gateway drops later AND when the daemon started in fallback mode.
		// The original code skipped reconnection when gwConnected==false,
		// so a fallback-start daemon never re-engaged HTTP even after the
		// gateway recovered. Now the reconnector pings every 60s regardless
		// and calls SetGatewayClient on success.
		runGatewayReconnector(context.Background(), gwClient, func() {
			loop.SetGatewayClient(gwClient)
		}, &gwConnected, *gatewayURL, clk)
	}
	// SCHED-GAP-170: the gateway-health admission gate's boot line. The install
	// path logs its own line, so this call exists for the shape where NO client
	// was ever installed — an empty --gateway-url/--gateway-key, or a gateway
	// that stayed unreachable through startup's retries. Those are exactly the
	// hosts where the gate is unarmed and every spawn goes straight to the
	// gateway, so the first seconds of scheduler.log must SAY so instead of
	// staying silent. No-op once a boot line has been logged (never two lines).
	scheduler.LogGatewayHealthGateBootState()

	// Simulation count mode: generate N ticks and exit.
	if *simCount > 0 {
		simCtx, simCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer simCancel()
		if err := loop.RunBulkSim(simCtx, *simCount); err != nil {
			log.Fatalf("FATAL: simulation: %v", err)
		}
		log.Printf("SIM: generated %d ticks", *simCount)
		return
	}

	// Simulation fixture mode: create test projects, run multi-tick, report.
	if *simSetup {
		fixture := scheduler.NewSimFixture(db)
		runner := scheduler.NewSimRunner(loop, fixture)
		runner.SetIdleRate(*simIdle)

		simCtx, simCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer simCancel()

		report, err := runner.RunMultiTick(simCtx, *simTicks)
		if err != nil {
			log.Fatalf("FATAL: sim setup: %v", err)
		}
		fmt.Print(report.Summary())
		return
	}

	// Create all components.
	// DuckBrain sync is created up-front so its health can be surfaced in
	// /api/v1/status (fallback state: reachable, spool depth). Its Run loop
	// starts later in background (see below).
	duckbrain := sync.NewDuckBrainSync(db, *duckbrainNS, *duckbrainURL)
	duckbrain.SetClock(clk)
	duckbrain.SetInterval(*duckbrainInterval)
	apiServer := api.NewServer(db, loop)
	apiServer.SetFailureWindow(*failureWindow)
	// SCHED-GAP-1602: arm the operator-credential gate. Token mode wins when
	// both credentials are configured; basic mode is the browser path. An
	// EMPTY credential set leaves authOff, which is fail-closed — every
	// mutating route answers 503 until the operator configures one. Reads
	// are never gated.
	apiServer.SetAuthConfig(api.ResolveAuthConfig(operatorToken, operatorUser, operatorPassword))
	// SCHED-GAP-1575-B: arm the heavy-read request deadline BEFORE the
	// resolved-config snapshot below, so the deadline actually enforced and
	// the value GET /api/v1/config reports are one number (both write
	// api_read_timeout from the same resolved *apiReadTimeout).
	apiServer.SetReadTimeout(*apiReadTimeout)
	// Deploy blocks (groups/templates) JSONL store: default paths next to the
	// DB when either flag is unset, overridable via --groups-file/--templates-file.
	// Deploy blocks store: resolve default JSONL paths next to the DB when flags are empty.
	groupsPath, templatesPath := *groupsFile, *templatesFile
	if groupsPath == "" {
		groupsPath = filepath.Join(filepath.Dir(*dbPath), "groups.jsonl")
	}
	if templatesPath == "" {
		templatesPath = filepath.Join(filepath.Dir(*dbPath), "templates.jsonl")
	}
	apiServer.SetBlocksStore(blocks.NewStore(groupsPath, templatesPath))
	// SCHED-GAP-219: the config_drift probe reads the seed file the daemon
	// actually booted from (the same --config resolution as above).
	apiServer.SetFleetTomlPath(*configFile)
	// SCHED-GAP-034: snapshot the ACTIVE three-layer config (TOML < env <
	// CLI) for GET /api/v1/config. By this point the flag vars carry the
	// resolved values — TOML overrides were applied above where flags sat
	// at their defaults, and SCHEDULER_* env overrides were applied earlier.
	// The gateway key is masked by SetResolvedConfig; it never reaches the wire.
	apiServer.SetResolvedConfig(api.ResolvedConfig{
		DBPath:                 *dbPath,
		Listen:                 *listen,
		PublicURL:              *publicURL,
		MinInterval:            minInterval.String(),
		MaxInterval:            maxInterval.String(),
		NumLevels:              *numLevels,
		WeightBudget:           *weightBudget,
		BudgetSource:           budgetSource,
		MaxConcurrent:          *maxConcurrent,
		TickTimeout:            tickTimeout.String(),
		GatewayResponseTimeout: gatewayResponseTimeout.String(),
		// SCHED-GAP-1575-B: the ARMED heavy-read request deadline, so
		// /api/v1/config reports the same duration the handlers enforce.
		APIReadTimeout:         apiReadTimeout.String(),
		SlotPatience:           slotPatience.String(),
		TasksPacing:            tasksPacing.String(),
		LoadGateThreshold:      *loadGateThreshold,
		SpawnMemLimitMB:        *spawnMemLimitMB,
		ModelRatesFile:         *modelRatesFile,
		NamespaceMode:          *namespaceMode,
		AutoDisableFailureRate: *autoDisableRate,
		AutoDisableWindow:      *autoDisableWindow,
		AutoDisableMinTicks:    *autoDisableMinTicks,
		FailureWindow:          *failureWindow,
		Gateway: api.GatewayConfigSnapshot{
			URL:            *gatewayURL,
			Key:            *gatewayKey,
			ForemanHome:    *foremanHome,
			NoExecFallback: *noExecFallback,
		},
		DuckBrain: api.DuckBrainConfigSnapshot{
			Namespace: *duckbrainNS,
			URL:       *duckbrainURL,
		},
	})
	apiServer.SetDuckBrainHealth(func() map[string]interface{} {
		h := duckbrain.Health()
		return map[string]interface{}{
			"reachable":            h.Reachable,
			"consecutive_failures": h.ConsecutiveErr,
			"last_error":           h.LastError,
			"last_ok_at":           h.LastOKAt,
			"spooled_pending":      h.Spooled,
			"base_url":             h.BaseURL,
			"interval":             h.Interval,
		}
	})
	mcpServer := mcp.NewServer(db, loop)
	// MCP serves the SAME JSONL block stores as the REST API (CTL-001):
	// both transports read/write one groups.jsonl + templates.jsonl.
	mcpServer.SetBlocksStore(blocks.NewStore(groupsPath, templatesPath))
	// SCHED-GAP-174: /queue and /api/v1/queue must answer with ONE urgency.
	// The dashboard ranks with the same calculator the API server builds from
	// the resolved interval range (SetResolvedConfig →
	// newUrgencyCalculatorFromConfig over the min-interval/max-interval/
	// num-levels values snapshotted above) — one formula, one source, so the
	// two surfaces cannot disagree. These are the same resolved values that
	// became cfg.MinInterval/cfg.MaxInterval/cfg.NumLevels; duration strings
	// round-trip exactly through ParseDuration, so both derivations are equal.
	dashGen := dashboard.NewGenerator(db, scheduler.NewUrgencyCalculator(*minInterval, *maxInterval, *numLevels), *gatewayURL)
	dashGen.SetClock(clk)
	dashGen.SetDuckBrainURL(*duckbrainURL)
	dashGen.SetSpawnCounts(loop.SpawnMethodCounts)
	// SCHED-GAP-1593: the tick drill-down resolves gateway_trace.session_id
	// into the agent's own state database to show what the agent generated.
	// Default path matches the Hermes state database; opened lazily and
	// read-only inside the dashboard (missing/unreachable degrades to an
	// explicit notice, never a failed render). The agentlog package holds
	// the read-only contract.
	agentStatePath := strings.TrimSpace(os.Getenv("SCHEDULER_AGENT_STATE_DB"))
	if agentStatePath == "" {
		agentStatePath = os.ExpandEnv("$HOME/.hermes/state.db")
	}
	dashGen.SetAgentStateDB(agentlog.NewReader(agentStatePath))
	// ADV-R09/G8: the dashboard renders the SAME effective budget the loop
	// was built with — never an independent literal.
	dashGen.SetWeightBudget(loop.WeightBudget())

	// Compose all handlers into one mux.
	mux := http.NewServeMux()

	// Dashboard at /. Supports the per-table server-side controls
	// (SCHED-GAP-1598): each of the four stacked tables takes its own
	// search / sort / page / size params — projects (q/project/outcome/
	// sort/dir/size/page), recent ticks (tq/tsort/tsize/tpage), namespaces
	// (nq/nsort/nsize/npage) and utilization history (hq/hsort/hsize/hpage).
	// All optional; unknown values fall back to the documented defaults.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		q := r.URL.Query()
		params := dashboard.ParseFleetTableQuery(q)
		if err := dashGen.GenerateParams(w, params); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// htmx partial: rendered for the main dashboard's tbody every 10s, and
	// on every autorefresh with the SAME query params the page was rendered
	// with (the tbody's hx-get carries the operator's current table state),
	// so a refresh preserves search / page / sort instead of resetting it
	// (SCHED-GAP-1598).
	mux.HandleFunc("GET /dashboard/partial", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateFleetTableParams(w, r.URL.Query()); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// Static assets bundled via Go embed (htmx.min.js).
	mux.HandleFunc("GET /static/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(dashGen.HTMXJS())
	})

	// Project detail page: /projects/{name}.
	mux.HandleFunc("GET /projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateProjectDetail(w, name); err != nil {
			if errors.Is(err, database.ErrProjectNotFound) {
				http.Error(w, "project not found: "+name, http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Queue page: /queue.
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateQueue(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Tick history page: /ticks (paginated, global tick log). Supports
	// server-side search/filter (SCHED-GAP-1593): q (substring against tick
	// id / project name), project, status, outcome — all optional, all
	// preserved across pagination links.
	// htmx polls return the #tick-history fragment only (HX-Request) — the
	// full page must never be swapped into its own poller.
	mux.HandleFunc("GET /ticks", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				page = n
			}
		}
		filter := database.TickFilter{
			Query:   r.URL.Query().Get("q"),
			Project: r.URL.Query().Get("project"),
			Status:  r.URL.Query().Get("status"),
			Outcome: r.URL.Query().Get("outcome"),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var err error
		if r.Header.Get("HX-Request") != "" {
			err = dashGen.GenerateTickHistoryPartial(w, page, filter)
		} else {
			err = dashGen.GenerateTickHistory(w, page, filter)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Tick detail page: /ticks/{id} (SCHED-GAP-1593) — the tick's own row,
	// its scheduler log events, and the agent's generated text resolved via
	// gateway_trace.session_id → the agent state database (read-only).
	mux.HandleFunc("GET /ticks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateTickDetail(w, id); err != nil {
			if errors.Is(err, database.ErrTickNotFound) {
				http.Error(w, "tick not found: "+id, http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Namespace view page: /namespaces/{id}.
	mux.HandleFunc("GET /namespaces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateNamespaceView(w, id); err != nil {
			if errors.Is(err, database.ErrNamespaceNotFound) {
				http.Error(w, "namespace not found: "+id, http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Health page: /health (daemon, db, gateway status).
	// htmx polls return the .cards fragment only (HX-Request) — the full page
	// must never be swapped into its own poller.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var err error
		if r.Header.Get("HX-Request") != "" {
			err = dashGen.GenerateHealthPartial(w)
		} else {
			err = dashGen.GenerateHealth(w)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Fleet Tape page: /tape (SCHED-GAP-1596) — the fleet as one instrument.
	// htmx polls return the board-rows fragment only (HX-Request) — the full
	// page must never be swapped into its own poller (same contract as
	// /ticks and /health). The browser JS drives updates from the SSE stream
	// (/api/v1/events/stream) with the shared auto-refresh cadence as
	// fallback; no poll against /api/v1/status or /api/v1/queue exists by
	// design (both wedge under load).
	mux.HandleFunc("GET /tape", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var err error
		if r.Header.Get("HX-Request") != "" {
			err = dashGen.GenerateTapeRows(w)
		} else {
			err = dashGen.GenerateTape(w)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// API at /api/
	mux.Handle("/api/", apiServer.Handler())

	// MCP at /mcp
	mux.Handle("/mcp", mcpServer.Handler())
	mux.Handle("/mcp/", mcpServer.Handler())

	// Start HTTP server with pprof on DefaultServeMux.
	// Custom mux handles API/MCP/dashboard. /debug/pprof/ falls through to DefaultServeMux.
	pprofMux := http.NewServeMux()
	pprofMux.Handle("/debug/pprof/", http.DefaultServeMux)
	pprofMux.Handle("/", mux)

	// SCHED-GAP-1607: arm the tick-permalink base for link-mode delivery.
	// Empty (unset) keeps link mode degrading to full at delivery time.
	scheduler.SetPublicBaseURL(*publicURL)
	if *publicURL != "" {
		log.Printf("DELIVER: public URL %s — deliver_mode=link builds tick permalinks against it", *publicURL)
	}

	server := &http.Server{
		Addr:    *listen,
		Handler: pprofMux,
	}
	go func() {
		log.Printf("HTTP: listening on %s", *listen)
		log.Printf("  Dashboard: http://%s/", *listen)
		log.Printf("  API:       http://%s/api/v1/health", *listen)
		log.Printf("  MCP:       http://%s/mcp", *listen)
		// SCHED-GAP-1602: one boot line that answers "are mutations gated?" —
		// NEVER prints the credential itself.
		switch {
		case operatorToken != "":
			log.Printf("  Auth:      operator token REQUIRED for mutating routes")
		case operatorUser != "":
			log.Printf("  Auth:      operator basic auth REQUIRED for mutating routes")
		default:
			log.Printf("  Auth:      FAIL-CLOSED — no operator credential configured; every mutating route answers 503 until SCHEDULER_OPERATOR_TOKEN/[api] operator_token is set")
		}
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	// Start the evaluation loop in background.
	go loop.Run()

	// ADV-R07: board-driven wake. The watcher polls enabled projects'
	// board files and forces a re-evaluation ~5 min after a board write
	// (bounded debounce). Ordering-only by law: cooldown stays the sole
	// admission authority, and every watcher failure fails open to the
	// clock cadence. A nil gateway/exec-less run keeps it armed — it
	// only adds evaluation triggers.
	boardWatcher := scheduler.NewBoardWakeWatcher(db, loop.ForceEvaluate)
	boardWatcher.SetClock(clk)
	boardWatcher.Start()

	// Start DuckBrain sync in background.
	go func() {
		duckbrain.Run(context.Background())
	}()

	// SCHED-GAP-148: the startup announcement carries the build identity, so
	// one grep of the boot log answers "which commit is this daemon running?"
	// — the same sha this daemon serves as /api/v1/status build_sha, which
	// ops/check-daemon-freshness.sh compares against the newest commit
	// touching admission/scheduling code. Shape:
	//   build version=<ver> sha=<8-char-or-injected-scope> built=<rfc3339>
	log.Printf("schedulerd ready — build version=%s sha=%s built=%s",
		version.Current(), version.CurrentCommit(), version.CurrentBuildDate())
	printStatus(db)

	// Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received %v, shutting down...", sig)

	// ADV-R07: stop the board watcher before the loop drains.
	boardWatcher.Stop()

	loop.Stop()
	// Wait for in-flight ticks to complete (with a generous timeout).
	// Spawned ticks can run up to tickTimeout; we give them a chance to
	// finish naturally before the HTTP server begins its own drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	log.Println("Shutdown complete")
}

func printStatus(d *sql.DB) {
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, d, false)
	if err != nil {
		return
	}
	enabled := 0
	for _, p := range projects {
		if p.Enabled {
			enabled++
		}
	}
	sep := strings.Repeat("─", 50)
	log.Print(sep)
	log.Printf("Fleet: %d projects (%d enabled)", len(projects), enabled)
	var n int
	_ = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&n)
	log.Printf("Active ticks: %d", n)
	log.Print(sep)
}
