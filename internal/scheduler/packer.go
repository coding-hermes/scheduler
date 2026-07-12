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
	name          string
	priority      float64
	weight        int
	urgency       float64
	decayRate     float64
	cooldownS     int
	lastCompleted *time.Time
	createdAt     time.Time
	workdir       string
	repoURL       string
}

// Pick returns the selected projects for this tick, sorted by urgency desc.
func (p *Packer) Pick(now time.Time) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT name, weight, priority, decay_rate, enabled, cooldown_s,
		       COALESCE(last_tick_completed, created_at),
		       created_at, workdir, repo_url
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
		var lastStr sql.NullString
		var createdAtStr string
		var enabled bool
		if err := rows.Scan(&s.name, &s.weight, &s.priority, &s.decayRate, &enabled, &s.cooldownS,
			&lastStr, &createdAtStr, &s.workdir, &s.repoURL); err != nil {
			log.Printf("ERROR scanning project row: %v", err)
			continue
		}
		s.createdAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if lastStr.Valid && lastStr.String != "" {
			t, err := time.Parse(time.RFC3339, lastStr.String)
			if err == nil {
				s.lastCompleted = &t
			}
		}
		s.urgency = p.calculator.ComputeUrgency(s.priority, s.decayRate, now, s.lastCompleted, s.createdAt)
		list = append(list, s)
	}

	// Sort by urgency descending.
	sort.Slice(list, func(i, j int) bool {
		return list[i].urgency > list[j].urgency
	})

	// Greedy pack: pick projects that fit in budget.
	currentlyRunning := p.runningCount()
	used := 0
	var packed []PackedProject

	for _, s := range list {
		if used+s.weight > p.budget {
			continue
		}
		if currentlyRunning >= p.maxConcurrent {
			log.Printf("PACKER: max concurrency reached (%d), stopping", p.maxConcurrent)
			break
		}
		cooldownDur := time.Duration(s.cooldownS) * time.Second
		if s.lastCompleted != nil && now.Sub(*s.lastCompleted) < cooldownDur {
			continue
		}
		packed = append(packed, PackedProject{
			Name:     s.name,
			Priority: s.priority,
			Weight:   s.weight,
			Urgency:  s.urgency,
			Workdir:  s.workdir,
			RepoURL:  s.repoURL,
		})
		used += s.weight
		currentlyRunning++
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

// Budget returns the current weight budget.
func (p *Packer) Budget() int { return p.budget }
