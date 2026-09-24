package dashboard_test

// SCHED-GAP-1593 — tick drill-in: search + click-through + the per-tick page
// showing the agent's own text.
//
// The four degradation states of /ticks/{id} are the acceptance-critical
// surface: an operator must never mistake "we could not fetch it" for
// "there was nothing". Each test below renders the page and asserts the
// EXPLICIT notice text, not merely the absence of a transcript.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/agentlog"
	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// traceWithSession marshals a GatewayPOSTTrace carrying the given session id
// — the exact JSON shape spawn.go persists into ticks.gateway_trace.
func traceWithSession(t *testing.T, sessID string) string {
	t.Helper()
	blob, err := json.Marshal(scheduler.GatewayPOSTTrace{
		TickID:         "t-traced",
		Project:        "alpha",
		Model:          "glm-5.3-flash",
		Provider:       "xkiro",
		Start:          time.Now().UTC(),
		Finish:         time.Now().UTC().Add(3 * time.Minute),
		ElapsedMS:      180000,
		DeadlineMS:     1800000,
		DeadlineMode:   "idle",
		Classification: "completed",
		Events:         98,
		SessionID:      sessID,
		Attempts:       1,
	})
	if err != nil {
		t.Fatalf("marshal trace: %v", err)
	}
	return string(blob)
}

// newAgentStateDBFixture builds a minimal schema-faithful state database
// with one session whose transcript contains a recognizable assistant line.
func newAgentStateDBFixture(t *testing.T) (path, sessID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture state.db: %v", err)
	}
	defer db.Close()
	schema := `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT,
    started_at REAL NOT NULL, ended_at REAL,
    message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0
);
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL, content TEXT, tool_name TEXT, timestamp REAL NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	sessID = "623c7302-a7ef-47d9-96dd-e47cf1c42738"
	if _, err := db.Exec(`INSERT INTO sessions (id, model, billing_provider, started_at, ended_at, message_count, tool_call_count)
VALUES (?, 'glm-5.3-flash', 'xkiro', 1790279310, 1790279520, 2, 1)`, sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	for _, m := range []struct{ role, content string }{
		{"user", "[Scheduler tick: t-traced] Fix the flaky test."},
		{"assistant", "THE-AGENT-SAYS: fixed the clock seam and committed abc123."},
	} {
		if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,?,?,100)`,
			sessID, m.role, m.content); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	return path, sessID
}

// mustCreateTracedTick inserts a project + a completed tick whose
// gateway_trace carries the given raw JSON.
func mustCreateTracedTick(t *testing.T, db *sql.DB, tickID, trace string) {
	t.Helper()
	ctx := context.Background()
	mustCreateProject(t, db, "alpha", 10, 5)
	if err := database.CreateTick(ctx, db, &database.Tick{
		ID:          tickID,
		ProjectName: "alpha",
		Status:      database.StatusCompleted,
		Outcome:     database.OutcomeCommitted,
		SpawnedAt:   "2026-09-24T15:00:00Z",
		CompletedAt: "2026-09-24T15:03:00Z",
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if _, err := db.Exec(`UPDATE ticks SET gateway_trace = ? WHERE id = ?`, trace, tickID); err != nil {
		t.Fatalf("set gateway_trace: %v", err)
	}
}

func TestGenerateTickDetail_ResolvesAgentText(t *testing.T) {
	db := newTestDB(t)
	statePath, sessID := newAgentStateDBFixture(t)
	mustCreateTracedTick(t, db, "t-traced", traceWithSession(t, sessID))

	gen := dashboard.NewGenerator(db, nil)
	gen.SetAgentStateDB(agentlog.NewReader(statePath))

	var out strings.Builder
	if err := gen.GenerateTickDetail(&out, "t-traced"); err != nil {
		t.Fatalf("GenerateTickDetail: %v", err)
	}
	page := out.String()

	// The tick's own row.
	for _, want := range []string{"t-traced", "committed"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The agent's ACTUAL generated text, resolved via the trace session id.
	if !strings.Contains(page, "THE-AGENT-SAYS: fixed the clock seam and committed abc123.") {
		t.Errorf("page missing the agent's generated text; got:\n%s", snippet(page, "Agent output"))
	}
	if !strings.Contains(page, "THE AGENT'S PROMPT") && !strings.Contains(page, "Fix the flaky test.") {
		t.Errorf("page missing the user prompt turn")
	}
	if !strings.Contains(page, sessID) {
		t.Error("page should surface the resolved session id")
	}
	// The explicit-degradation notices must be ABSENT on the happy path.
	if strings.Contains(page, "⚠") {
		t.Errorf("happy-path render must not carry degradation notices: %s", snippet(page, "⚠"))
	}
}

func TestGenerateTickDetail_NoTraceShowsExplicitNotice(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	if err := database.CreateTick(context.Background(), db, &database.Tick{
		ID: "t-notrace", ProjectName: "alpha", Status: database.StatusFailed, Outcome: database.OutcomeFailed,
	}); err != nil {
		t.Fatal(err)
	}
	statePath, _ := newAgentStateDBFixture(t)
	gen := dashboard.NewGenerator(db, nil)
	gen.SetAgentStateDB(agentlog.NewReader(statePath))

	var out strings.Builder
	if err := gen.GenerateTickDetail(&out, "t-notrace"); err != nil {
		t.Fatalf("GenerateTickDetail: %v", err)
	}
	page := out.String()
	if !strings.Contains(page, "NO gateway trace recorded") {
		t.Errorf("missing the explicit no-trace notice; got:\n%s", snippet(page, "Agent output"))
	}
	if !strings.Contains(page, "cannot be fetched") {
		t.Errorf("notice must say the fetch is impossible, not that nothing was generated")
	}
}

func TestGenerateTickDetail_MissingStateDBDoesNotFailRender(t *testing.T) {
	db := newTestDB(t)
	sessID := "623c7302-a7ef-47d9-96dd-e47cf1c42738"
	mustCreateTracedTick(t, db, "t-traced", traceWithSession(t, sessID))

	// Reader points at a state.db that does not exist (the live case when
	// the dashboard runs on a host without the agent home).
	gen := dashboard.NewGenerator(db, nil)
	gen.SetAgentStateDB(agentlog.NewReader(filepath.Join(t.TempDir(), "absent", "state.db")))

	var out strings.Builder
	if err := gen.GenerateTickDetail(&out, "t-traced"); err != nil {
		t.Fatalf("missing state.db must NOT fail the render: %v", err)
	}
	page := out.String()
	if !strings.Contains(page, "AGENT STATE DATABASE UNAVAILABLE") {
		t.Errorf("missing explicit unavailable notice; got:\n%s", snippet(page, "Agent output"))
	}
	if !strings.Contains(page, sessID) {
		t.Error("the trace's session id must still be shown (it IS recorded)")
	}
}

func TestGenerateTickDetail_NilReaderDegrades(t *testing.T) {
	db := newTestDB(t)
	sessID := "623c7302-a7ef-47d9-96dd-e47cf1c42738"
	mustCreateTracedTick(t, db, "t-traced", traceWithSession(t, sessID))

	gen := dashboard.NewGenerator(db, nil) // no SetAgentStateDB call

	var out strings.Builder
	if err := gen.GenerateTickDetail(&out, "t-traced"); err != nil {
		t.Fatalf("nil reader must not fail the render: %v", err)
	}
	if !strings.Contains(out.String(), "not configured") {
		t.Errorf("missing explicit not-configured notice; got:\n%s", snippet(out.String(), "Agent output"))
	}
}

func TestGenerateTickDetail_SessionNotFoundDegrades(t *testing.T) {
	db := newTestDB(t)
	statePath, _ := newAgentStateDBFixture(t)
	mustCreateTracedTick(t, db, "t-traced", traceWithSession(t, "ffffffff-0000-0000-0000-000000000000"))

	gen := dashboard.NewGenerator(db, nil)
	gen.SetAgentStateDB(agentlog.NewReader(statePath))

	var out strings.Builder
	if err := gen.GenerateTickDetail(&out, "t-traced"); err != nil {
		t.Fatalf("GenerateTickDetail: %v", err)
	}
	page := out.String()
	if !strings.Contains(page, "AGENT SESSION NOT FOUND") {
		t.Errorf("missing explicit session-not-found notice; got:\n%s", snippet(page, "Agent output"))
	}
}

func TestGenerateTickDetail_UnknownTickReturnsErrTickNotFound(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	var out strings.Builder
	err := gen.GenerateTickDetail(&out, "no-such-tick")
	if err == nil {
		t.Fatal("expected an error for an unknown tick id")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %v should wrap ErrTickNotFound", err)
	}
}

// seedHistoryTicks writes a small mixed history for the filter tests.
func seedHistoryTicks(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	mustCreateProject(t, db, "alpha", 10, 5)
	mustCreateProject(t, db, "beta", 10, 5)
	rows := []struct {
		id, project string
		status      database.TickStatus
		outcome     database.TickOutcome
	}{
		{"alpha-t-001", "alpha", database.StatusCompleted, database.OutcomeCommitted},
		{"alpha-t-002", "alpha", database.StatusCompleted, database.OutcomeDryRun},
		{"beta-t-001", "beta", database.StatusFailed, database.OutcomeFailed},
		{"beta-t-002", "beta", database.StatusTimeout, database.OutcomeTimeout},
	}
	for _, r := range rows {
		if err := database.CreateTick(ctx, db, &database.Tick{
			ID: r.id, ProjectName: r.project, Status: r.status, Outcome: r.outcome,
		}); err != nil {
			t.Fatalf("CreateTick %s: %v", r.id, err)
		}
	}
}

func pageTickIDs(t *testing.T, page string) []string {
	t.Helper()
	var ids []string
	for _, id := range []string{"alpha-t-001", "alpha-t-002", "beta-t-001", "beta-t-002"} {
		if strings.Contains(page, `href="/ticks/`+id+`"`) {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestGenerateTickHistory_FilterByProject(t *testing.T) {
	db := newTestDB(t)
	seedHistoryTicks(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{Project: "alpha"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	page := out.String()
	if got := pageTickIDs(t, page); len(got) != 2 {
		t.Errorf("project filter should show exactly alpha's 2 ticks, got %v", got)
	}
	if !strings.Contains(page, "match") {
		t.Errorf("filtered heading should say 'match' not 'total': %s", snippet(page, "Tick History"))
	}
}

func TestGenerateTickHistory_SearchMatchesIDAndProject(t *testing.T) {
	db := newTestDB(t)
	seedHistoryTicks(t, db)
	gen := dashboard.NewGenerator(db, nil)

	// "beta-t-002" matches the tick id itself.
	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{Query: "beta-t-002"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	if got := pageTickIDs(t, out.String()); len(got) != 1 || got[0] != "beta-t-002" {
		t.Errorf("id search should match only beta-t-002, got %v", got)
	}

	// "beta" matches the project name → both beta ticks.
	var out2 strings.Builder
	if err := gen.GenerateTickHistory(&out2, 1, database.TickFilter{Query: "beta"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	if got := pageTickIDs(t, out2.String()); len(got) != 2 {
		t.Errorf("project-name search should match both beta ticks, got %v", got)
	}
}

func TestGenerateTickHistory_FilterByStatusAndOutcome(t *testing.T) {
	db := newTestDB(t)
	seedHistoryTicks(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{Status: "completed"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	if got := pageTickIDs(t, out.String()); len(got) != 2 {
		t.Errorf("status=completed should show the 2 alpha ticks, got %v", got)
	}

	var out2 strings.Builder
	if err := gen.GenerateTickHistory(&out2, 1, database.TickFilter{Outcome: "timeout"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	if got := pageTickIDs(t, out2.String()); len(got) != 1 || got[0] != "beta-t-002" {
		t.Errorf("outcome=timeout should show only beta-t-002, got %v", got)
	}
}

func TestGenerateTickHistory_UnknownStatusDropsFilter(t *testing.T) {
	db := newTestDB(t)
	seedHistoryTicks(t, db)
	gen := dashboard.NewGenerator(db, nil)

	// A stray/unknown status value must not render a guaranteed-empty page —
	// the filter is dropped and the unfiltered history renders.
	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{Status: "sparkly"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	if got := pageTickIDs(t, out.String()); len(got) != 4 {
		t.Errorf("unknown status should drop the filter (all 4 ticks), got %v", got)
	}
}

func TestGenerateTickHistory_PaginationPreservesFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProject(t, db, "alpha", 10, 5)
	// 2 pages worth of alpha ticks so the Next link exists under a filter.
	for i := 0; i < 51; i++ {
		if err := database.CreateTick(ctx, db, &database.Tick{
			ID:          "alpha-p-" + string(rune('A'+i%26)) + string(rune('0'+i/26)),
			ProjectName: "alpha", Status: database.StatusCompleted,
		}); err != nil {
			t.Fatal(err)
		}
	}
	gen := dashboard.NewGenerator(db, nil)

	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{Project: "alpha"}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	page := out.String()
	hasNextWithFilter := strings.Contains(page, `/ticks?page=2&project=alpha`) ||
		strings.Contains(page, `/ticks?page=2&amp;project=alpha`)
	if !hasNextWithFilter {
		t.Errorf("pagination link must carry the active filter; got: %s", snippet(page, "pagination"))
	}
}

func TestGenerateTickHistory_TickIDsLinkToDetail(t *testing.T) {
	db := newTestDB(t)
	seedHistoryTicks(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var out strings.Builder
	if err := gen.GenerateTickHistory(&out, 1, database.TickFilter{}); err != nil {
		t.Fatalf("GenerateTickHistory: %v", err)
	}
	for _, want := range []string{`href="/ticks/alpha-t-001"`, `href="/ticks/beta-t-001"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tick-history rows must link into the drill-down, missing %s", want)
		}
	}
}
