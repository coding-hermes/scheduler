package dashboard

import (
	"context"
	"database/sql"
	"fmt"
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
}

// TickRow is one tick in the history table.
type TickRow struct {
	ID, Project, Status, Outcome, SessionID, SpawnedAt, CompletedAt string
	Commits, FilesChanged                                           int
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
	Project     *database.Project
	LatestTick  *database.Tick
	RecentTicks []database.Tick
}

// TickHistoryData holds one page of the global tick history.
type TickHistoryData struct {
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
	Namespace       *database.Namespace
	Projects        []database.Project
	RecentTicks     []database.NamespaceTick
	LatestTick      *database.NamespaceTick
	EnabledProjects int
	TotalWeight     int
	Utilization     float64
}

// HealthData holds daemon, database, and gateway liveness information.
type HealthData struct {
	GeneratedAt    string
	DaemonStatus   string
	DatabaseStatus string
	GatewayStatus  string
	GatewayURL     string
	Uptime         string
	ActiveTicks    int
	TotalTicks     int
	Goroutines     int
	MemoryMB       float64
}

func (g *Generator) collect(ctx context.Context) FleetData {
	data := FleetData{
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
	dayAgo := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	rows, err := g.db.QueryContext(ctx, projectQuery, dayAgo, weekAgo)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r FleetRow
			if err := rows.Scan(&r.Name, &r.Weight, &r.Priority, &r.Enabled,
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
			data.CostTodayTotal += r.CostToday
			data.CostWeekTotal += r.CostWeek
			data.Projects = append(data.Projects, r)
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
