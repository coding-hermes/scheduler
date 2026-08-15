package dashboard

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

// QueueEntry is one project in the evaluation queue view.
type QueueEntry struct {
	Name      string
	Weight    int
	Priority  int
	CooldownS int
	Enabled   bool
	Urgency   float64
}

// QueueData holds all data for the queue page.
type QueueData struct {
	Title       string
	Count       int
	TotalWeight int
	Entries     []QueueEntry
}

// FleetRow is one project in the fleet overview table.
type FleetRow struct {
	Name        string
	Weight      int
	Priority    int
	Enabled     bool
	LastTick    string
	LastOutcome string
	SessionID   string
	Urgency     float64
	RunningNow  int // 0 or 1; int avoids modernc.org/sqlite int→bool scan bug
	Completed   int
	Failed      int
	Timeout     int
	CostToday   float64
	CostWeek    float64
	// Board progress (parsed from <workdir>/.coding-hermes/tasks.md).
	Workdir           string
	CooldownS         int
	LastTickCompleted string
	BoardDone         int
	BoardTotal        int
	NextTickIn        string // human-readable "in Xm Ys", "running", "due now", or "—"
	// Recent cost series (last up-to-N completed ticks, oldest→newest) for the
	// cost sparkline, plus the count of recent failed/timeout ticks (failure flag).
	CostSeries     []float64
	RecentFailures int
	RecentTicks    int
	// Observability: average tick duration (seconds), success rate (0-100),
	// and estimated time-to-completion (from avg duration × steps left).
	AvgTickSecs int
	SuccessRate int // percent
	ETA         string
	// CompletionAt is the projected wall-clock completion as RFC3339 (UTC);
	// the dashboard renders it in the viewer's local timezone via JS.
	CompletionAt string
	// ProjectedCost is the estimated remaining cost to finish the board
	// (avg cost per completed tick × steps remaining).
	ProjectedCost float64
	// AvgCost is the mean cost per completed tick, used for live-cost estimate
	// of running ticks.
	AvgCost float64
	// EtaBreakdown is the learning-predictor per-type estimate, e.g.
	// "code ×2 40m + test ×5 25m" (empty when no signal).
	EtaBreakdown string
	// GitReins LLM-judge verdict pass rate (0-100) over the project history.
	GitReinsPass int // percent; -1 = no verdicts
	// CIConclusion is the latest GitHub Actions run conclusion (success/failure/
	// "" ) for the project's repo — an INDEPENDENT cross-check on GitReins. A
	// GitReins 100% is only trustworthy when CI is also green; a red CI flags
	// that the LLM-judge gate may be passing a suite that is actually failing
	// (e.g. cached test results). "" = unknown (no CI workflow or query failed).
	CIConclusion string
}

// TickRow is one tick in the history table.
type TickRow struct {
	ID, Project, Status, Outcome, SessionID, SpawnedAt, CompletedAt string
	Commits, FilesChanged                                           int
	Duration                                                        string // human-readable elapsed time between spawned and completed
}

// NamespaceRow is one namespace in the allocation overview table.
type NamespaceRow struct {
	ID           string
	Weight       int
	Reserved     int
	HardCap      int
	Allocated    int
	Used         int
	Borrowed     int
	Lent         int
	ProjectCount int
	Utilization  float64
}

// NamespaceTickRow is one namespace_tick in the utilization history table.
type NamespaceTickRow struct {
	TickGroup   string
	NamespaceID string
	Allocated   int
	Used        int
	Borrowed    int
	Lent        int
	CreatedAt   string
}

// FleetData holds all data for the dashboard.
type FleetData struct {
	Title           string
	GeneratedAt     string
	BudgetTotal     int
	BudgetUsed      int
	ActiveTicks     int
	TotalProjects   int
	EnabledProjects int
	Projects        []FleetRow
	RecentTicks     []TickRow
	Namespaces      []NamespaceRow
	NamespaceTicks  []NamespaceTickRow
	CostTodayTotal  float64
	CostWeekTotal   float64
}

// ProjectDetailData holds all data for the /projects/{name} page.
type ProjectDetailData struct {
	Title         string
	Project       *database.Project
	LatestTick    *database.Tick
	RecentTicks   []database.Tick
	BoardDone     int
	BoardTotal    int
	NextTickIn    string
	AvgTickSecs   int
	SuccessRate   int
	ETA           string
	BoardSteps    []BoardStep
	TickWork      map[string]string // tick id → what it worked on (commit subjects)
	GitReins      GitReinsSummary
	CompletionAt  string
	ProjectedCost float64
	AvgCost       float64          // mean cost per completed tick (for live-cost estimate)
	EtaBreakdown  string           // per-type estimate, e.g. "code ×2 40m + test ×5 25m"
	SpeedCost     []SpeedCostPoint // for the speed/cost-over-time charts
}

// BoardStep is one task row from the board, for the roadmap visualization.
type BoardStep struct {
	ID     string
	Title  string
	Status string // "done" | "active" | "pending"
	Commit string
}

// GitReinsVerdict is one LLM-judge verdict from .gitreins/history.
type GitReinsVerdict struct {
	TaskID      string
	TaskTitle   string
	Passed      bool
	Tier1Passed bool
	Tier2Passed bool
	HasTier2    bool
	EvaluatedAt string
}

// GitReinsSummary is the aggregate pass rate + latest verdicts for a project.
type GitReinsSummary struct {
	Total   int
	Passed  int
	Failed  int
	RatePct int
	Latest  []GitReinsVerdict // newest first, capped
}

// TickHistoryData holds one page of the global tick history.
type TickHistoryData struct {
	Title        string
	GeneratedAt  string
	Ticks        []database.Tick
	Page         int
	PageSize     int
	TotalTicks   int
	TotalPages   int
	HasPrevious  bool
	PreviousPage int
	HasNext      bool
	NextPage     int
}

// NamespaceViewData holds namespace configuration, projects, and recent
// allocation history for /namespaces/{id}.
type NamespaceViewData struct {
	Title           string
	Namespace       *database.Namespace
	Projects        []database.Project
	RecentTicks     []database.NamespaceTick
	LatestTick      *database.NamespaceTick
	EnabledProjects int
	TotalWeight     int
	Utilization     float64
}

// HealthData holds daemon, database, gateway, and DuckBrain liveness info.
type HealthData struct {
	Title            string
	GeneratedAt      string
	DaemonStatus     string
	DatabaseStatus   string
	GatewayStatus    string
	GatewayURL       string
	DuckBrainStatus  string
	DuckBrainBaseURL string
	DuckBrainSpooled int
	Uptime           string
	ActiveTicks      int
	TotalTicks       int
	Goroutines       int
	MemoryMB         float64
}

func (g *Generator) collect(ctx context.Context) FleetData {
	data := FleetData{
		Title:       "Fleet Overview",
		GeneratedAt: time.Now().Format(time.RFC3339),
		BudgetTotal: 100,
	}

	// ── Projects: batch query with per-project stats via LEFT JOINs ──
	// Single query replaces 7 per-project queries (N+1 → 1).
	// Note: outcome and session_id are fetched via a SECOND LEFT JOIN to ticks
	// (t2) rather than correlated subqueries — SQLite's modernc driver rejects
	// MAX() references inside correlated subqueries ("misuse of aggregate").
	projectQuery := `
		SELECT
			p.name, p.weight, p.priority, p.enabled,
			COALESCE(p.workdir, '')            AS workdir,
			COALESCE(p.cooldown_s, 900)        AS cooldown_s,
			COALESCE(p.last_tick_completed, '') AS last_tick_completed,
			COALESCE(t.spawned_at, '')            AS last_tick,
			COALESCE(t2.outcome, '')               AS last_outcome,
			COALESCE(t2.session_id, '')            AS session_id,
			COALESCE(t.running, 0) > 0             AS running_now,
			COALESCE(t.completed, 0)               AS completed,
			COALESCE(t.failed, 0)                  AS failed,
			COALESCE(t.timed_out, 0)              AS timed_out,
			COALESCE(t.cost_today, 0.0)            AS cost_today,
			COALESCE(t.cost_week, 0.0)             AS cost_week
		FROM projects p
		LEFT JOIN (
			SELECT
				tk.project_name,
				MAX(tk.spawned_at) AS spawned_at,
				SUM(CASE WHEN tk.status = 'running'   THEN 1 ELSE 0 END) AS running,
				SUM(CASE WHEN tk.status = 'completed' THEN 1 ELSE 0 END) AS completed,
				SUM(CASE WHEN tk.status = 'failed'    THEN 1 ELSE 0 END) AS failed,
				SUM(CASE WHEN tk.status = 'timeout'   THEN 1 ELSE 0 END) AS timed_out,
				COALESCE(SUM(CASE WHEN tk.status = 'completed' AND tk.completed_at >= ? THEN tk.cost_usd ELSE 0 END), 0.0) AS cost_today,
				COALESCE(SUM(CASE WHEN tk.status = 'completed' AND tk.completed_at >= ? THEN tk.cost_usd ELSE 0 END), 0.0) AS cost_week
			FROM ticks tk
			GROUP BY tk.project_name
		) t ON t.project_name = p.name
		LEFT JOIN ticks t2 ON t2.project_name = t.project_name AND t2.spawned_at = t.spawned_at
		ORDER BY p.name
	`
	// completed_at is stored as UTC RFC3339 (nowUTC in database/ticks.go), so
	// the window bounds must be UTC too — comparing local-offset strings
	// lexicographically against UTC strings mis-counts ticks near the boundary
	// by the server's UTC offset.
	dayAgo := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	weekAgo := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	rows, err := g.db.QueryContext(ctx, projectQuery, dayAgo, weekAgo)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r FleetRow
			if err := rows.Scan(&r.Name, &r.Weight, &r.Priority, &r.Enabled,
				&r.Workdir, &r.CooldownS, &r.LastTickCompleted,
				&r.LastTick, &r.LastOutcome, &r.SessionID,
				&r.RunningNow, &r.Completed, &r.Failed, &r.Timeout,
				&r.CostToday, &r.CostWeek); err != nil {
				continue
			}
			data.TotalProjects++
			if r.Enabled {
				data.EnabledProjects++
				data.BudgetUsed += r.Weight
			}
			// Urgency: priority * (1 + hours since last tick)
			if r.LastTick != "" {
				if t, err := time.Parse(time.RFC3339, r.LastTick); err == nil {
					r.Urgency = float64(r.Priority) * (1 + time.Since(t).Hours())
				}
			}
			// Board progress (done/total) from the project's tasks.md, plus the
			// human-readable countdown to the next tick.
			if r.Workdir != "" {
				r.BoardDone, r.BoardTotal = readBoardProgress(filepath.Join(r.Workdir, ".coding-hermes", "tasks.md"))
			}
			r.NextTickIn = nextTickIn(r.RunningNow == 1, r.LastTickCompleted, r.CooldownS)
			data.CostTodayTotal += r.CostToday
			data.CostWeekTotal += r.CostWeek
			data.Projects = append(data.Projects, r)
		}
	}

	// Second pass for cost sparklines + recent failure flags + observability.
	// Done AFTER the project rows cursor is fully closed — the modernc.org/sqlite
	// driver deadlocks if we open nested queries on the same connection while a
	// rows cursor is still open (the collect() N+1 warning).
	// The fleet-wide learned prior is built ONCE and shared across all projects.
	fleet := g.fleetLearned(ctx)
	for i := range data.Projects {
		r := &data.Projects[i]
		r.CostSeries = g.recentCostSeries(ctx, r.Name, 12)
		r.RecentTicks, r.RecentFailures = g.recentTickHealth(ctx, r.Name, 10)
		r.AvgTickSecs, r.AvgCost, r.SuccessRate, r.ETA, r.CompletionAt, r.ProjectedCost = g.observabilityStats(ctx, r.Name, r.BoardDone, r.BoardTotal, r.RecentTicks, r.RecentFailures)
		// Learning ETA: predict remaining time + cost from per-task-type
		// estimates learned from tick history + the fleet-wide prior.
		if r.Workdir != "" {
			steps := readBoardSteps(filepath.Join(r.Workdir, ".coding-hermes", "tasks.md"))
			if learned, learnedAt, breakdown, projCost := g.learnedETA(ctx, r.Name, r.Workdir, steps, fleet); learned > 0 {
				r.ETA = formatETA(learned)
				r.CompletionAt = learnedAt
				r.EtaBreakdown = breakdown
				if projCost > 0 {
					r.ProjectedCost = projCost
				}
			}
		}
		r.GitReinsPass = -1
		if r.Workdir != "" {
			if gr := readGitReins(r.Workdir, 0); gr.Total > 0 {
				r.GitReinsPass = gr.RatePct
			}
			r.CIConclusion = ciConclusion(r.Workdir)
		}
	}

	// Active ticks count.
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&data.ActiveTicks)

	// Recent ticks.
	tickRows, _ := g.db.QueryContext(ctx, `SELECT id, project_name, status, COALESCE(outcome,''), COALESCE(session_id,''), spawned_at, COALESCE(completed_at,''), commits, files_changed FROM ticks ORDER BY spawned_at DESC LIMIT 20`)
	if tickRows != nil {
		defer tickRows.Close()
		for tickRows.Next() {
			var t TickRow
			_ = tickRows.Scan(&t.ID, &t.Project, &t.Status, &t.Outcome, &t.SessionID, &t.SpawnedAt, &t.CompletedAt, &t.Commits, &t.FilesChanged)
			t.Duration = tickDuration(t.SpawnedAt, t.CompletedAt)
			data.RecentTicks = append(data.RecentTicks, t)
		}
	}

	// Namespaces — batch latest ticks + project counts to avoid N+1.
	namespaces, err := database.ListNamespaces(ctx, g.db, false)
	if err == nil && len(namespaces) > 0 {
		// Batch 1: latest namespace_tick per namespace (1 query, not N).
		type nsTickVal struct {
			allocated, used, borrowed, lent int
		}
		latestTicks := make(map[string]nsTickVal)
		tickRows, terr := g.db.QueryContext(ctx, `
			SELECT nt.namespace_id, nt.allocated, nt.used, nt.borrowed, nt.lent
			FROM namespace_ticks nt
			INNER JOIN (
				SELECT namespace_id, MAX(created_at) AS max_created
				FROM namespace_ticks
				GROUP BY namespace_id
			) latest ON nt.namespace_id = latest.namespace_id AND nt.created_at = latest.max_created
		`)
		if terr == nil {
			defer tickRows.Close()
			for tickRows.Next() {
				var nsID string
				var v nsTickVal
				if tickRows.Scan(&nsID, &v.allocated, &v.used, &v.borrowed, &v.lent) == nil {
					latestTicks[nsID] = v
				}
			}
		}

		// Batch 2: enabled project count per namespace (1 query, not N).
		projectCounts := make(map[string]int)
		countRows, cerr := g.db.QueryContext(ctx, `
			SELECT namespace_id, COUNT(*) FROM projects WHERE enabled=1 GROUP BY namespace_id
		`)
		if cerr == nil {
			defer countRows.Close()
			for countRows.Next() {
				var nsID string
				var cnt int
				if countRows.Scan(&nsID, &cnt) == nil {
					projectCounts[nsID] = cnt
				}
			}
		}

		for _, ns := range namespaces {
			row := NamespaceRow{
				ID:       ns.ID,
				Weight:   ns.Weight,
				Reserved: ns.Reserved,
				HardCap:  ns.HardCap,
			}
			if v, ok := latestTicks[ns.ID]; ok {
				row.Allocated = v.allocated
				row.Used = v.used
				row.Borrowed = v.borrowed
				row.Lent = v.lent
			}
			if row.Allocated > 0 {
				row.Utilization = float64(row.Used) / float64(row.Allocated) * 100
			}
			row.ProjectCount = projectCounts[ns.ID]
			data.Namespaces = append(data.Namespaces, row)
		}
	}

	// Recent namespace ticks for the utilization chart.
	nsTickRows, _ := g.db.QueryContext(ctx, `SELECT tick_group, namespace_id, allocated, used, borrowed, lent, created_at FROM namespace_ticks ORDER BY created_at DESC LIMIT 100`)
	if nsTickRows != nil {
		defer nsTickRows.Close()
		for nsTickRows.Next() {
			var nt NamespaceTickRow
			_ = nsTickRows.Scan(&nt.TickGroup, &nt.NamespaceID, &nt.Allocated, &nt.Used, &nt.Borrowed, &nt.Lent, &nt.CreatedAt)
			data.NamespaceTicks = append(data.NamespaceTicks, nt)
		}
	}

	return data
}

// latestTickForProject returns the most recently spawned tick for the project,
// or nil if the project has never been scheduled. Implementation lives here
// (not in the database package) to avoid widening the db API for a single
// dashboard caller; the SQL is a single indexed row lookup.
func latestTickForProject(ctx context.Context, db *sql.DB, projectName string) (*database.Tick, error) {
	const q = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, COALESCE(error,''), created_at
FROM ticks WHERE project_name = ?
ORDER BY spawned_at DESC LIMIT 1`
	var t database.Tick
	var status, outcome string
	err := db.QueryRowContext(ctx, q, projectName).Scan(
		&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed,
		&t.Error, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // no ticks yet — not an error for the dashboard
	}
	if err != nil {
		return nil, fmt.Errorf("latest tick for %q: %w", projectName, err)
	}
	t.Status = database.TickStatus(status)
	t.Outcome = database.TickOutcome(outcome)
	return &t, nil
}

// readBoardProgress counts task rows in a coding-hermes board file. It
// returns (done, total). Board format (model-router matrix):
//
//	## Active          → table rows "| T06 | ..." are PENDING tasks
//	## Completed       → table rows "| T05 | ..." are DONE tasks
//	## [ ] NEVER-DONE  → perpetual audit; NOT counted (never completes)
//
// A task row is any line starting with "| T" (or "| T00"-style task id) inside
// the Active or Completed section. Returns (0,0) if the board is missing or
// unreadable, so the dashboard degrades gracefully.
func readBoardProgress(path string) (done, total int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	section := "" // "active" | "completed" | other
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "## "):
			low := strings.ToLower(line)
			switch {
			case strings.Contains(low, "active"):
				section = "active"
			case strings.Contains(low, "completed"):
				section = "completed"
			default:
				// NEVER-DONE or any other section — not counted.
				section = "other"
			}
		case isTaskRow(line):
			// Task row. The NEVER-DONE line ("## [ ] NEVER-DONE") is a heading,
			// not a task row, so it never reaches here.
			switch section {
			case "active":
				total++
				// Markdown checklist item "- [x]" in an active section is done.
				if strings.HasPrefix(line, "- [x] ") {
					done++
				}
			case "completed":
				done++
				total++
			}
		}
	}
	return done, total
}

// trailingCommitRe matches a trailing "(<commit_hash>[, ...])" reference in a
// markdown checklist title, e.g. "(b0420b9, 2026-08-08)" or "(3cd9b0e)". Used
// by readBoardSteps to extract the commit for done steps (gitreins2 boards).
var trailingCommitRe = regexp.MustCompile(`\(([0-9a-f]{7}|[0-9a-f]{40})[,\)]`)

// isTaskRow reports whether a trimmed line is a task row — either a table row
// ("| T05 | ...") or a markdown task-list item ("- [ ] T05 ..." / "- [x] T05 ...").
// This lets the board parser count any task-prefix (T##, V01, DOCS-###, R2-##)
// in either board format. (Markdown checklist support added 2026-08-08 — some
// boards, e.g. gitreins2, track tasks as "- [x] R2-1 ..." rather than table rows.)
func isTaskRow(line string) bool {
	// Markdown task-list item: "- [ ] <ID>" or "- [x] <ID>".
	if strings.HasPrefix(line, "- [ ] ") || strings.HasPrefix(line, "- [x] ") {
		rest := line[6:] // strip "- [ ] " (6 chars) — covers both "[ ]" and "[x]"
		// ID is everything up to the first space or tab.
		end := strings.IndexAny(rest, " 	")
		if end <= 0 {
			return false
		}
		return isTaskID(rest[:end])
	}
	// Table row: "| <ID> |".
	if !strings.HasPrefix(line, "| ") {
		return false
	}
	rest := strings.TrimPrefix(line, "| ")
	idx := strings.Index(rest, " |")
	if idx <= 0 {
		return false
	}
	return isTaskID(rest[:idx])
}

// isTaskID reports whether s looks like a task identifier: 2+ chars of
// uppercase letters, digits, hyphens (e.g. T05, V01, DOCS-000, E2E-001).
// The first char must be a letter so the "---" table separator is excluded,
// and the literal header id "ID" is rejected so the table header row is not
// counted as a task.
func isTaskID(s string) bool {
	if len(s) < 2 || s == "ID" || !isUpperLetter(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isUpperLetter(c) && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isUpperLetter(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

// nextTickIn returns a human-readable countdown to the next tick, or a
// status string. running=true means a tick is in flight now. Otherwise the
// next tick is due cooldownS after the last tick completed.
func nextTickIn(running bool, lastTickCompleted string, cooldownS int) string {
	if running {
		return "running"
	}
	if cooldownS <= 0 {
		cooldownS = 900
	}
	if lastTickCompleted == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, lastTickCompleted)
	if err != nil {
		return "—"
	}
	due := t.Add(time.Duration(cooldownS) * time.Second)
	wait := time.Until(due)
	if wait <= 0 {
		return "due now"
	}
	m := int(wait.Minutes())
	s := int(wait.Seconds()) % 60
	return fmt.Sprintf("in %dm %ds", m, s)
}

// SpeedCostPoint is one completed tick's (time, speed, cost, output) data point
// for the per-project speed/cost/commits/files charts.
type SpeedCostPoint struct {
	Label    string // "14:12", "16:44", ...
	Duration int    // tick duration in seconds (speed)
	Cost     float64
	Commits  int
	Files    int
}

// speedCostSeries returns the last up-to-n completed ticks for a project,
// oldest→newest, as (label, durationSecs, cost, commits, files) points. Used to
// draw the speed/cost/commits/files-over-time charts. Returns nil on error /
// empty. Only ticks that produced output (commits>0 || files>0 || cost>0) are
// included — pure timeouts/failures (0/0/0) are omitted since they output
// nothing meaningful.
func (g *Generator) speedCostSeries(ctx context.Context, project string, n int) []SpeedCostPoint {
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at, cost_usd, commits, files_changed FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type rev struct {
		label    string
		duration int
		cost     float64
		commits  int
		files    int
	}
	var reversed []rev
	for rows.Next() {
		var sp, co string
		var cost float64
		var commits, files int
		if rows.Scan(&sp, &co, &cost, &commits, &files) != nil {
			continue
		}
		// Skip zero-output ticks (nothing committed / no files changed / no cost).
		if commits <= 0 && files <= 0 && cost <= 0 {
			continue
		}
		d := parseDuration(sp, co)
		if d <= 0 {
			continue
		}
		label := ""
		if t, err := time.Parse(time.RFC3339, sp); err == nil {
			label = t.Local().Format("15:04")
		}
		reversed = append(reversed, rev{label: label, duration: int(d.Seconds()), cost: cost, commits: commits, files: files})
	}
	if len(reversed) == 0 {
		return nil
	}
	// Reverse to oldest→newest.
	out := make([]SpeedCostPoint, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, SpeedCostPoint{Label: reversed[i].label, Duration: reversed[i].duration, Cost: reversed[i].cost, Commits: reversed[i].commits, Files: reversed[i].files})
	}
	return out
}

// recentCostSeries returns the cost_usd of the last up-to-n completed ticks
// for a project, oldest→newest, for the cost sparkline. Returns nil on error.
func (g *Generator) recentCostSeries(ctx context.Context, project string, n int) []float64 {
	rows, err := g.db.QueryContext(ctx, `
		SELECT cost_usd FROM ticks
		WHERE project_name = ? AND status = 'completed'
		ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	// Collect newest→oldest, then reverse.
	rev := []float64{}
	for rows.Next() {
		var c float64
		if rows.Scan(&c) == nil {
			rev = append(rev, c)
		}
	}
	out := make([]float64, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// recentTickHealth returns (totalRecent, failedRecent) for a project over the
// last n ticks (any status), used for the failure flag. A failedRecent > 0
// lets the dashboard highlight a project with recent failed/timeout ticks.
func (g *Generator) recentTickHealth(ctx context.Context, project string, n int) (total, failed int) {
	rows, err := g.db.QueryContext(ctx, `
		SELECT status FROM ticks
		WHERE project_name = ? ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		if rows.Scan(&status) != nil {
			continue
		}
		total++
		if status == "failed" || status == "timeout" {
			failed++
		}
	}
	return total, failed
}

// tickDuration returns the human-readable elapsed time between spawned_at and
// completed_at, or "" when either is missing (still running / not finished).
func tickDuration(spawned, completed string) string {
	if spawned == "" || completed == "" {
		return ""
	}
	s, err1 := time.Parse(time.RFC3339, spawned)
	c, err2 := time.Parse(time.RFC3339, completed)
	if err1 != nil || err2 != nil {
		return ""
	}
	d := c.Sub(s)
	if d < 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// observabilityStats returns (avgSecs, avgCost, successPct, eta, completionAt, projectedCost)
// for a project over its recent completed ticks.
//   - avgSecs: mean duration of the last N completed ticks (floored at 60s)
//   - avgCost: mean cost of the last N completed ticks
//   - successPct: % of last N ticks that completed (vs failed/timeout)
//   - eta: avg duration × remaining board steps ("" when no signal)
//   - completionAt: UTC RFC3339 of now + eta ("" when no eta)
//   - projectedCost: avg cost per completed tick × remaining steps
func (g *Generator) observabilityStats(ctx context.Context, project string, boardDone, boardTotal int, recentTicks, recentFailures int) (avgSecs int, avgCost float64, successPct int, eta, completionAt string, projectedCost float64) {
	// Average duration + cost over up-to-10 completed ticks.
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at, cost_usd FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 10
	`, project)
	var total time.Duration
	var totalCost float64
	var count int
	if err == nil {
		for rows.Next() {
			var sp, co string
			var cost float64
			if rows.Scan(&sp, &co, &cost) == nil {
				if d := parseDuration(sp, co); d > 0 {
					total += d
					totalCost += cost
					count++
				}
			}
		}
		_ = rows.Close()
	}
	if count > 0 {
		avgSecs = int(total.Seconds() / float64(count))
		if avgSecs < 60 {
			avgSecs = 60 // floor so ETA isn't absurdly short
		}
		avgCost = totalCost / float64(count)
	}

	// Success rate over the last N ticks (recentTicks = total, recentFailures = bad).
	if recentTicks > 0 {
		successPct = (recentTicks - recentFailures) * 100 / recentTicks
	}

	// ETA + completion timestamp + projected cost from steps remaining.
	remaining := 0
	if boardTotal > 0 {
		remaining = boardTotal - boardDone
	}
	if remaining > 0 {
		if avgSecs > 0 {
			// avgSecs is in seconds; convert to a Duration properly.
			d := time.Duration(avgSecs) * time.Second * time.Duration(remaining)
			eta = formatETA(d)
			completionAt = time.Now().UTC().Add(d).Format(time.RFC3339)
		}
		if avgCost > 0 {
			projectedCost = avgCost * float64(remaining)
		}
	}
	return avgSecs, avgCost, successPct, eta, completionAt, projectedCost
}

func parseDuration(spawned, completed string) time.Duration {
	s, err1 := time.Parse(time.RFC3339, spawned)
	c, err2 := time.Parse(time.RFC3339, completed)
	if err1 != nil || err2 != nil {
		return 0
	}
	d := c.Sub(s)
	if d < 0 {
		return 0
	}
	return d
}

// formatETA renders a duration as a compact human string, e.g. "1h 24m",
// "2d 3h", "3w 2d". Falls back to "—" for zero/negative.
func formatETA(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	const (
		day  = 24 * time.Hour
		week = 7 * day
	)
	switch {
	case d >= week:
		return fmt.Sprintf("%dw %dd", int(d/week), int(d%week/day))
	case d >= day:
		return fmt.Sprintf("%dd %dh", int(d/day), int(d%day/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// ciConclusion returns the latest GitHub Actions run conclusion for the repo
// at workdir (success / failure / "" when unknown). It is an independent
// cross-check on the GitReins LLM-judge pass rate: the judge can report a
// cached or LLM-asserted "green" that does not match a genuinely failing
// suite, and a red CI is the ground truth that unmasks it. Best-effort — on
// any error (no gh, no workflow, timeout) it returns "" so the dashboard
// degrades gracefully.
func ciConclusion(workdir string) string {
	if workdir == "" {
		return ""
	}
	// gh has no -C dir flag (that's git); set the subprocess working dir.
	cmd := exec.Command("gh", "run", "list", "--limit", "1",
		"--json", "conclusion,status,headBranch",
		"--jq", `.[0].conclusion`)
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "null" {
		return ""
	}
	// In-progress runs have conclusion=null; treat as unknown rather than
	// green/red so we don't mislabel a running CI.
	return s
}

// readGitReins walks a project's .gitreins/history and returns the aggregate
// LLM-judge verdict summary (pass rate + latest verdicts). Each verdict is a
// .gitreins/history/<YYYY-MM-DD>/<sha>/verdict.json. Best-effort: malformed
// files are skipped; a missing/empty history yields a zero summary.
func readGitReins(workdir string, maxLatest int) GitReinsSummary {
	root := filepath.Join(workdir, ".gitreins", "history")
	var sum GitReinsSummary
	var all []GitReinsVerdict

	// dateDir/verdictDir/verdict.json
	dateDirs, err := os.ReadDir(root)
	if err != nil {
		return sum
	}
	for _, dd := range dateDirs {
		if !dd.IsDir() {
			continue
		}
		verdictDirs, err := os.ReadDir(filepath.Join(root, dd.Name()))
		if err != nil {
			continue
		}
		for _, vd := range verdictDirs {
			if !vd.IsDir() {
				continue
			}
			p := filepath.Join(root, dd.Name(), vd.Name(), "verdict.json")
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var raw struct {
				TaskID    string `json:"task_id"`
				TaskTitle string `json:"task_title"`
				Passed    bool   `json:"passed"`
				Evaluated string `json:"evaluated_at"`
				Stages    struct {
					Tier1 *struct {
						Passed bool `json:"passed"`
					} `json:"tier1"`
					Tier2 *struct {
						Passed bool `json:"passed"`
					} `json:"tier2"`
				} `json:"stages"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
			v := GitReinsVerdict{
				TaskID:    raw.TaskID,
				TaskTitle: raw.TaskTitle,
				Passed:    raw.Passed,
			}
			if raw.Stages.Tier1 != nil {
				v.Tier1Passed = raw.Stages.Tier1.Passed
			}
			if raw.Stages.Tier2 != nil {
				v.Tier2Passed = raw.Stages.Tier2.Passed
				v.HasTier2 = true
			}
			if raw.Evaluated != "" {
				v.EvaluatedAt = raw.Evaluated
			} else {
				v.EvaluatedAt = dd.Name()
			}
			sum.Total++
			if v.Passed {
				sum.Passed++
			} else {
				sum.Failed++
			}
			all = append(all, v)
		}
	}
	if sum.Total > 0 {
		sum.RatePct = sum.Passed * 100 / sum.Total
	}
	// Newest first: sort by evaluatedAt desc (string compare works for ISO).
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[j].EvaluatedAt > all[i].EvaluatedAt {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if maxLatest > 0 && len(all) > maxLatest {
		all = all[:maxLatest]
	}
	sum.Latest = all
	return sum
}

// tickWork returns the commit subject lines that landed between spawned and
// completed for a project, by scanning the workdir git log. It's the
// observability answer to "what did this tick actually work on?" Best-effort:
// on any git error it returns "". commitCount caps how many messages we fetch.
func tickWork(workdir, spawned, completed string, commitCount int) string {
	if workdir == "" || spawned == "" {
		return ""
	}
	// If completed is empty, only show commits strictly after spawned.
	since, err1 := time.Parse(time.RFC3339, spawned)
	if err1 != nil {
		return ""
	}
	var until time.Time
	if completed != "" {
		until, err1 = time.Parse(time.RFC3339, completed)
		if err1 != nil {
			until = time.Now()
		}
	} else {
		until = time.Now()
	}
	if until.Before(since) {
		until = time.Now()
	}

	// git log --pretty=%s (subject only) with `--since`/`--until` in ISO.
	args := []string{
		"-C", workdir, "log",
		"--since=" + since.Add(-2*time.Second).Format(time.RFC3339),
		"--until=" + until.Add(2*time.Second).Format(time.RFC3339),
		"--pretty=%s", "-n", fmt.Sprintf("%d", commitCount),
	}
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, " · ")
}

// readBoardSteps parses a board into an ordered roadmap of steps (completed
// first, then pending). The first pending task is marked "active" (next up).
// The NEVER-DONE perpetual audit is excluded. Returns nil on missing/unreadable.
func readBoardSteps(path string) []BoardStep {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	type row struct {
		id, title, commit string
	}
	var doneRows, pendingRows []row
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "## "):
			low := strings.ToLower(line)
			switch {
			case strings.Contains(low, "active"):
				section = "active"
			case strings.Contains(low, "completed"):
				section = "completed"
			default:
				section = "other"
			}
		case isTaskRow(line):
			// Two formats:
			//  Table row:       | T05 | Title | ... | commit |
			//  Markdown checklist: - [x] R2-1 Title ... (commit)
			// (markdown checklist support added 2026-08-08 — gitreins2 boards)
			if strings.HasPrefix(line, "- [ ] ") || strings.HasPrefix(line, "- [x] ") {
				isDone := strings.HasPrefix(line, "- [x] ")
				rest := line[6:]
				// id = first token, title = remainder, commit = 7/40-hex in parens.
				end := strings.IndexAny(rest, " 	")
				if end <= 0 {
					continue
				}
				id := rest[:end]
				title := strings.TrimSpace(rest[end:])
				// Strip a trailing "(<hex>[, ...])" commit reference.
				commit := ""
				if cm := trailingCommitRe.FindStringSubmatch(title); cm != nil {
					commit = cm[1]
					title = strings.TrimSpace(strings.TrimSuffix(title, cm[0]))
				}
				if id == "" {
					continue
				}
				switch section {
				case "active":
					if isDone {
						doneRows = append(doneRows, row{id: id, title: title, commit: commit})
					} else {
						pendingRows = append(pendingRows, row{id: id, title: title, commit: commit})
					}
				case "completed":
					doneRows = append(doneRows, row{id: id, title: title, commit: commit})
				}
				continue
			}
			// Table row: | T05 | Title | ... |
			// cols[1]=ID, cols[2]=title. Only COMPLETED rows carry a commit
			// hash in a trailing cell; Active/pending rows have deps + model
			// names (e.g. "GLM-5.2") that look hash-like, so don't guess there.
			cols := strings.Split(line, "|")
			var id, title, commit string
			if len(cols) > 1 {
				id = strings.TrimSpace(cols[1])
			}
			if len(cols) > 2 {
				title = strings.TrimSpace(cols[2])
			}
			if section == "completed" {
				for i := len(cols) - 1; i >= 3; i-- {
					c := strings.TrimSpace(cols[i])
					if c != "" && (len(c) == 7 || len(c) == 40) {
						commit = c
						break
					}
				}
			}
			if id == "" {
				continue
			}
			r := row{id: id, title: title, commit: commit}
			switch section {
			case "active":
				pendingRows = append(pendingRows, r)
			case "completed":
				doneRows = append(doneRows, r)
			}
		}
	}
	if len(doneRows) == 0 && len(pendingRows) == 0 {
		return nil
	}
	// Order: completed first (in board order), then pending.
	out := make([]BoardStep, 0, len(doneRows)+len(pendingRows))
	for _, r := range doneRows {
		out = append(out, BoardStep{ID: r.id, Title: r.title, Status: "done", Commit: r.commit})
	}
	for i, r := range pendingRows {
		status := "pending"
		if i == 0 {
			status = "active" // next up
		}
		out = append(out, BoardStep{ID: r.id, Title: r.title, Status: status, Commit: r.commit})
	}
	return out
}
