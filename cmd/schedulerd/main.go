package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coding-herms/scheduler/internal/api"
	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

func main() {
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/scheduler.db"), "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:9090", "HTTP listen address")
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

	// Create the API server.
	apiServer := api.NewServer(db, loop)

	// Start HTTP server.
	server := &http.Server{
		Addr:    *listen,
		Handler: apiServer.Handler(),
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
