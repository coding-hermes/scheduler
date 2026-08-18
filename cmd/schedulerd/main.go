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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/sync"
)

func main() {
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"), "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:9090", "HTTP listen address")
	minInterval := flag.Duration("min-interval", 30*time.Second, "Fastest tick interval")
	maxInterval := flag.Duration("max-interval", 24*time.Hour, "Slowest tick interval")
	numLevels := flag.Int("num-levels", 10, "Number of priority levels")
	weightBudget := flag.Int("budget", 100, "Weight budget")
	maxConcurrent := flag.Int("max-concurrent", 10, "Max concurrent foremen")
	namespaceMode := flag.Bool("namespace-mode", false, "Enable multi-namespace scheduling")
	tickTimeout := flag.Duration("tick-timeout", 7200*time.Second, "Maximum tick duration before timeout (2h)")
	testVerifyFlag := flag.Int("test-verify", 0, "Run N-cycle correctness verification and exit")
	duckbrainNS := flag.String("duckbrain-ns", "coding-hermes", "DuckBrain namespace for sync")
	duckbrainURL := flag.String("duckbrain-url", "http://localhost:3000", "DuckBrain HTTP server URL")
	simulate := flag.Bool("simulate", false, "Run in dry-run/simulation mode (no real spawning)")
	simSuccess := flag.Float64("sim-success", 0.85, "Simulated success rate (0.0-1.0)")
	simCount := flag.Int("sim-count", 0, "Generate N simulated ticks and exit (0 = run loop)")
	gatewayURL := flag.String("gateway-url", "http://127.0.0.1:8642", "Hermes gateway API URL (empty = use exec.Command)")
	gatewayKey := flag.String("gateway-key", os.Getenv("API_SERVER_KEY"), "Hermes gateway API key")
	noExecFallback := flag.Bool("no-exec-fallback", true, "Disable exec.Command fallback when gateway fails (default true for safety)")
	foremanHome := flag.String("foreman-home", os.ExpandEnv("$HOME/.hermes/foreman"), "HERMES_HOME path for foreman sessions")
	simSetup := flag.Bool("sim-setup", false, "Create test fixture with 13 dry-run projects (12 enabled + 1 disabled)")
	simTicks := flag.Int("sim-ticks", 10, "Number of evaluation ticks to run in sim-setup mode")
	configFile := flag.String("config", "", "Path to TOML fleet config file")
	failureWindow := flag.Int("failure-window", 100, "Number of recent ticks per project for /api/v1/status per-project failure-rate breakdown")
	autoDisableRate := flag.Float64("auto-disable-failure-rate", 0, "Per-project failure-rate threshold (0.0–1.0) for auto-disable; 0 = off (SCHED-GAP-018)")
	autoDisableWindow := flag.Int("auto-disable-window", 100, "Ticks per project over which auto-disable failure rate is computed")
	autoDisableMinTicks := flag.Int("auto-disable-min-ticks", 50, "Minimum ticks in window before auto-disable can fire")
	logFile := flag.String("log-file", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.log"), "Path to append structured tick logs (JSON lines); empty disables")
	showConfigFlag := flag.Bool("show-config", false, "Print resolved config (CLI + env) as TOML and exit")
	schemaFlag := flag.Bool("schema", false, "Output JSON Schema for schedulerd.toml and exit")
	flag.Parse()

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
	if v := os.Getenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			*autoDisableRate = f
		}
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
			*tickTimeout,
			*gatewayURL, *gatewayKey, *foremanHome, *noExecFallback,
			*duckbrainNS, *duckbrainURL,
			*autoDisableRate, *autoDisableWindow, *autoDisableMinTicks, *failureWindow)
		return
	}

	// ── Test-verify mode: run correctness checks and exit ──
	// Runs BEFORE the main database is opened: testVerify creates its own
	// temp DB, so requiring the production DB path here would break CI and
	// any host without ~/.hermes/coding-hermes/ (DOGFOOD-002 follow-up).
	if *testVerifyFlag > 0 {
		if err := testVerify(*testVerifyFlag); err != nil {
			log.Fatalf("VERIFY FAILED: %v", err)
		}
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
	loop := scheduler.NewLoop(db, *minInterval, *maxInterval, *numLevels, *weightBudget, *maxConcurrent, *namespaceMode)
	// Apply the tick timeout to the real spawner so Wait()/scanner cleanup use it.
	loop.SetTickTimeout(*tickTimeout)
	loop.SetForemanHome(*foremanHome)
	loop.SetNoExecFallback(*noExecFallback)
	if *simulate {
		loop.SetSimulation(*simSuccess)
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
		}
	}
	loop.SetAutoDisablePolicy(*autoDisableRate, *autoDisableWindow, *autoDisableMinTicks)
	if *autoDisableRate > 0 {
		log.Printf("AUTO-DISABLE: enabled — rate=%.2f window=%d min_ticks=%d", *autoDisableRate, *autoDisableWindow, *autoDisableMinTicks)
	} else {
		log.Printf("AUTO-DISABLE: off (rate=0)")
	}

	// Wire gateway HTTP client with retry (FEAT-003).
	if *gatewayURL != "" && *gatewayKey != "" {
		gwClient := scheduler.NewGatewayClient(*gatewayURL, *gatewayKey, *tickTimeout)
		// Retry gateway connection with backoff — gateway may not be ready
		// when schedulerd starts (systemd ordering). Once connected, keep
		// retrying in the background if it ever drops.
		var gwConnected atomic.Bool
		for attempt := 0; attempt < 10; attempt++ {
			if err := gwClient.Ping(context.Background()); err != nil {
				wait := time.Duration(attempt+1) * 2 * time.Second
				log.Printf("WARN: gateway %s not reachable (attempt %d/10, retry in %v): %v", *gatewayURL, attempt+1, wait, err)
				time.Sleep(wait)
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
		}, &gwConnected, *gatewayURL)
	}

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

		simCtx, simCancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	apiServer := api.NewServer(db, loop)
	apiServer.SetFailureWindow(*failureWindow)
	// SCHED-GAP-034: snapshot the ACTIVE three-layer config (TOML < env <
	// CLI) for GET /api/v1/config. By this point the flag vars carry the
	// resolved values — TOML overrides were applied above where flags sat
	// at their defaults, and SCHEDULER_* env overrides were applied earlier.
	// The gateway key is masked by SetResolvedConfig; it never reaches the wire.
	apiServer.SetResolvedConfig(api.ResolvedConfig{
		DBPath:                 *dbPath,
		Listen:                 *listen,
		MinInterval:            minInterval.String(),
		MaxInterval:            maxInterval.String(),
		NumLevels:              *numLevels,
		WeightBudget:           *weightBudget,
		MaxConcurrent:          *maxConcurrent,
		TickTimeout:            tickTimeout.String(),
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
	dashGen := dashboard.NewGenerator(db, *gatewayURL)
	dashGen.SetDuckBrainURL(*duckbrainURL)
	dashGen.SetSpawnCounts(loop.SpawnMethodCounts)

	// Compose all handlers into one mux.
	mux := http.NewServeMux()

	// Dashboard at /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.Generate(w); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})

	// htmx partial: rendered for the main dashboard's tbody every 10s.
	mux.HandleFunc("GET /dashboard/partial", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashGen.GenerateFleetTable(w); err != nil {
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

	// Tick history page: /ticks (paginated, global tick log).
	// htmx polls return the #tick-history fragment only (HX-Request) — the
	// full page must never be swapped into its own poller.
	mux.HandleFunc("GET /ticks", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				page = n
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var err error
		if r.Header.Get("HX-Request") != "" {
			err = dashGen.GenerateTickHistoryPartial(w, page)
		} else {
			err = dashGen.GenerateTickHistory(w, page)
		}
		if err != nil {
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

	server := &http.Server{
		Addr:    *listen,
		Handler: pprofMux,
	}
	go func() {
		log.Printf("HTTP: listening on %s", *listen)
		log.Printf("  Dashboard: http://%s/", *listen)
		log.Printf("  API:       http://%s/api/v1/health", *listen)
		log.Printf("  MCP:       http://%s/mcp", *listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	// Start the evaluation loop in background.
	go loop.Run()

	// Start DuckBrain sync in background.
	go func() {
		duckbrain.Run(context.Background())
	}()

	log.Println("schedulerd ready")
	printStatus(db)

	// Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received %v, shutting down...", sig)

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
