package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
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
		// Each fixture project gets its own workdir — the case-insensitive
		// dup-workdir guard in CreateProject rejects enabled projects that
		// share a directory (DOGFOOD-002: sharing tmpDir left the harness
		// red for 50+ runs). The dirs must exist because spawns cd into them.
		workdir := tmpDir + "/" + p.Name
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", p.Name, err)
		}
		proj := &database.Project{
			Name:      p.Name,
			RepoURL:   "local:/test",
			Workdir:   workdir,
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
	loop.SetNoDeliver(true) // suppress Telegram spam during verify
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

	// 6. Priority ordering: the packer selects projects urgency-desc/
	// priority-desc, so the FIRST evaluation cycle of a fresh fleet must
	// contain the highest-priority projects that fit the budget. Per-cycle
	// spawn order cannot be checked via spawned_at because slot-pool
	// goroutines start concurrently (goroutine scheduling shuffles
	// sub-second spawn order — DOGFOOD-002 follow-up, 2026-08-04), so the
	// check compares set membership of the first cycle against the top
	// priorities by pack order.
	type weightPrio struct {
		name     string
		weight   int
		priority int
		urgency  float64
	}
	cands := make([]weightPrio, len(projects))
	for i, p := range projects {
		// urgency == priority when a project has never ticked (fresh fleet).
		cands[i] = weightPrio{p.Name, p.Weight, p.Priority, float64(p.Priority)}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].urgency != cands[j].urgency {
			return cands[i].urgency > cands[j].urgency
		}
		return cands[i].priority > cands[j].priority
	})
	// Greedy knapsack fill, same as the packer.
	wantFirst := map[string]bool{}
	usedW := 0
	for _, c := range cands {
		if len(wantFirst) >= maxConcur || usedW+c.weight > budget {
			continue
		}
		wantFirst[c.name] = true
		usedW += c.weight
	}
	// Find the earliest spawn second (first cycle) and its membership.
	firstCycle := ""
	if err := db.QueryRowContext(ctx, `
		SELECT MIN(substr(spawned_at, 1, 19)) FROM ticks
		WHERE status IN ('completed','failed','timeout') AND spawned_at != ''`).Scan(&firstCycle); err != nil {
		firstCycle = ""
	}
	gotFirst := map[string]bool{}
	if firstCycle != "" {
		fcRows, _ := db.QueryContext(ctx, `
			SELECT DISTINCT project_name FROM ticks
			WHERE substr(spawned_at, 1, 19) = ?`, firstCycle)
		if fcRows != nil {
			defer fcRows.Close()
			for fcRows.Next() {
				var n string
				fcRows.Scan(&n)
				gotFirst[n] = true
			}
		}
	}
	prioOK := true
	for n := range wantFirst {
		if !gotFirst[n] {
			prioOK = false
		}
	}
	for n := range gotFirst {
		if !wantFirst[n] {
			prioOK = false
		}
	}
	check("First cycle = highest-priority pack", prioOK,
		fmt.Sprintf("first cycle %s: %d/%d expected projects spawned", firstCycle, len(gotFirst), len(wantFirst)))

	// ── Performance audit (DOGFOOD-006) ─────────────────────────────────
	// Runs after the correctness checks so the seeded data cannot disturb
	// them. All output is informational — the pass/fail verdict above is
	// driven by the 6 correctness checks.
	//
	// 7. Seed a production-scale tick history so the latency and
	//    failure-rate measurements run at realistic row counts (the live
	//    fleet DB holds ~30k completed ticks). The mix is deliberately
	//    uneven: epsilon dominates the failed bucket the way starved
	//    projects do on the live board.
	type outcomeMix struct {
		completed int
		failed    int
	}
	mixes := []struct {
		name string
		mix  outcomeMix
	}{
		{"alpha", outcomeMix{2000, 200}},
		{"beta", outcomeMix{1800, 150}},
		{"gamma", outcomeMix{1500, 300}},
		{"delta", outcomeMix{1200, 800}},
		{"epsilon", outcomeMix{3000, 7000}}, // dominant offender
		{"zeta", outcomeMix{1500, 4000}},
		{"eta", outcomeMix{800, 4850}},
	}
	seeded := 0
	{
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("seed tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }() // no-op after successful Commit
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO ticks
			(id, project_name, session_id, status, outcome, spawned_at, completed_at, exit_code, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("seed prepare: %w", err)
		}
		base := time.Now().Add(-48 * time.Hour)
		i := 0
		for _, m := range mixes {
			for s := 0; s < m.mix.completed; s++ {
				spawned := base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
				if _, err := stmt.ExecContext(ctx, fmt.Sprintf("syn-%06d", i), m.name,
					fmt.Sprintf("sess-%06d", i), "completed", "committed", spawned,
					base.Add(time.Duration(i)*time.Minute+90*time.Second).UTC().Format(time.RFC3339), 0, spawned); err != nil {
					return fmt.Errorf("seed completed %s: %w", m.name, err)
				}
				i++
				seeded++
			}
			for s := 0; s < m.mix.failed; s++ {
				spawned := base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
				if _, err := stmt.ExecContext(ctx, fmt.Sprintf("syn-%06d", i), m.name,
					fmt.Sprintf("sess-%06d", i), "failed", "failed", spawned,
					base.Add(time.Duration(i)*time.Minute+90*time.Second).UTC().Format(time.RFC3339), 1, spawned); err != nil {
					return fmt.Errorf("seed failed %s: %w", m.name, err)
				}
				i++
				seeded++
			}
		}
		if err := stmt.Close(); err != nil {
			return fmt.Errorf("seed stmt close: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("seed commit: %w", err)
		}
	}
	fmt.Printf("\n── Performance audit (DOGFOOD-006) ──\n")
	fmt.Printf("seeded %d synthetic ticks at production scale\n", seeded)

	// 8. Read-endpoint latency: p50/p90/p99 over N requests through the
	//    real API handler. Spec §10 promises <100ms p99 on read endpoints.
	svr := httptest.NewServer(api.NewServer(db, loop).Handler())
	defer svr.Close()
	const nReq = 100
	probe := func(path string) []time.Duration {
		ds := make([]time.Duration, 0, nReq)
		for i := 0; i < nReq; i++ {
			t0 := time.Now()
			resp, err := http.Get(svr.URL + path)
			if err != nil {
				return nil
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			ds = append(ds, time.Since(t0))
		}
		return ds
	}
	printLatency := func(name string, ds []time.Duration) {
		if len(ds) == 0 {
			fmt.Printf("  %-24s FAILED (no responses)\n", name)
			return
		}
		p50, p90, p99, mx := latencyStats(ds)
		fmt.Printf("  %-24s n=%d  p50=%v  p90=%v  p99=%v  max=%v\n", name, len(ds), p50, p90, p99, mx)
		if p99 >= 100*time.Millisecond {
			fmt.Printf("    ⚠ p99 ≥ 100ms — spec §10 read-latency promise exceeded\n")
		}
	}
	projLat := probe("/api/v1/projects")
	statLat := probe("/api/v1/status")
	printLatency("GET /api/v1/projects", projLat)
	printLatency("GET /api/v1/status", statLat)

	// 9. Failure-rate breakdown by project — which projects dominate the
	//    failed bucket (the live-board open question: recent_outcomes shows
	//    a ~70-79% fleet failure rate). Same WHERE clause as
	//    countRecentOutcomes (completed_at IS NOT NULL), grouped by project.
	type projOutcome struct {
		name      string
		completed int
		failed    int
		timeout   int
	}
	var outcomes []projOutcome
	{
		rows, err := db.QueryContext(ctx, `
			SELECT project_name, status, COUNT(*)
			FROM ticks
			WHERE completed_at IS NOT NULL
			GROUP BY project_name, status
			ORDER BY project_name, status`)
		if err != nil {
			return fmt.Errorf("failure breakdown query: %w", err)
		}
		defer rows.Close()
		byName := map[string]*projOutcome{}
		for rows.Next() {
			var name, status string
			var n int
			if err := rows.Scan(&name, &status, &n); err != nil {
				return fmt.Errorf("failure breakdown scan: %w", err)
			}
			po, ok := byName[name]
			if !ok {
				po = &projOutcome{name: name}
				byName[name] = po
			}
			switch status {
			case "completed":
				po.completed = n
			case "failed":
				po.failed = n
			case "timeout":
				po.timeout = n
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("failure breakdown rows: %w", err)
		}
		for _, po := range byName {
			outcomes = append(outcomes, *po)
		}
		sort.Slice(outcomes, func(i, j int) bool {
			fi := outcomes[i].failed + outcomes[i].timeout
			fj := outcomes[j].failed + outcomes[j].timeout
			if fi != fj {
				return fi > fj
			}
			return outcomes[i].name < outcomes[j].name
		})
	}
	fmt.Printf("\n── Failure-rate breakdown by project (top offenders) ──\n")
	fmt.Printf("  %-12s %10s %10s %10s %8s\n", "project", "completed", "failed", "timeout", "fail%")
	totC, totF, totT := 0, 0, 0
	for _, po := range outcomes {
		tot := po.completed + po.failed + po.timeout
		if tot == 0 {
			continue
		}
		totC += po.completed
		totF += po.failed
		totT += po.timeout
		fmt.Printf("  %-12s %10d %10d %10d %7.1f%%\n", po.name,
			po.completed, po.failed, po.timeout,
			float64(po.failed+po.timeout)/float64(tot)*100)
	}
	fmt.Printf("  %-12s %10d %10d %10d %7.1f%%\n", "TOTAL",
		totC, totF, totT, float64(totF+totT)/float64(totC+totF+totT)*100)

	fmt.Printf("\n---\n%d checks, %d failures\n", checks, failures)
	if failures > 0 {
		fmt.Println("❌ VERIFY FAILED")
		return fmt.Errorf("%d/%d checks failed", failures, checks)
	}
	fmt.Println("✅ SCHEDULER VERIFIED")
	return nil
}

// latencyStats returns p50/p90/p99 and the max of a latency sample.
func latencyStats(ds []time.Duration) (p50, p90, p99, max time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0, 0
	}
	s := make([]time.Duration, len(ds))
	copy(s, ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	p := func(frac float64) time.Duration {
		return s[int(frac*float64(len(s)-1))]
	}
	return p(0.50), p(0.90), p(0.99), s[len(s)-1]
}
