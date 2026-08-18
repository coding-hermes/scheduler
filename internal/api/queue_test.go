package api_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/api"
	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// queueRow mirrors the JSON shape of a /api/v1/queue entry.
type queueRow struct {
	Project   string  `json:"project"`
	Urgency   float64 `json:"urgency"`
	Weight    int     `json:"weight"`
	Priority  int     `json:"priority"`
	CooldownS int     `json:"cooldown_s"`
	Enabled   bool    `json:"enabled"`
}

// mustSeedQueueProject inserts an enabled project with known
// created_at / last_tick_completed timestamps, mirroring the inputs the
// scheduler engine's Pick path uses for ComputeUrgency.
func mustSeedQueueProject(t *testing.T, a *apiTestServer, name string, priority int, createdAt, lastCompleted time.Time) {
	t.Helper()
	if err := database.CreateProject(context.Background(), a.db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  priority,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
	if _, err := a.db.Exec(`UPDATE projects SET created_at = ?, last_tick_completed = ? WHERE name = ?`,
		createdAt.Format(time.RFC3339), lastCompleted.Format(time.RFC3339), name); err != nil {
		t.Fatalf("stamp %s timestamps: %v", name, err)
	}
}

func rowByName(rows []queueRow, name string) queueRow {
	for _, r := range rows {
		if r.Project == name {
			return r
		}
	}
	return queueRow{}
}

// TestAPI_Queue_UrgencyEngineFormula (GAP-054) verifies GET /api/v1/queue
// computes real engine-formula urgency scores (priority * (1 +
// elapsed/interval)^decay_rate) and orders by urgency descending — instead
// of the previous all-zero urgency field / priority ordering.
func TestAPI_Queue_UrgencyEngineFormula(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		MinInterval: "30s",
		MaxInterval: "24h",
		NumLevels:   10,
	})

	now := time.Now()
	// hot: priority 10, last completed 3h ago → far past its 30s interval.
	hotLast := now.Add(-3 * time.Hour)
	// fresh: priority 10, last completed 30s ago → just past its 30s interval.
	freshLast := now.Add(-30 * time.Second)
	// slow: priority 1 (interval = 24h), completed 3h ago → well within its interval.
	slowLast := now.Add(-3 * time.Hour)
	created := now.Add(-7 * 24 * time.Hour)
	mustSeedQueueProject(t, a, "hot", 10, created, hotLast)
	mustSeedQueueProject(t, a, "fresh", 10, created, freshLast)
	mustSeedQueueProject(t, a, "slow", 1, created, slowLast)

	status, body := a.do(t, "GET", "/api/v1/queue", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	raw, _ := json.Marshal(body["queue"])
	var rows []queueRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d queue rows, want 3", len(rows))
	}

	// (a) The overdue high-priority project must carry a real urgency score:
	// nonzero and strictly above its priority — impossible for the old
	// all-zero field or the priority-only fallback.
	hot := rowByName(rows, "hot")
	if hot.Urgency == 0 {
		t.Errorf("hot urgency = 0, want nonzero engine score")
	}
	if hot.Urgency <= float64(hot.Priority) {
		t.Errorf("hot urgency = %f, want > priority %d (real formula, not fallback)", hot.Urgency, hot.Priority)
	}
	// JSON shape unchanged: other fields still populated.
	if hot.Weight != 10 || hot.CooldownS != 900 || !hot.Enabled {
		t.Errorf("hot row shape = %+v, want weight=10 cooldown_s=900 enabled=true", hot)
	}

	// (b) Rows ordered by urgency descending; the overdue project first.
	for i := 1; i < len(rows); i++ {
		if rows[i].Urgency > rows[i-1].Urgency {
			t.Errorf("queue not ordered by urgency desc: rows[%d]=%s(%.4f) > rows[%d]=%s(%.4f)",
				i, rows[i].Project, rows[i].Urgency, i-1, rows[i-1].Project, rows[i-1].Urgency)
		}
	}
	if rows[0].Project != "hot" {
		t.Errorf("first row = %s, want hot", rows[0].Project)
	}

	// (c) Scores match what the engine's ComputeUrgency produces for the
	// same inputs (same interval range). Timestamps are truncated to
	// seconds because the DB stores RFC3339 second precision (the handler
	// parses the stored value, not the original sub-second one). Sub-second
	// clock drift between the handler's time.Now() and this test's makes
	// exact equality wrong, so compare with a tight relative bound.
	calc := scheduler.NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
	createdSec := created.Truncate(time.Second)
	expect := func(p int, last time.Time) float64 {
		lt := last.Truncate(time.Second)
		return calc.ComputeUrgency(float64(p), 1.0, time.Now(), &lt, createdSec)
	}
	want := map[string]float64{
		"hot":   expect(10, hotLast),
		"fresh": expect(10, freshLast),
		"slow":  expect(1, slowLast),
	}
	for _, r := range rows {
		w := want[r.Project]
		if math.Abs(r.Urgency-w) > 1e-4*math.Max(1.0, w) {
			t.Errorf("%s urgency = %f, engine ComputeUrgency = %f (diff %e)", r.Project, r.Urgency, w, math.Abs(r.Urgency-w))
		}
	}
}

// TestAPI_Queue_UrgencyFallback verifies a Server without resolved config
// (tests, unconfigured) still serves the queue with priority-only scores
// and does not panic on the nil calculator (GAP-054).
func TestAPI_Queue_UrgencyFallback(t *testing.T) {
	a := newAPITestServer(t)                   // no SetResolvedConfig → nil calculator
	mustCreateAPITestProject(t, a.db, "alpha") // priority 5

	status, body := a.do(t, "GET", "/api/v1/queue", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	raw, _ := json.Marshal(body["queue"])
	var rows []queueRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d queue rows, want 1", len(rows))
	}
	if rows[0].Urgency != float64(rows[0].Priority) {
		t.Errorf("urgency = %f, want priority-only fallback %d", rows[0].Urgency, rows[0].Priority)
	}
}
