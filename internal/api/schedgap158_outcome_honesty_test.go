package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-158 acceptance tests: the outcome-honesty blocks (stalls,
// escalations, lane_churn) on GET /api/v1/metrics.
//
// The seed arithmetic is spelled out in each test so every assertion is an
// EXACT expected value, never "> 0". The honesty rule under test: a block
// whose real source is absent reports {"available": false, "reason": ...} —
// never a fabricated zero.

// gap158Now is the fixed decision instant every window in this file is
// derived from. The Server reads it through clock.NewFixed (SCHED-GAP-169),
// so the 24h cutoff is deterministic: gap158Now - 24h.
var gap158Now = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// gap158RFC3339 formats an offset from gap158Now as a UTC RFC3339 stamp —
// the same shape the daemon's nowUTC writes.
func gap158RFC3339(ago time.Duration) string {
	return gap158Now.Add(-ago).Format(time.RFC3339)
}

// gap158SeedProjects creates the projects rows the tick fixture references
// (ticks.project_name REFERENCES projects(name)).
func gap158SeedProjects(t *testing.T, db *sql.DB, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		if err := database.CreateProject(ctx, db, &database.Project{
			Name:      name,
			RepoURL:   "https://example.com/" + name,
			Workdir:   "/tmp/" + name,
			Weight:    10,
			Priority:  5,
			CooldownS: 3600,
			DecayRate: 1.0,
			Model:     "test",
			Provider:  "test",
			Enabled:   true,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
}

// gap158InsertTick writes one tick row. spawnedAgo/durationMs follow the
// gap156 convention: durationMs == 0 leaves completed_at NULL, spawnedAgo < 0
// leaves spawned_at NULL.
func gap158InsertTick(t *testing.T, db *sql.DB, id, project, status, outcome string, spawnedAgo time.Duration, durationMs int) {
	t.Helper()
	var spawned, completed interface{}
	if spawnedAgo >= 0 {
		spawned = gap158RFC3339(spawnedAgo)
	}
	if durationMs > 0 {
		completed = gap158Now.Add(-spawnedAgo + time.Duration(durationMs)*time.Millisecond).Format(time.RFC3339)
	}
	var outcomeArg interface{}
	if outcome != "" {
		outcomeArg = outcome
	}
	if _, err := db.ExecContext(context.Background(), `
INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, commits, error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project, status, outcomeArg, spawned, completed, 0, nil, gap158RFC3339(spawnedAgo)); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// gap158InsertEvent writes one events row (direct INSERT: the api package
// tests own the DB handle, and LogEvent's publish fan-out has no subscriber
// here).
func gap158InsertEvent(t *testing.T, db *sql.DB, severity, component, message string, createdAt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO events (severity, component, message, details, created_at) VALUES (?, ?, ?, '{}', ?)`,
		severity, component, message, createdAt); err != nil {
		t.Fatalf("insert event [%s] %s: %v", severity, message, err)
	}
}

// gap158StampDisable stamps the GAP-044 disable-provenance columns directly
// (the production paths own disabled_at; a test seed owns its own clock).
func gap158StampDisable(t *testing.T, db *sql.DB, name, by, reason, at string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE projects SET enabled = 0, disabled_at = ?, disabled_by = ?, disabled_reason = ? WHERE name = ?`,
		at, by, reason, name); err != nil {
		t.Fatalf("stamp disable %s: %v", name, err)
	}
}

// gap158GetMetrics issues the real GET through the server's Handler and
// decodes the top-level object.
func gap158GetMetrics(t *testing.T, s *Server) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil))
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/v1/metrics: %v (body: %s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// gap158Block pulls a top-level block, failing when missing or not an object.
func gap158Block(t *testing.T, body map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	raw, ok := body[name]
	if !ok {
		t.Fatalf("response is missing top-level block %q", name)
	}
	block, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("block %q is %T, want object", name, raw)
	}
	return block
}

// gap158Int asserts the field is a JSON number and returns it as int.
func gap158Int(t *testing.T, parent map[string]interface{}, field string) int {
	t.Helper()
	raw, ok := parent[field]
	if !ok {
		t.Fatalf("missing number field %q", field)
	}
	f, ok := raw.(float64)
	if !ok {
		t.Fatalf("field %q is %T, want number", field, raw)
	}
	return int(f)
}

// gap158Map asserts the field is a JSON object (this is the {}-never-null
// check: json.Unmarshal maps a JSON null to an untyped nil, not a map).
func gap158Map(t *testing.T, parent map[string]interface{}, field string) map[string]interface{} {
	t.Helper()
	raw, ok := parent[field]
	if !ok {
		t.Fatalf("missing object field %q", field)
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("field %q is %T, want object", field, raw)
	}
	return m
}

// gap158String asserts the field is a JSON string.
func gap158String(t *testing.T, parent map[string]interface{}, field string) string {
	t.Helper()
	raw, ok := parent[field]
	if !ok {
		t.Fatalf("missing string field %q", field)
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("field %q is %T, want string", field, raw)
	}
	return s
}

// gap158IntMap asserts the field is a JSON object of string -> int.
func gap158IntMap(t *testing.T, parent map[string]interface{}, field string) map[string]int {
	t.Helper()
	raw := gap158Map(t, parent, field)
	out := make(map[string]int, len(raw))
	for k, v := range raw {
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("%s[%q] is %T, want number", field, k, v)
		}
		out[k] = int(f)
	}
	return out
}

// TestSCHEDGAP158_Stalls pins the stall-bucket arithmetic. Seed (all
// completed inside the window, spawned_at present):
//
//	g158-s1   31 min   -> 30-60   (just over the floor)
//	g158-s2   60 min   -> 30-60   (exactly 1h: the 30-60 bucket is
//	                     >30m up to AND INCLUDING 1h)
//	g158-s3   61 min   -> 60-120
//	g158-s4  120 min   -> 60-120  (exactly 2h: the 60-120 bucket is
//	                     >1h up to AND INCLUDING 2h)
//	g158-s5  121 min   -> 120+
//	g158-s6   30 min   -> NOTHING (exactly the floor: EXCEEDS 30 required)
//	g158-s7   10 min   -> NOTHING (healthy tick)
//	g158-neg  -5 min   -> NOTHING (negative duration: completed BEFORE
//	                     spawned — excluded by the non-negative guard even
//	                     though completed_at is inside the window)
//	g158-null  -        -> NOTHING (completed_at NULL: no measured duration,
//	                     never counted as stalled)
//	g158-out 181 min   -> NOTHING (spawned 30h ago, so completed_at lands
//	                     26h59m ago: outside the 24h completed_at window —
//	                     would land in 120+ if the window filter were wrong)
func TestSCHEDGAP158_Stalls(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	gap158SeedProjects(t, db, "gap158p")

	gap158InsertTick(t, db, "g158-s1", "gap158p", "completed", "committed", 3*time.Hour, 31*60000)
	gap158InsertTick(t, db, "g158-s2", "gap158p", "completed", "committed", 3*time.Hour, 60*60000)
	gap158InsertTick(t, db, "g158-s3", "gap158p", "completed", "committed", 3*time.Hour, 61*60000)
	gap158InsertTick(t, db, "g158-s4", "gap158p", "completed", "committed", 3*time.Hour, 120*60000)
	gap158InsertTick(t, db, "g158-s5", "gap158p", "completed", "committed", 3*time.Hour, 121*60000)
	gap158InsertTick(t, db, "g158-s6", "gap158p", "completed", "committed", 3*time.Hour, 30*60000)
	gap158InsertTick(t, db, "g158-s7", "gap158p", "completed", "committed", 3*time.Hour, 10*60000)
	// Negative duration, completed inside the window: completed_at = now-2h58m
	// (spawned 3h ago - 5 min), spawned_at = now-3h. The non-negative guard
	// must exclude it.
	gap158InsertTick(t, db, "g158-neg", "gap158p", "completed", "committed", 3*time.Hour, -5*60000)
	gap158InsertTick(t, db, "g158-null", "gap158p", "running", "", 20*time.Minute, 0)
	// Out-of-window completion: spawned 30h ago + 181 min -> completed 26h59m ago.
	gap158InsertTick(t, db, "g158-out", "gap158p", "completed", "timeout", 30*time.Hour, 181*60000)

	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(gap158Now))

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (body: %v)", code, body)
	}
	stalls := gap158Block(t, body, "stalls")
	if stalls["available"] != true {
		t.Fatalf("stalls.available = %v, want true on a seeded read", stalls["available"])
	}
	if got := gap158String(t, stalls, "window"); got != "24h" {
		t.Errorf("stalls.window = %q, want \"24h\"", got)
	}
	if got := gap158Int(t, stalls, "threshold"); got != 30 {
		t.Errorf("stalls.threshold = %d, want 30", got)
	}
	byMinutes := gap158IntMap(t, stalls, "by_minutes")
	// Every bucket edge is pinned: 30 and the (30,60] / (60,120] / (120,∞)
	// boundaries from BOTH sides (s6 exactly-at-floor excluded, s2/s4
	// exactly-at-edge included in the bucket below, s1/s3/s5 one minute over).
	want := map[string]int{"30-60": 2, "60-120": 2, "120+": 1}
	for bucket, wantN := range want {
		if got := byMinutes[bucket]; got != wantN {
			t.Errorf("stalls.by_minutes[%q] = %d, want %d", bucket, got, wantN)
		}
	}
	if got := len(byMinutes); got != 3 {
		t.Errorf("stalls.by_minutes has %d buckets, want 3 (every bucket key always present, 0 = real query result)", got)
	}
	if got := gap158Int(t, stalls, "total"); got != 5 {
		t.Errorf("stalls.total = %d, want 5 (s1+s2+s3+s4+s5; at-floor, sub-floor, negative, uncompleted and out-of-window rows excluded)", got)
	}
}

// TestSCHEDGAP158_Escalations pins the escalation counts: ONE events row with
// severity CRITICAL or HIGH inside the window, grouped by UTC day
// (date(created_at)) and by component. Seed:
//
//	HIGH     loop  10h ago, 2 rows      -> day(10h ago): HIGH 2
//	CRITICAL escalation 30m ago, 1 row  -> today: CRITICAL 1
//	HIGH     api   26h ago, 1 row      -> OUTSIDE the window
//	MEDIUM   loop  1h ago, 1 row       -> NOT an alert severity (SCHED-GAP-061 demoted tier)
//	INFO     api   2h ago, 1 row       -> not an alert severity
func TestSCHEDGAP158_Escalations(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	day := func(ago time.Duration) string { return gap158Now.Add(-ago).Format("2006-01-02") }
	gap158InsertEvent(t, db, "HIGH", "loop", "eval loop stalled — forced re-evaluation", gap158RFC3339(10*time.Hour))
	gap158InsertEvent(t, db, "HIGH", "loop", "eval loop stalled — forced re-evaluation", gap158RFC3339(9*time.Hour))
	gap158InsertEvent(t, db, "CRITICAL", "escalation", "scheduler not evaluating — never ran", gap158RFC3339(30*time.Minute))
	gap158InsertEvent(t, db, "HIGH", "api", "outside the window", gap158RFC3339(26*time.Hour))
	gap158InsertEvent(t, db, "MEDIUM", "loop", "eval loop stalled (recovered)", gap158RFC3339(time.Hour))
	gap158InsertEvent(t, db, "INFO", "api", "project disabled: x (api)", gap158RFC3339(2*time.Hour))

	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(gap158Now))

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (body: %v)", code, body)
	}
	esc := gap158Block(t, body, "escalations")
	if esc["available"] != true {
		t.Fatalf("escalations.available = %v, want true on a seeded read", esc["available"])
	}
	if got := gap158String(t, esc, "window"); got != "24h" {
		t.Errorf("escalations.window = %q, want \"24h\"", got)
	}
	if got := gap158Int(t, esc, "total"); got != 3 {
		t.Errorf("escalations.total = %d, want 3 (2 HIGH + 1 CRITICAL inside the window)", got)
	}
	bySeverity := gap158IntMap(t, esc, "by_severity")
	if got := bySeverity["HIGH"]; got != 2 {
		t.Errorf("escalations.by_severity[HIGH] = %d, want 2", got)
	}
	if got := bySeverity["CRITICAL"]; got != 1 {
		t.Errorf("escalations.by_severity[CRITICAL] = %d, want 1", got)
	}
	byClass := gap158IntMap(t, esc, "by_class")
	if got := byClass["loop"]; got != 2 {
		t.Errorf("escalations.by_class[loop] = %d, want 2", got)
	}
	if got := byClass["escalation"]; got != 1 {
		t.Errorf("escalations.by_class[escalation] = %d, want 1", got)
	}
	byDay := map[string]map[string]int{}
	for d, raw := range gap158Map(t, esc, "by_day") {
		sev, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("escalations.by_day[%q] is %T, want object", d, raw)
		}
		m := map[string]int{}
		for k, v := range sev {
			f, ok := v.(float64)
			if !ok {
				t.Fatalf("escalations.by_day[%q][%q] is %T, want number", d, k, v)
			}
			m[k] = int(f)
		}
		byDay[d] = m
	}
	if got := byDay[day(10*time.Hour)]["HIGH"]; got != 2 {
		t.Errorf("escalations.by_day[%s][HIGH] = %d, want 2 (UTC-day bucket via date(created_at))", day(10*time.Hour), got)
	}
	if got := byDay[day(30*time.Minute)]["CRITICAL"]; got != 1 {
		t.Errorf("escalations.by_day[%s][CRITICAL] = %d, want 1", day(30*time.Minute), got)
	}
	// The empty CRITICAL day and the out-of-window day must not exist.
	if _, ok := byDay[day(26*time.Hour)]; ok {
		t.Errorf("escalations.by_day has a key for the out-of-window day %s", day(26*time.Hour))
	}
}

// TestSCHEDGAP158_LaneChurn pins the churn counts. Seed:
//
//	g158-paused  disabled 6h ago by api-pause  -> today: api-pause 1
//	g158-put     disabled 5h ago by api        -> today: api 1
//	g158-deleted disabled 3h ago by api-delete -> today: api-delete 1
//	g158-casc    disabled 2h ago by api-pause-cascade -> today: cascade 1
//	g158-legacy  disabled (backfill) by legacy, disabled_at 8 days ago -> OUTSIDE window
//	g158-live    never disabled, enabled=1    -> only in enabled_current
//
// plus one cascade-resume enable event 4h ago (component api) and one INFO
// event that is NOT an enable.
func TestSCHEDGAP158_LaneChurn(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	gap158SeedProjects(t, db, "g158-paused", "g158-put", "g158-deleted", "g158-casc", "g158-legacy", "g158-live")

	gap158StampDisable(t, db, "g158-paused", "api-pause", "paused via POST /projects/{name}/pause", gap158RFC3339(6*time.Hour))
	gap158StampDisable(t, db, "g158-put", "api", "disabled via PUT enabled=false", gap158RFC3339(5*time.Hour))
	gap158StampDisable(t, db, "g158-deleted", "api-delete", "deleted via DELETE ?confirm=true", gap158RFC3339(3*time.Hour))
	gap158StampDisable(t, db, "g158-casc", "api-pause-cascade", "paused by target g158-paused (via POST /projects/{name}/pause)", gap158RFC3339(2*time.Hour))
	gap158StampDisable(t, db, "g158-legacy", "legacy", "pre-GAP-044 disable", gap158RFC3339(8*24*time.Hour))

	gap158InsertEvent(t, db, "INFO", "api",
		"project enabled: g158-casc (cascade resume of g158-paused)", gap158RFC3339(4*time.Hour))
	gap158InsertEvent(t, db, "INFO", "api",
		"unrelated event that must not count as an enable", gap158RFC3339(time.Hour))

	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(gap158Now))

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (body: %v)", code, body)
	}
	churn := gap158Block(t, body, "lane_churn")
	if churn["available"] != true {
		t.Fatalf("lane_churn.available = %v, want true on a seeded read", churn["available"])
	}
	if got := gap158String(t, churn, "window"); got != "24h" {
		t.Errorf("lane_churn.window = %q, want \"24h\"", got)
	}
	today := gap158Now.Format("2006-01-02")
	byDay := map[string]map[string]int{}
	for d, raw := range gap158Map(t, churn, "disabled_by_day") {
		prov, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("lane_churn.disabled_by_day[%q] is %T, want object", d, raw)
		}
		m := map[string]int{}
		for k, v := range prov {
			f, ok := v.(float64)
			if !ok {
				t.Fatalf("lane_churn.disabled_by_day[%q][%q] is %T, want number", d, k, v)
			}
			m[k] = int(f)
		}
		byDay[d] = m
	}
	dayMap, ok := byDay[today]
	if !ok {
		t.Fatalf("lane_churn.disabled_by_day has no key for today (%s): %v", today, byDay)
	}
	for prov, wantN := range map[string]int{
		"api-pause":         1,
		"api":               1,
		"api-delete":        1,
		"api-pause-cascade": 1,
	} {
		if got := dayMap[prov]; got != wantN {
			t.Errorf("lane_churn.disabled_by_day[%s][%s] = %d, want %d", today, prov, got, wantN)
		}
	}
	if got := len(dayMap); got != 4 {
		t.Errorf("lane_churn.disabled_by_day[%s] has %d provenance keys, want 4", today, got)
	}
	byProvenance := gap158IntMap(t, churn, "by_provenance")
	if got := byProvenance["legacy"]; got != 0 {
		t.Errorf("lane_churn.by_provenance[legacy] = %d, want 0 (disabled_at 8 days ago is outside the window)", got)
	}
	if got := gap158Int(t, churn, "disabled_total"); got != 4 {
		t.Errorf("lane_churn.disabled_total = %d, want 4", got)
	}
	enabledPerDay := gap158IntMap(t, churn, "enabled_per_day")
	if got := enabledPerDay[today]; got != 1 {
		t.Errorf("lane_churn.enabled_per_day[%s] = %d, want 1 (exactly one cascade-resume enable event)", today, got)
	}
	if got := gap158Int(t, churn, "enabled_current"); got != 1 {
		t.Errorf("lane_churn.enabled_current = %d, want 1 (only g158-live is still enabled)", got)
	}
}

// TestSCHEDGAP158_SourcesNameEveryNewBlock pins AC3: each new block key has a
// non-trivial entry in the sources map of the same response.
func TestSCHEDGAP158_SourcesNameEveryNewBlock(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	s := NewServer(db, nil)

	_, body := gap158GetMetrics(t, s)
	sources := gap158Block(t, body, "sources")
	for _, block := range []string{"stalls", "escalations", "lane_churn"} {
		desc, ok := sources[block].(string)
		if !ok || len(desc) < 20 {
			t.Errorf("AC3: sources[%q] is missing or too short to name a source: %v", block, sources[block])
		}
	}
	// The escalation definition must be auditable from the wire: the sources
	// string names the severity classes that define an escalation.
	escSource, _ := sources["escalations"].(string)
	for _, want := range []string{"CRITICAL", "HIGH", "events"} {
		if !contains(escSource, want) {
			t.Errorf("AC3: sources[escalations] does not name %q — the definition must be auditable from the wire: %q", want, escSource)
		}
	}
}

// TestSCHEDGAP158_UnavailableWhenUnsourced pins the honesty rule for the new
// blocks: with the DB closed each block reports available=false with a
// non-empty reason — never a fabricated 0.
func TestSCHEDGAP158_UnavailableWhenUnsourced(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := NewServer(db, nil)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (a dead source is a reported gap, not a 5xx)", code)
	}
	for _, block := range []string{"stalls", "escalations", "lane_churn"} {
		got := gap158Block(t, body, block)
		if got["available"] != false {
			t.Errorf("AC2: %s.available = %v with the DB closed, want false", block, got["available"])
		}
		if reason, _ := got["reason"].(string); reason == "" {
			t.Errorf("AC2: %s has available=false but no reason", block)
		}
	}
}

// TestSCHEDGAP158_EmptyDBRealZeros proves the flip side of the honesty rule:
// on a live DB with NO rows the blocks are available=true and the zero totals
// come from real queries — with every map marshalling as {} never null.
func TestSCHEDGAP158_EmptyDBRealZeros(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(gap158Now))

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200", code)
	}

	stalls := gap158Block(t, body, "stalls")
	if stalls["available"] != true {
		t.Fatalf("stalls.available = %v on a live empty DB, want true (a real query CAN return 0)", stalls["available"])
	}
	if got := gap158Int(t, stalls, "total"); got != 0 {
		t.Errorf("stalls.total = %d, want 0 on an empty DB", got)
	}
	gap158IntMap(t, stalls, "by_minutes") // must be an object ({}), not null

	esc := gap158Block(t, body, "escalations")
	if esc["available"] != true {
		t.Fatalf("escalations.available = %v on a live empty DB, want true", esc["available"])
	}
	if got := gap158Int(t, esc, "total"); got != 0 {
		t.Errorf("escalations.total = %d, want 0 on an empty DB", got)
	}
	if got := gap158IntMap(t, esc, "by_day"); len(got) != 0 {
		t.Errorf("escalations.by_day = %v, want {}", got)
	}
	if got := gap158IntMap(t, esc, "by_class"); len(got) != 0 {
		t.Errorf("escalations.by_class = %v, want {}", got)
	}

	churn := gap158Block(t, body, "lane_churn")
	if churn["available"] != true {
		t.Fatalf("lane_churn.available = %v on a live empty DB, want true", churn["available"])
	}
	if got := gap158Int(t, churn, "disabled_total"); got != 0 {
		t.Errorf("lane_churn.disabled_total = %d, want 0 on an empty DB", got)
	}
	if got := gap158IntMap(t, churn, "disabled_by_day"); len(got) != 0 {
		t.Errorf("lane_churn.disabled_by_day = %v, want {}", got)
	}
	if got := gap158IntMap(t, churn, "enabled_per_day"); len(got) != 0 {
		t.Errorf("lane_churn.enabled_per_day = %v, want {}", got)
	}
	if got := gap158Int(t, churn, "enabled_current"); got != 0 {
		t.Errorf("lane_churn.enabled_current = %d, want 0 on an empty DB (a real COUNT over a real table)", got)
	}
}

// TestSCHEDGAP158_GetStillDecodes pins AC4's posture clause: GET is still 200
// and the response still decodes into the SCHED-GAP-156 shape (every legacy
// top-level block present, deferrals still honestly unavailable without a
// Loop, POST still 405) — nothing about the existing contract regressed.
func TestSCHEDGAP158_GetStillDecodes(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	s := NewServer(db, nil)

	code, body := gap158GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200", code)
	}
	for _, block := range []string{
		"generated_at", "uptime_s", "sources", "spawns", "deferrals", "nudges",
		"ticks", "tick_duration_ms", "gateway", "outcomes",
		// and the new three:
		"stalls", "escalations", "lane_churn",
	} {
		if _, ok := body[block]; !ok {
			t.Errorf("top-level block %q missing", block)
		}
	}
	if _, err := time.Parse(time.RFC3339, body["generated_at"].(string)); err != nil {
		t.Errorf("generated_at is not RFC3339: %v", body["generated_at"])
	}
	if deferrals := gap158Block(t, body, "deferrals"); deferrals["available"] != false {
		t.Errorf("deferrals.available = %v with no Loop, want false (unchanged honesty posture)", deferrals["available"])
	}

	postRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/api/v1/metrics", nil))
	if postRec.Code != 405 {
		t.Errorf("POST /api/v1/metrics = %d, want 405 (GET-only posture unchanged)", postRec.Code)
	}
}

// TestSCHEDGAP158_OpenAPISpecStillMatches pins AC4's spec clause: the served
// OpenAPI entry for /api/v1/metrics still parses and still names every block
// the endpoint now serves (including the three new ones).
func TestSCHEDGAP158_OpenAPISpecStillMatches(t *testing.T) {
	if !json.Valid(openapiSpec) {
		t.Fatal("openapiSpec is not valid JSON")
	}
	var spec map[string]interface{}
	if err := json.Unmarshal(openapiSpec, &spec); err != nil {
		t.Fatalf("unmarshal openapiSpec: %v", err)
	}
	paths, ok := spec["paths"].(map[string]interface{})
	if !ok {
		t.Fatal("openapiSpec has no paths object")
	}
	entry, ok := paths["/api/v1/metrics"].(map[string]interface{})
	if !ok {
		t.Fatal("openapiSpec has no /api/v1/metrics path")
	}
	get, ok := entry["get"].(map[string]interface{})
	if !ok {
		t.Fatal("/api/v1/metrics has no get operation")
	}
	responses, ok := get["responses"].(map[string]interface{})
	if !ok {
		t.Fatal("get operation has no responses")
	}
	ok200, ok := responses["200"].(map[string]interface{})
	if !ok {
		t.Fatal("get operation has no 200 response")
	}
	desc, _ := ok200["description"].(string)
	for _, block := range []string{"stalls", "escalations", "lane_churn", "outcomes", "spawns", "tick_duration_ms"} {
		if !contains(desc, block) {
			t.Errorf("openapi 200 description does not name block %q — the spec no longer matches the endpoint", block)
		}
	}
}

// contains reports whether the sources/spec text names the token.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
