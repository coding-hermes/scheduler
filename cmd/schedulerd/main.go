package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // registers handlers on DefaultServeMux
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coding-herms/scheduler/internal/api"
	"github.com/coding-herms/scheduler/internal/config"
	"github.com/coding-herms/scheduler/internal/dashboard"
	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/mcp"
	"github.com/coding-herms/scheduler/internal/scheduler"
	"github.com/coding-herms/scheduler/internal/sync"
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
	tickTimeout := flag.Duration("tick-timeout", 900*time.Second, "Maximum tick duration before timeout")
	testVerifyFlag := flag.Int("test-verify", 0, "Run N-cycle correctness verification and exit")
	duckbrainNS := flag.String("duckbrain-ns", "coding-hermes", "DuckBrain namespace for sync")
	duckbrainURL := flag.String("duckbrain-url", "http://localhost:3000", "DuckBrain HTTP server URL")
	simulate := flag.Bool("simulate", false, "Run in dry-run/simulation mode (no real spawning)")
	simSuccess := flag.Float64("sim-success", 0.85, "Simulated success rate (0.0-1.0)")
	simCount := flag.Int("sim-count", 0, "Generate N simulated ticks and exit (0 = run loop)")
	gatewayURL := flag.String("gateway-url", "http://127.0.0.1:8642", "Hermes gateway API URL (empty = use exec.Command)")
	gatewayKey := flag.String("gateway-key", os.Getenv("API_SERVER_KEY"), "Hermes gateway API key")
	foremanHome := flag.String("foreman-home", os.ExpandEnv("$HOME/.hermes/foreman"), "HERMES_HOME path for foreman sessions")
	simSetup := flag.Bool("sim-setup", false, "Create test fixture with 14 dry-run projects")
	simTicks := flag.Int("sim-ticks", 10, "Number of evaluation ticks to run in sim-setup mode")
	configFile := flag.String("config", "", "Path to TOML fleet config file")
	showConfigFlag := flag.Bool("show-config", false, "Print resolved config (CLI + env) as TOML and exit")
	schemaFlag := flag.Bool("schema", false, "Output JSON Schema for schedulerd.toml and exit")
	flag.Parse()

	if *schemaFlag {
		printSchema()
		return
	}
	if *showConfigFlag {
		printConfig(*configFile, *dbPath, *listen, *minInterval, *maxInterval,
			*numLevels, *weightBudget, *maxConcurrent, *namespaceMode,
			*tickTimeout, *gatewayURL, *gatewayKey, *foremanHome,
			*duckbrainNS, *duckbrainURL)
		return
	}

	if os.Getenv("SCHEDULER_NAMESPACE_MODE") == "true" {
		*namespaceMode = true
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

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

	// ── Test-verify mode: run correctness checks and exit ──
	if *testVerifyFlag > 0 {
		if err := testVerify(*testVerifyFlag); err != nil {
			log.Fatalf("VERIFY FAILED: %v", err)
		}
		return
	}

	// Create the evaluation loop.
	loop := scheduler.NewLoop(db, *minInterval, *maxInterval, *numLevels, *weightBudget, *maxConcurrent, *namespaceMode)
	// Apply the tick timeout to the real spawner so Wait()/scanner cleanup use it.
	loop.SetTickTimeout(*tickTimeout)
	loop.SetForemanHome(*foremanHome)
	if *simulate {
		loop.SetSimulation(*simSuccess)
	}

	// Wire gateway HTTP client with retry (FEAT-003).
	if *gatewayURL != "" && *gatewayKey != "" {
		gwClient := scheduler.NewGatewayClient(*gatewayURL, *gatewayKey, *tickTimeout)
		// Retry gateway connection with backoff — gateway may not be ready
		// when schedulerd starts (systemd ordering). Once connected, keep
		// retrying in the background if it ever drops.
		var gwConnected bool
		for attempt := 0; attempt < 10; attempt++ {
			if err := gwClient.Ping(context.Background()); err != nil {
				wait := time.Duration(attempt+1) * 2 * time.Second
				log.Printf("WARN: gateway %s not reachable (attempt %d/10, retry in %v): %v", *gatewayURL, attempt+1, wait, err)
				time.Sleep(wait)
			} else {
				loop.SetGatewayClient(gwClient)
				log.Printf("GATEWAY: connected to %s — using HTTP API instead of exec.Command", *gatewayURL)
				gwConnected = true
				break
			}
		}
		if !gwConnected {
			log.Printf("WARN: gateway %s unreachable after 10 retries — falling back to exec.Command", *gatewayURL)
		}
		// Launch background reconnector — keeps trying if gateway drops later.
		go func() {
			for {
				time.Sleep(60 * time.Second)
				if !gwConnected {
					continue // already in fallback mode
				}
				if err := gwClient.Ping(context.Background()); err != nil {
					log.Printf("WARN: gateway %s dropped (%v) — retrying...", *gatewayURL, err)
					for attempt := 0; attempt < 10; attempt++ {
						time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
						if err := gwClient.Ping(context.Background()); err == nil {
							log.Printf("GATEWAY: reconnected to %s", *gatewayURL)
							loop.SetGatewayClient(gwClient)
							break
						}
					}
				}
			}
		}()
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
	apiServer := api.NewServer(db, loop)
	mcpServer := mcp.NewServer(db, loop)
	dashGen := dashboard.NewGenerator(db)

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
		duckbrain := sync.NewDuckBrainSync(db, *duckbrainNS, *duckbrainURL)
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
