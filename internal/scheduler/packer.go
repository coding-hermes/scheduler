package scheduler

import (
	"database/sql"
	"log"
	"sort"
	"time"
)

// PackedProject is a project selected to run in this tick.
type PackedProject struct {
	Name     string
	Priority float64
	Weight   int
	Urgency  float64
	Workdir  string
	RepoURL  string
	Command  string // optional: custom spawn command (overrides default hermes chat)
	Model    string // LLM model for this project (empty = use spawner default)
	Provider string // LLM provider for this project (empty = use spawner default)
	Deliver  string // delivery target (telegram:chat_id:thread_id)
}

// Packer selects which projects run given a weight budget and running set.
type Packer struct {
	db            *sql.DB
	calculator    *UrgencyCalculator
	budget        int
	maxConcurrent int
}

// NewPacker creates a packer with the given budget and concurrency cap.
func NewPacker(db *sql.DB, calc *UrgencyCalculator, budget, maxConcurrent int) *Packer {
	return &Packer{db: db, calculator: calc, budget: budget, maxConcurrent: maxConcurrent}
}

// scored is a project with its computed urgency.
type scored struct {
	name       string
	priority   float64
	weight     int
	urgency    float64
	decayRate  float64
	cooldownS  int
	lastTickAt *time.Time
	createdAt  time.Time
	workdir    string
	repoURL    string
	command    string
	model      string
	provider   string
	deliver    string
}

// Pick returns the selected projects for this tick, sorted by urgency desc.
func (p *Packer) Pick(now time.Time, spawnerRunning map[string]bool) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT name, weight, priority, decay_rate, enabled, cooldown_s,
		       last_tick_completed,
		       created_at, workdir, repo_url, COALESCE(command, ''),
		       COALESCE(model, ''), COALESCE(provider, ''), COALESCE(deliver, '')
		FROM projects
		WHERE enabled = 1
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []scored

	for rows.Next() {
		var s scored
		var lastCompleted *time.Time
		var lastStr sql.NullString
		var createdAtStr string
		var enabled bool
		if err := rows.Scan(&s.name, &s.weight, &s.priority, &s.decayRate, &enabled, &s.cooldownS,
			&lastStr, &createdAtStr, &s.workdir, &s.repoURL, &s.command,
			&s.model, &s.provider, &s.deliver); err != nil {
			log.Printf("ERROR scanning project row: %v", err)
			continue
		}
		s.createdAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if lastStr.Valid && lastStr.String != "" {
			t, err := time.Parse(time.RFC3339, lastStr.String)
			if err == nil {
				lastCompleted = &t
			}
		}
		s.urgency = p.calculator.ComputeUrgency(s.priority, s.decayRate, now, lastCompleted, s.createdAt)
		s.lastTickAt = lastCompleted
		list = append(list, s)
	}

	// Sort by urgency descending.
	sort.Slice(list, func(i, j int) bool {
		return list[i].urgency > list[j].urgency
	})

		// Greedy pack: pick projects that fit in budget.
		currentlyRunning := p.runningCount()
		runningSet := p.runningProjectSet()
		// Merge with spawner's in-memory active set — spawns may not be\n	// committed to DB yet (race condition).\n	for name := range spawnerRunning {\n		runningSet[name] = true\n	}\n	// Re-pool currentlyRunning from merged set for accuracy.\n	if len(runningSet) > currentlyRunning {\n		currentlyRunning = len(runningSet)\n	}
	used := 0
	packed := make([]PackedProject, 0, max(1, len(list)/2))

	totalChecked := 0
	totalSkippedBudget := 0
	totalSkippedCooldown := 0
	totalSkippedRunning := 0

	for _, s := range list {
		totalChecked++
		if runningSet[s.name] {
			totalSkippedRunning++
			continue
		}
		if used+s.weight > p.budget {
			totalSkippedBudget++
			continue
		}
		if currentlyRunning >= p.maxConcurrent {
			log.Printf("PACKER: max concurrency reached (%d), stopping", p.maxConcurrent)
			break
		}
		cooldownDur := time.Duration(s.cooldownS) * time.Second
		if s.cooldownS == 0 {
			// Dynamic: derive from priority via urgency calculator.
			cooldownDur = p.calculator.ComputeInterval(s.priority)
		}
		if s.lastTickAt != nil && now.Sub(*s.lastTickAt) < cooldownDur {
			totalSkippedCooldown++
			continue
		}
		packed = append(packed, PackedProject{
			Name:     s.name,
			Priority: s.priority,
			Weight:   s.weight,
			Urgency:  s.urgency,
			Workdir:  s.workdir,
			RepoURL:  s.repoURL,
			Command:  s.command,
			Model:    s.model,
			Provider: s.provider,
			Deliver:  s.deliver,
		})
		used += s.weight
		currentlyRunning++
	}

	if len(packed) == 0 {
		log.Printf("PACKER: nothing packed — checked %d projects, skipped budget=%d cooldown=%d already-running=%d, total-running=%d/%d",
			totalChecked, totalSkippedBudget, totalSkippedCooldown, totalSkippedRunning, currentlyRunning, p.maxConcurrent)
	}
	return packed, nil
}

// runningCount returns the number of ticks currently in running status.
func (p *Packer) runningCount() int {
	var n int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n); err != nil {
		log.Printf("ERROR counting running ticks: %v", err)
		return 0
	}
	return n
}

// runningProjectSet returns the set of project names that have at least one running tick.
func (p *Packer) runningProjectSet() map[string]bool {
	set := map[string]bool{}
	rows, err := p.db.Query(`SELECT DISTINCT project_name FROM ticks WHERE status = 'running'`)
	if err != nil {
		log.Printf("ERROR querying running projects: %v", err)
		return set
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		rows.Scan(&name)
		set[name] = true
	}
	return set
}

// Budget returns the current weight budget.
func (p *Packer) Budget() int { return p.budget }

// ListEnabled returns all enabled projects as PackedProject for simulation.
func (p *Packer) ListEnabled(ctx interface{}) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT name, weight, priority, workdir, repo_url
		FROM projects WHERE enabled = 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PackedProject
	for rows.Next() {
		var pp PackedProject
		if err := rows.Scan(&pp.Name, &pp.Weight, &pp.Priority, &pp.Workdir, &pp.RepoURL); err != nil {
			return nil, err
		}
		out = append(out, pp)
	}
	return out, rows.Err()
}
