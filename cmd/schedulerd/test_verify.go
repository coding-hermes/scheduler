package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// testVerify runs a self-contained end-to-end scheduling correctness test.
// It creates a temp DB, registers a known fleet, runs N cycles, and checks invariants.
func testVerify(cycles int) error {
	tmpDir, err := os.MkdirTemp("", "scheduler-verify-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := tmpDir + "/test.db"
	db, err := database.InitDB(dbPath)
	if err != nil {
		return fmt.Errorf("init db: %w", err)
	}
	defer db.Close()

	// ── Register test fleet ──
	type testProj struct {
		Name      string
		Weight    int
		Priority  int
		CooldownS int
		SleepS    int
	}

	projects := []testProj{
		{"alpha", 30, 9, 15, 2},
		{"beta", 30, 7, 15, 2},
		{"gamma", 20, 8, 10, 3},
		{"delta", 20, 4, 10, 4},
		{"epsilon", 10, 9, 5, 1},
		{"zeta", 10, 2, 5, 5},
		{"eta", 5, 1, 5, 10},
	}

	ctx := context.Background()
	budget := 100
	maxConcur := 6

	for _, p := range projects {
		proj := &database.Project{
			Name:      p.Name,
			RepoURL:   "local:/test",
			Workdir:   tmpDir,
			Weight:    p.Weight,
			Priority:  p.Priority,
			CooldownS: p.CooldownS,
			Enabled:   true,
			Command: fmt.Sprintf(
				"bash -c 'echo session_id: %s-$(date +%%s)-$$; sleep %d; echo done'",
				p.Name, p.SleepS,
			),
		}
		if err := database.CreateProject(ctx, db, proj); err != nil {
			return fmt.Errorf("create %s: %w", p.Name, err)
		}
	}

	// ── Run cycles ──
	loop := scheduler.NewLoop(db, 60*time.Second, 4*time.Hour, 10, budget, maxConcur)
	// Default loop already has simulation OFF; ensure we're using real spawns.

	for i := 0; i < cycles; i++ {
		loop.ForceEvaluate()
		time.Sleep(time.Duration(projects[0].SleepS+1) * time.Second)
	}

	// ── Wait for all ticks to settle ──
	time.Sleep(5 * time.Second)

	// ── Verify invariants ──
	checks := 0
	failures := 0

	check := func(name string, ok bool, detail string) {
		checks++
		if ok {
			fmt.Printf("  ✓ %-40s %s\n", name, detail)
		} else {
			failures++
			fmt.Printf("  ✗ %-40s %s\n", name, detail)
		}
	}

	// 1. No hanging ticks.
	var hanging int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&hanging)
	check("No hanging ticks", hanging == 0, fmt.Sprintf("%d hanging", hanging))

	// 2. All projects got at least one tick.
	var projCount int
	db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT project_name) FROM ticks`).Scan(&projCount)
	check("All 7 projects got ticks", projCount >= 7, fmt.Sprintf("%d/7", projCount))

	// 3. Budget never exceeded.
	rows, _ := db.QueryContext(ctx, `SELECT id, project_name, spawned_at FROM ticks WHERE status='completed' OR status='failed' OR status='timeout'`)
	type tickInfo struct{ id, proj, spawned string }
	type evalGroup struct {
		time  string
		ticks []tickInfo
	}
	evals := map[string]*evalGroup{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ti tickInfo
			rows.Scan(&ti.id, &ti.proj, &ti.spawned)
			if len(ti.spawned) >= 19 {
				key := ti.spawned[:19]
				if _, ok := evals[key]; !ok {
					evals[key] = &evalGroup{time: key}
				}
				evals[key].ticks = append(evals[key].ticks, ti)
			}
		}
	}
	budgetOK := true
	wm := map[string]int{}
	for _, p := range projects {
		wm[p.Name] = p.Weight
	}
	for _, eg := range evals {
		totalW := 0
		for _, t := range eg.ticks {
			totalW += wm[t.proj]
		}
		if totalW > budget {
			budgetOK = false
		}
	}
	check("Budget never exceeded", budgetOK, fmt.Sprintf("%d eval groups checked", len(evals)))

	// 4. No duplicate spawns in same eval.
	dupOK := true
	dupCount := 0
	for _, eg := range evals {
		seen := map[string]bool{}
		for _, t := range eg.ticks {
			if seen[t.proj] {
				dupOK = false
				dupCount++
			}
			seen[t.proj] = true
		}
	}
	check("No duplicate spawns per eval", dupOK, fmt.Sprintf("%d duplicates across %d evals", dupCount, len(evals)))

	// 5. Session IDs captured.
	var noSid int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE (session_id IS NULL OR session_id = '') AND status != 'running'`).Scan(&noSid)
	check("Session IDs captured", noSid == 0, fmt.Sprintf("%d ticks without session ID", noSid))

	// 6. Priority ordering within budget (high priority ≤ low priority in spawn order).
	prioOK := true
	sort.Slice(projects, func(i, j int) bool { return projects[i].Priority > projects[j].Priority })
	for _, eg := range evals {
		lastPrio := 999
		for _, t := range eg.ticks {
			p := 0
			for _, proj := range projects {
				if proj.Name == t.proj {
					p = proj.Priority
					break
				}
			}
			if p > lastPrio {
				prioOK = false
			}
			lastPrio = p
		}
	}
	check("Priority descending within evals", prioOK, "higher priority projects spawned first")

	fmt.Printf("\n---\n%d checks, %d failures\n", checks, failures)
	if failures > 0 {
		fmt.Println("❌ VERIFY FAILED")
		return fmt.Errorf("%d/%d checks failed", failures, checks)
	}
	fmt.Println("✅ SCHEDULER VERIFIED")
	return nil
}
