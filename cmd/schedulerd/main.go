package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coding-herms/scheduler/internal/api"
	"github.com/coding-herms/scheduler/internal/dashboard"
	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/mcp"
	"github.com/coding-herms/scheduler/internal/scheduler"
	"github.com/coding-herms/scheduler/internal/sync"
)

func main() {
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/scheduler.db"), "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:9090", "HTTP listen address")
	minInterval := flag.Duration("min-interval", 20*time.Minute, "Fastest tick interval")
	maxInterval := flag.Duration("max-interval", 24*time.Hour, "Slowest tick interval")
	numLevels := flag.Int("num-levels", 10, "Number of priority levels")
	weightBudget := flag.Int("budget", 100, "Weight budget")
	maxConcurrent := flag.Int("max-concurrent", 8, "Max concurrent foremen")
	duckbrainNS := flag.String("duckbrain-ns", "coding-hermes", "DuckBrain namespace for sync")
	simulate := flag.Bool("simulate", false, "Run in dry-run/simulation mode (no real spawning)")
	simSuccess := flag.Float64("sim-success", 0.85, "Simulated success rate (0.0-1.0)")
	simCount := flag.Int("sim-count", 0, "Generate N simulated ticks and exit (0 = run loop)")
	simSetup := flag.Bool("sim-setup", false, "Create test fixture with 14 dry-run projects")
	simTicks := flag.Int("sim-ticks", 10, "Number of evaluation ticks to run in sim-setup mode")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Initialize database.
	db, err := database.InitDB(*dbPath)
	if err != nil {
		log.Fatalf("FATAL: database init: %v", err)
	}
	defer db.Close()
	log.Printf("Database: %s (WAL mode)", *dbPath)

	// Create the evaluation loop.
	loop := scheduler.NewLoop(db, *minInterval, *maxInterval, *numLevels, *weightBudget, *maxConcurrent)
	if *simulate {
		loop.SetSimulation(*simSuccess)
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

	// Start HTTP server.
	server := &http.Server{
		Addr:    *listen,
		Handler: mux,
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
		duckbrain := sync.NewDuckBrainSync(db, *duckbrainNS)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
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
