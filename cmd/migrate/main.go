package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coding-herms/scheduler/internal/database"
)

// CronJob mirrors the actual Hermes cron job config format.
type CronJob struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Skills   []string `json:"skills,omitempty"`
	Model    string   `json:"model,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Enabled  bool     `json:"enabled"`
	Schedule struct {
		Kind    string `json:"kind"`
		Expr    string `json:"expr,omitempty"`
		Minutes int    `json:"minutes,omitempty"`
		Display string `json:"display"`
	} `json:"schedule"`
	Prompt string `json:"prompt,omitempty"`
}

var workdirRe = regexp.MustCompile(`[Ww]orkdir:\s*(\S+)`)

func main() {
	jobsFile := flag.String("jobs", os.ExpandEnv("$HOME/.hermes/cron/jobs.json"), "Path to cron jobs.json")
	dbFile := flag.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"), "SQLite database path")
	dryRun := flag.Bool("dry-run", false, "Print what would be imported without writing")
	flag.Parse()

	jobs, err := loadJobs(*jobsFile)
	if err != nil {
		log.Fatalf("FATAL: load jobs: %v", err)
	}
	log.Printf("Loaded %d jobs from %s", len(jobs), *jobsFile)

	db, err := database.InitDB(*dbFile)
	if err != nil {
		log.Fatalf("FATAL: database init: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	imported := 0
	skipped := 0

	for _, j := range jobs {
		if !isCodingHermesJob(j) {
			skipped++
			continue
		}

		name := projectName(j)
		workdir := extractWorkdir(j)

		if workdir == "" {
			log.Printf("SKIP %s: no workdir found in prompt", name)
			skipped++
			continue
		}

		if !*dryRun {
			// Check if already imported.
			_, err := database.GetProject(ctx, db, name)
			if err == nil {
				log.Printf("SKIP %s: already exists", name)
				skipped++
				continue
			}
		}

		model := j.Model
		if model == "" {
			model = "deepseek-v4-pro"
		}
		provider := j.Provider
		if provider == "" {
			provider = "deepseek-foreman"
		}

		if *dryRun {
			log.Printf("DRY-RUN: %s (w=%d p=%d workdir=%s model=%s provider=%s enabled=%v schedule=%s)",
				name, 10, 5, workdir, model, provider, j.Enabled, j.Schedule.Display)
			imported++
			continue
		}

		p := &database.Project{
			Name:      name,
			RepoURL:   fmt.Sprintf("local:%s", workdir),
			Workdir:   workdir,
			Weight:    10,
			Priority:  5,
			CooldownS: 900,
			DecayRate: 1.0,
			Model:     model,
			Provider:  provider,
			Enabled:   j.Enabled,
		}

		if err := database.CreateProject(ctx, db, p); err != nil {
			log.Printf("ERROR importing %s: %v", name, err)
			continue
		}
		log.Printf("IMPORTED %s (w=%d p=%d workdir=%s schedule=%s)",
			name, p.Weight, p.Priority, workdir, j.Schedule.Display)
		imported++
	}

	log.Printf("Done: %d imported, %d skipped", imported, skipped)

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
		return nil, fmt.Errorf("parse jobs.json: %w", err)
	}
	return wrapper.Jobs, nil
}

func isCodingHermesJob(j CronJob) bool {
	for _, s := range j.Skills {
		if strings.Contains(s, "coding-hermes") {
			return true
		}
	}
	return strings.Contains(strings.ToLower(j.Name), "coding-hermes") ||
		strings.Contains(strings.ToLower(j.Name), "foreman")
}

func extractWorkdir(j CronJob) string {
	matches := workdirRe.FindStringSubmatch(j.Prompt)
	if len(matches) >= 2 {
		wd := strings.TrimRight(matches[1], ".,;:")
		return wd
	}
	return ""
}

func projectName(j CronJob) string {
	// Prefer workdir basename for clean project keys.
	wd := extractWorkdir(j)
	if wd != "" {
		return filepath.Base(wd)
	}
	// Fallback: clean the job name.
	name := strings.TrimSuffix(strings.TrimSuffix(j.Name, " coding-hermes-foreman"), " coding-hermes")
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.ToLower(name)
	if name == "" || name == "coding-hermes" {
		name = fmt.Sprintf("unknown-%s", j.ID[:8])
	}
	return name
}
