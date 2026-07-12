package main

import (
	"context"
	"database/sql"
	"flag"
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
	server.Shutdown(ctx)
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
