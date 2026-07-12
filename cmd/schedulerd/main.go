package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

func main() {
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/scheduler.db"), "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:9090", "HTTP listen address")
	_ = flag.String("unix-socket", "", "Unix socket path (overrides -listen)")
	minInterval := flag.Duration("min-interval", 20*time.Minute, "Fastest tick interval")
	maxInterval := flag.Duration("max-interval", 24*time.Hour, "Slowest tick interval")
	numLevels := flag.Int("num-levels", 10, "Number of priority levels")
	weightBudget := flag.Int("budget", 100, "Weight budget")
	maxConcurrent := flag.Int("max-concurrent", 8, "Max concurrent foremen")
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

	// HTTP server with basic endpoints.
	mux := http.NewServeMux()

	// Health.
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})

	// Fleet status.
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		projects, err := database.ListProjects(context.Background(), db, true)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{
			"budget_total":    *weightBudget,
			"max_concurrent":  *maxConcurrent,
			"active_projects": len(projects),
		})
	})

	// Force evaluate.
	mux.HandleFunc("/api/v1/evaluate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		loop.ForceEvaluate()
		writeJSON(w, map[string]string{"status": "evaluation triggered"})
	})

	// Pause / Resume.
	mux.HandleFunc("/api/v1/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		loop.Pause()
		writeJSON(w, map[string]string{"status": "paused"})
	})
	mux.HandleFunc("/api/v1/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		loop.Resume()
		writeJSON(w, map[string]string{"status": "resumed"})
	})

	// Start server.
	server := &http.Server{
		Addr:    *listen,
		Handler: mux,
	}
	go func() {
		log.Printf("HTTP: listening on %s", *listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	// Start the evaluation loop in background.
	go loop.Run()

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

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Ensure unused import for scheduler package resolves.
var _ = fmt.Sprintf
