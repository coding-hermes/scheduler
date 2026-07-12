package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/coding-herms/scheduler/internal/database"
)

// CronJob mirrors the Hermes cron job config format.
type CronJob struct {
	JobID    string   `json:"job_id"`
	Name     string   `json:"name"`
	Skills   []string `json:"skills,omitempty"`
	Model    string   `json:"model,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Workdir  string   `json:"workdir,omitempty"`
	Enabled  bool     `json:"enabled"`
	Schedule string   `json:"schedule"`
}

func main() {
	jobsFile := flag.String("jobs", os.ExpandEnv("$HOME/.hermes/cron/jobs.json"), "Path to cron jobs.json")
	dbPath := flag.String("db", os.ExpandEnv("$HOME/.hermes/scheduler.db"), "SQLite database path")
	dryRun := flag.Bool("dry-run", false, "Print what would be imported without writing")
	flag.Parse()

	jobs, err := loadJobs(*jobsFile)
	if err != nil {
		log.Fatalf("FATAL: load jobs: %v", err)
	}
	log.Printf("Loaded %d jobs from %s", len(jobs), *jobsFile)

	db, err := database.InitDB(*dbPath)
	if err != nil {
		log.Fatalf("FATAL: database init: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	imported := 0
	skipped := 0

	for _, j := range jobs {
		// Only import coding-hermes foreman jobs.
		if !isCodingHermesJob(j) {
			skipped++
			continue
		}

		name := projectName(j)

		// Check if already imported.
		_, err := database.GetProject(ctx, db, name)
		if err == nil {
			log.Printf("SKIP %s: already exists", name)
			skipped++
			continue
		}

		if *dryRun {
			log.Printf("DRY-RUN: would import %s (weight=10, priority=5, workdir=%s, model=%s, provider=%s)",
				name, j.Workdir, j.Model, j.Provider)
			imported++
			continue
		}

		p := &database.Project{
			Name:     name,
			RepoURL:  fmt.Sprintf("local:%s", j.Workdir),
			Workdir:  j.Workdir,
			Weight:   10,
			Priority: 5,
			CooldownS: 900,
			DecayRate: 1.0,
			Model:    "deepseek-v4-pro",
			Provider: "deepseek-foreman",
			Enabled:  true,
		}

		if j.Model != "" {
			p.Model = j.Model
		}
		if j.Provider != "" {
			p.Provider = j.Provider
		}

		if err := database.CreateProject(ctx, db, p); err != nil {
			log.Printf("ERROR importing %s: %v", name, err)
			continue
		}
		log.Printf("IMPORTED %s (weight=%d, priority=%d, model=%s, provider=%s)",
			name, p.Weight, p.Priority, p.Model, p.Provider)
		imported++
	}

	log.Printf("Done: %d imported, %d skipped (non-foreman or already exists)", imported, skipped)

	if *dryRun {
		fmt.Println("\nRun without --dry-run to perform the import.")
	}
}

func loadJobs(path string) ([]CronJob, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Jobs []CronJob `json:"jobs"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Jobs, nil
}

func isCodingHermesJob(j CronJob) bool {
	for _, s := range j.Skills {
		if strings.Contains(s, "coding-hermes") {
			return true
		}
	}
	return strings.Contains(j.Name, "foreman") || strings.Contains(j.Name, "coding-hermes")
}

func projectName(j CronJob) string {
	// Derive project name from workdir or job name.
	if j.Workdir != "" {
		return filepath.Base(j.Workdir)
	}
	// Clean job name.
	name := strings.ReplaceAll(strings.ToLower(j.Name), " ", "-")
	name = strings.ReplaceAll(name, "coding-hermes-foreman", "")
	name = strings.ReplaceAll(name, "coding-hermes", "")
	name = strings.Trim(name, "- ")
	if name == "" {
		name = fmt.Sprintf("unknown-%s", j.JobID[:8])
	}
	return name
}
