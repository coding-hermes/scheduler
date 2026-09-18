package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-156 acceptance test.
//
// The seed below is deliberately exhaustive and its arithmetic is spelled out
// so every assertion is an EXACT expected value, never "> 0":
//
//	seed window: every windowed block looks back 24h from the request time.
//
//	projects
//	  gap156a  enabled, ns "gap156ns" (cap 2, cooldown_s 3600), last_tick_completed = now-2h, no live tick -> cooldown expired (counted)
//	  gap156b  enabled, ns "gap156ns", last_tick_completed = now-10m, RUNNING tick -> excluded
//	  gap156c  enabled, ns "gap156ns", last_tick_completed = now-2h, QUEUED tick   -> excluded
//	  gap156d  DISABLED, no namespace row at all (namespace_id NULL) -> by_namespace "-"
//
//	ticks (spawned_at inside the window unless noted)
//	  g156-d01..d10  gap156a  completed committed commits=1   duration 100000..1000000 ms
//	  g156-zero      gap156a  completed committed commits=0   duration 5000 ms   <- question 8
//	  g156-dry       gap156a  completed dry_run   commits=0   duration 3000 ms
//	  g156-drain     gap156b  failed    failed    commits=0   duration 2000 ms   <- question 7 (drain-class error)
//	  g156-nons      gap156d  completed committed commits=1   duration 10000 ms  <- no namespace -> "-"
//	  g156-run       gap156b  running   (no outcome, completed_at NULL)
//	  g156-timeout   gap156c  timeout   timeout   completed_at NULL (never reaped: out of the duration sample)
//	  g156-queued    gap156c  queued    (spawned_at NULL — queued rows are pre-spawn)
//	  g156-nudge1    gap156a  completed failed  spawned 3 DAYS ago  nudge_count=2 orphan_reason=drain_timeout
//	  g156-nudge2    gap156a  completed failed  spawned 3 DAYS ago  nudge_count=1 orphan_reason=zombie_reap
//
//	DURATION SAMPLE (completed_at in window, both stamps present) — ascending:
//	  2000, 3000, 5000, 10000, 100000, 200000, 300000, 400000, 500000, 600000,
//	  700000, 800000, 900000, 1000000   (n = 14)
//	  nearest-rank idx = ceil(pct/100 * n) - 1:
//	    p50 -> ceil(7)   - 1 = 6  -> 300000
//	    p90 -> ceil(12.6) - 1 = 12 -> 900000
//	    p99 -> ceil(13.86) - 1 = 13 -> 1000000
//
// Do not "fix" these numbers by re-deriving them with the production helper in
// the test — the point of AC4 is that they were computed independently (they
// were: a separate implementation of the documented rule produced the same
// three values before this test was written).
const (
	gap156Namespace = "gap156ns"
	// gap156AllTimeNudges is the number of nudge increments seeded outside the
	// 24h window; it proves the nudges block is all-time, not windowed.
	gap156AllTimeNudges = 3
)

// gap156Seed inserts the fixture described in the file comment.
func gap156Seed(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID:            gap156Namespace,
		Weight:        10,
		Reserved:      0,
		HardCap:       0,
		MaxConcurrent: 2,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	// lastCompleted is stamped with a direct UPDATE: the projects row carries
	// the cooldown anchor the cooldown-expired gauge reads.
	newProject := func(name, workdir, lastCompleted, namespaceID string, enabled bool) {
		t.Helper()
		var nsID *string
		if namespaceID != "" {
			nsID = &namespaceID
		}
		if err := database.CreateProject(ctx, db, &database.Project{
			Name:        name,
			RepoURL:     "https://example.com/" + name,
			Workdir:     workdir,
			Weight:      10,
			Priority:    5,
			CooldownS:   3600,
			DecayRate:   1.0,
			Model:       "test",
			Provider:    "test",
			NamespaceID: nsID,
			Enabled:     enabled,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
		if lastCompleted != "" {
			if _, err := db.ExecContext(ctx,
				`UPDATE projects SET last_tick_completed = ? WHERE name = ?`, lastCompleted, name); err != nil {
				t.Fatalf("stamp last_tick_completed %s: %v", name, err)
			}
		}
	}
	// gap156a: cooldown long elapsed, no live tick -> counted by question 5.
	newProject("gap156a", "/tmp/gap156a", ts(2*time.Hour), gap156Namespace, true)
	// gap156b: running tick -> never counted by question 5.
	newProject("gap156b", "/tmp/gap156b", ts(10*time.Minute), gap156Namespace, true)
	// gap156c: queued tick owns a future slot -> never counted by question 5.
	newProject("gap156c", "/tmp/gap156c", ts(2*time.Hour), gap156Namespace, true)
	// gap156d: DISABLED and namespace-less — its ticks must land under the "-"
	// namespace key and must not enter the cooldown-expired gauge.
	newProject("gap156d", "/tmp/gap156d", "", "", false)

	// insertTick writes one tick row. durationMs == 0 leaves completed_at NULL
	// (the row is then outside the duration sample by design); spawnedAgo < 0
	// leaves spawned_at NULL (a pre-spawn queued row).
	insertTick := func(id, project, status, outcome string, commits int, spawnedAgo time.Duration, durationMs int, errText string) {
		t.Helper()
		var spawned, completed interface{}
		if spawnedAgo >= 0 {
			spawned = ts(spawnedAgo)
		}
		if durationMs > 0 {
			completed = now.Add(-spawnedAgo + time.Duration(durationMs)*time.Millisecond).Format(time.RFC3339)
		}
		var outcomeArg interface{}
		if outcome != "" {
			outcomeArg = outcome
		}
		var errArg interface{}
		if errText != "" {
			errArg = errText
		}
		if _, err := db.ExecContext(ctx, `
INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, commits, error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, project, status, outcomeArg, spawned, completed, commits, errArg, ts(spawnedAgo)); err != nil {
			t.Fatalf("insert tick %s: %v", id, err)
		}
	}

	// 10 known durations (question 6 sample core).
	for i := 1; i <= 10; i++ {
		insertTick(fmt.Sprintf("g156-d%02d", i), "gap156a", "completed", "committed", 1,
			2*time.Hour, i*100000, "")
	}
	// zero-output committed (question 8).
	insertTick("g156-zero", "gap156a", "completed", "committed", 0, time.Hour, 5000, "")
	insertTick("g156-dry", "gap156a", "completed", "dry_run", 0, 45*time.Minute, 3000, "")
	// drain-class failure (question 7).
	insertTick("g156-drain", "gap156b", "failed", "failed", 0, 30*time.Minute, 2000,
		`gateway POST: HTTP 503: {"error":"gateway is draining"}`)
	// namespace-less tick: project gap156d has no namespace_id -> "-".
	insertTick("g156-nons", "gap156d", "completed", "committed", 1, 40*time.Minute, 10000, "")
	// live occupancy (question 4).
	insertTick("g156-run", "gap156b", "running", "", 0, 5*time.Minute, 0, "")
	insertTick("g156-timeout", "gap156c", "timeout", "timeout", 0, 20*time.Minute, 0, "")
	insertTick("g156-queued", "gap156c", "queued", "", 0, -1, 0, "")

	// orphan re-nudges (question 3) — 3 days old, so the nudges block must
	// still count them (it is all-time) while every windowed block must not.
	for _, n := range []struct {
		id, path string
		count    int
	}{
		{"g156-nudge1", scheduler.OrphanReasonDrainTimeout, 2},
		{"g156-nudge2", scheduler.OrphanReasonZombieReap, 1},
	} {
		insertTick(n.id, "gap156a", "completed", "failed", 0, 72*time.Hour, 60000, "")
		if _, err := db.ExecContext(ctx,
			`UPDATE ticks SET nudge_count = ?, orphan_reason = ? WHERE id = ?`,
			n.count, n.path, n.id); err != nil {
			t.Fatalf("stamp nudge row %s: %v", n.id, err)
		}
	}
}

// gap156GetMetrics performs the real HTTP request against the server's Handler
// and returns the decoded top-level object.
func gap156GetMetrics(t *testing.T, s *Server) (int, string, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil))
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/v1/metrics: %v (body: %s)", err, rec.Body.String())
	}
	return rec.Code, rec.Header().Get("Content-Type"), body
}

// gap156Block pulls a top-level block out of the response, failing when it is
// missing or not an object.
func gap156Block(t *testing.T, body map[string]interface{}, name string) map[string]interface{} {
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

// gap156Num asserts the value at path is a JSON number and returns it as int.
func gap156Num(t *testing.T, parent map[string]interface{}, path string) int {
	t.Helper()
	raw, ok := parent[path]
	if !ok {
		t.Fatalf("missing number field %q", path)
	}
	f, ok := raw.(float64)
	if !ok {
		t.Fatalf("field %q is %T, want number", path, raw)
	}
	return int(f)
}

// gap156Map asserts the value at path is a JSON object.
func gap156Map(t *testing.T, parent map[string]interface{}, path string) map[string]interface{} {
	t.Helper()
	raw, ok := parent[path]
	if !ok {
		t.Fatalf("missing object field %q", path)
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("field %q is %T, want object", path, raw)
	}
	return m
}

// TestSCHEDGAP156_Metrics is the acceptance test: one request must answer all
// eight questions with exact, sourced numbers.
func TestSCHEDGAP156_Metrics(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	gap156Seed(t, db)

	// AC5 names NewServer(db, nil) as the construction under test; the
	// resolved-config snapshot is set so ticks.global_cap has the same source
	// /api/v1/config serves (AC2).
	s := NewServer(db, nil)
	s.SetResolvedConfig(ResolvedConfig{MaxConcurrent: 4})

	code, ctype, body := gap156GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (body: %v)", code, body)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}

	// --- AC1: every top-level block is present -------------------------------
	for _, block := range []string{
		"generated_at", "uptime_s", "sources", "spawns", "deferrals", "nudges",
		"ticks", "tick_duration_ms", "gateway", "outcomes",
	} {
		if _, ok := body[block]; !ok {
			t.Errorf("AC1: top-level block %q missing", block)
		}
	}
	if _, err := time.Parse(time.RFC3339, body["generated_at"].(string)); err != nil {
		t.Errorf("generated_at is not RFC3339: %v", body["generated_at"])
	}
	if uptime := gap156Num(t, body, "uptime_s"); uptime < 0 {
		t.Errorf("uptime_s = %d, want >= 0", uptime)
	}

	// --- AC2/AC3: spawns (question 1) ---------------------------------------
	spawns := gap156Block(t, body, "spawns")
	if spawns["available"] != true {
		t.Errorf("spawns.available = %v, want true", spawns["available"])
	}
	if got := gap156Num(t, spawns, "total"); got != 16 {
		t.Errorf("spawns.total = %d, want 16 (15 outcome-stamped rows + 1 running)", got)
	}
	byNS := gap156Map(t, spawns, "by_namespace")
	wantNS := map[string]int{gap156Namespace: 15, "-": 1}
	if len(byNS) != len(wantNS) {
		t.Errorf("spawns.by_namespace = %v, want %v", byNS, wantNS)
	}
	for ns, want := range wantNS {
		if got := int(byNS[ns].(float64)); got != want {
			t.Errorf("spawns.by_namespace[%q] = %d, want %d", ns, got, want)
		}
	}
	byOutcome := gap156Map(t, spawns, "by_outcome")
	wantOutcome := map[string]int{"committed": 12, "dry_run": 1, "failed": 1, "timeout": 1}
	for outcome, want := range wantOutcome {
		raw, ok := byOutcome[outcome]
		if !ok {
			t.Fatalf("spawns.by_outcome[%q] missing (the four CHECK-constraint values must always be present)", outcome)
		}
		if got := int(raw.(float64)); got != want {
			t.Errorf("spawns.by_outcome[%q] = %d, want %d", outcome, got, want)
		}
	}

	// --- question 2: deferrals, with no Loop attached (AC3 honesty) ---------
	deferrals := gap156Block(t, body, "deferrals")
	if deferrals["available"] != false {
		t.Errorf("deferrals.available = %v, want false: this Server has no Loop, so the SCHED-GAP-155 counters have no source", deferrals["available"])
	}
	if reason, _ := deferrals["reason"].(string); reason == "" {
		t.Error("deferrals.reason is empty — an unsourced block must say what is missing")
	}

	// --- question 3: nudges by drop path (all-time) -------------------------
	nudges := gap156Block(t, body, "nudges")
	if nudges["available"] != true {
		t.Errorf("nudges.available = %v, want true", nudges["available"])
	}
	byPath := gap156Map(t, nudges, "by_path")
	wantPath := map[string]int{
		scheduler.OrphanReasonDrainTimeout: 2,
		scheduler.OrphanReasonZombieReap:   1,
	}
	if len(byPath) != len(wantPath) {
		t.Errorf("nudges.by_path = %v, want %v", byPath, wantPath)
	}
	total := 0
	for path, want := range wantPath {
		raw, ok := byPath[path]
		if !ok {
			t.Fatalf("nudges.by_path[%q] missing (nudge rows are 3 days old — the block must be all-time, not windowed)", path)
		}
		got := int(raw.(float64))
		if got != want {
			t.Errorf("nudges.by_path[%q] = %d, want %d", path, got, want)
		}
		total += got
	}
	if total != gap156AllTimeNudges {
		t.Errorf("nudges.by_path total = %d, want %d", total, gap156AllTimeNudges)
	}

	// --- questions 4 and 5: occupancy gauges vs caps -----------------------
	ticksBlock := gap156Block(t, body, "ticks")
	if ticksBlock["available"] != true {
		t.Errorf("ticks.available = %v, want true", ticksBlock["available"])
	}
	if got := gap156Num(t, ticksBlock, "active"); got != 1 {
		t.Errorf("ticks.active = %d, want 1", got)
	}
	if got := gap156Num(t, ticksBlock, "queued"); got != 1 {
		t.Errorf("ticks.queued = %d, want 1", got)
	}
	// global_cap must equal what /api/v1/config reports (same source).
	configRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(configRec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	var cfg map[string]interface{}
	if err := json.Unmarshal(configRec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode /api/v1/config: %v", err)
	}
	wantCap := int(cfg["max_concurrent"].(float64))
	if got := gap156Num(t, ticksBlock, "global_cap"); got != wantCap {
		t.Errorf("ticks.global_cap = %d, want %d (= /api/v1/config max_concurrent)", got, wantCap)
	}
	nsBlock := gap156Map(t, ticksBlock, "by_namespace")
	entry, ok := nsBlock[gap156Namespace].(map[string]interface{})
	if !ok {
		t.Fatalf("ticks.by_namespace[%q] missing or not an object: %v", gap156Namespace, nsBlock)
	}
	if got := gap156Num(t, entry, "active"); got != 1 {
		t.Errorf("ticks.by_namespace[%s].active = %d, want 1", gap156Namespace, got)
	}
	if got := gap156Num(t, entry, "queued"); got != 1 {
		t.Errorf("ticks.by_namespace[%s].queued = %d, want 1", gap156Namespace, got)
	}
	if got := gap156Num(t, entry, "cap"); got != 2 {
		t.Errorf("ticks.by_namespace[%s].cap = %d, want 2", gap156Namespace, got)
	}
	// gap156a is expired+idle; gap156b has a running tick; gap156c a queued one.
	if got := gap156Num(t, ticksBlock, "cooldown_expired_unscheduled"); got != 1 {
		t.Errorf("ticks.cooldown_expired_unscheduled = %d, want 1", got)
	}

	// --- question 6: exact nearest-rank percentiles (AC4) ------------------
	dur, ok := body["tick_duration_ms"].(map[string]interface{})
	if !ok {
		t.Fatalf("tick_duration_ms is %T, want object", body["tick_duration_ms"])
	}
	if got := dur["window"]; got != "24h" {
		t.Errorf("tick_duration_ms.window = %v, want \"24h\"", got)
	}
	if got := gap156Num(t, dur, "count"); got != 14 {
		t.Errorf("tick_duration_ms.count = %d, want 14", got)
	}
	for field, want := range map[string]int{"p50": 300000, "p90": 900000, "p99": 1000000} {
		raw, ok := dur[field]
		if !ok {
			t.Fatalf("tick_duration_ms.%s missing", field)
		}
		got, ok := raw.(float64)
		if !ok {
			t.Fatalf("tick_duration_ms.%s is %T, want number", field, raw)
		}
		if int(got) != want {
			t.Errorf("AC4: tick_duration_ms.%s = %d, want exactly %d", field, int(got), want)
		}
	}

	// --- question 7: drain-class 503s --------------------------------------
	gateway := gap156Block(t, body, "gateway")
	if gateway["available"] != true {
		t.Errorf("gateway.available = %v, want true", gateway["available"])
	}
	if got := gap156Num(t, gateway, "drain_503"); got != 1 {
		t.Errorf("gateway.drain_503 = %d, want 1", got)
	}

	// --- question 8: zero-output 'committed' ticks -------------------------
	outcomes := gap156Block(t, body, "outcomes")
	if outcomes["available"] != true {
		t.Errorf("outcomes.available = %v, want true", outcomes["available"])
	}
	if got := gap156Num(t, outcomes, "zero_output_committed"); got != 1 {
		t.Errorf("outcomes.zero_output_committed = %d, want 1", got)
	}

	// --- AC3: the sources map names the source (or the gap) per block ------
	sources := gap156Block(t, body, "sources")
	for _, block := range []string{"spawns", "deferrals", "nudges", "ticks", "tick_duration_ms", "gateway", "outcomes"} {
		desc, ok := sources[block].(string)
		if !ok || len(desc) < 20 {
			t.Errorf("sources[%q] is missing or too short to name a source: %v", block, sources[block])
		}
	}

	// --- AC5: ticks.active agrees with /api/v1/status for the same DB ------
	statusRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(statusRec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	var status map[string]interface{}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode /api/v1/status: %v", err)
	}
	if got, want := gap156Num(t, ticksBlock, "active"), int(status["active_ticks"].(float64)); got != want {
		t.Errorf("ticks.active = %d, but /api/v1/status active_ticks = %d — the two surfaces disagree", got, want)
	}

	// --- AC1: non-GET is 405 in the repo's writeError shape ----------------
	postRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/api/v1/metrics", nil))
	if postRec.Code != 405 {
		t.Errorf("POST /api/v1/metrics = %d, want 405", postRec.Code)
	}
	var errBody map[string]interface{}
	if err := json.Unmarshal(postRec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode 405 body: %v", err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Errorf("405 body has no \"error\" key (writeError shape): %s", postRec.Body.String())
	}
}

// TestSCHEDGAP156_MetricsWithLoop proves that when a Loop IS attached every
// §2.1 path is present and typed — including deferrals.by_reason, sourced from
// the SCHED-GAP-155 admission counters (all reasons at 0 for a Loop that has
// never evaluated, which is a real query result, not a placeholder).
func TestSCHEDGAP156_MetricsWithLoop(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	gap156Seed(t, db)

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	defer loop.Stop()
	s := NewServer(db, loop)
	s.SetResolvedConfig(ResolvedConfig{MaxConcurrent: 4})

	code, _, body := gap156GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200", code)
	}
	deferrals := gap156Block(t, body, "deferrals")
	if deferrals["available"] != true {
		t.Fatalf("deferrals.available = %v, want true when a Loop is attached", deferrals["available"])
	}
	byReason := gap156Map(t, deferrals, "by_reason")
	for _, reason := range []string{
		scheduler.AdmissionReasonOK,
		scheduler.AdmissionReasonCap,
		scheduler.AdmissionReasonLoadGate,
		scheduler.AdmissionReasonCooldown,
		scheduler.AdmissionReasonTasksNoWork,
		scheduler.AdmissionReasonBoardUnowned,
		scheduler.AdmissionReasonBudget,
		scheduler.AdmissionReasonTasksDeferred,
	} {
		raw, ok := byReason[reason]
		if !ok {
			t.Errorf("deferrals.by_reason[%q] missing — the counter vocabulary must be complete from boot", reason)
			continue
		}
		if got := int(raw.(float64)); got != 0 {
			t.Errorf("deferrals.by_reason[%q] = %d, want 0 (the Loop never evaluated)", reason, got)
		}
	}
	if _, ok := deferrals["admitted_by_namespace"]; !ok {
		t.Error("deferrals.admitted_by_namespace missing")
	}
	if _, ok := deferrals["passes"]; !ok {
		t.Error("deferrals.passes missing")
	}
	// Every other block must still be sourced (nothing regressed by attaching
	// a Loop).
	for _, block := range []string{"spawns", "nudges", "ticks", "gateway", "outcomes"} {
		if got := gap156Block(t, body, block)["available"]; got != true {
			t.Errorf("%s.available = %v, want true", block, got)
		}
	}
}

// TestSCHEDGAP156_UnavailableWhenUnsourced pins the honesty rule structurally:
// with the DB closed the windowed blocks must report {"available": false,
// "reason": ...} — never a fabricated 0.
func TestSCHEDGAP156_UnavailableWhenUnsourced(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := NewServer(db, nil)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	code, _, body := gap156GetMetrics(t, s)
	if code != 200 {
		t.Fatalf("GET /api/v1/metrics = %d, want 200 (a dead source is a reported gap, not a 5xx)", code)
	}
	for _, block := range []string{"spawns", "nudges", "ticks", "gateway", "outcomes"} {
		got := gap156Block(t, body, block)
		if got["available"] != false {
			t.Errorf("AC3: %s.available = %v with the DB closed, want false", block, got["available"])
		}
		reason, _ := got["reason"].(string)
		if reason == "" {
			t.Errorf("AC3: %s has available=false but no reason", block)
		}
	}
	if got := gap156Block(t, body, "deferrals")["available"]; got != false {
		t.Errorf("deferrals.available = %v with no Loop and a dead DB, want false", got)
	}
}

// TestSCHEDGAP156_DurationPercentilesEmptyWindow proves the null contract: an
// empty sample reports count 0 with p50/p90/p99 JSON null, NOT 0 (zero
// milliseconds would claim a measured duration).
func TestSCHEDGAP156_DurationPercentilesEmptyWindow(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      "gap156a",
		RepoURL:   "https://example.com/gap156a",
		Workdir:   "/tmp/gap156a",
		Weight:    10,
		Priority:  5,
		CooldownS: 3600,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('g156-empty', 'gap156a', 'queued', '2026-09-18T00:00:00Z')`); err != nil {
		t.Fatalf("insert queued tick: %v", err)
	}

	s := NewServer(db, nil)
	_, _, body := gap156GetMetrics(t, s)
	dur, ok := body["tick_duration_ms"].(map[string]interface{})
	if !ok {
		t.Fatalf("tick_duration_ms is %T, want object", body["tick_duration_ms"])
	}
	if got := gap156Num(t, dur, "count"); got != 0 {
		t.Errorf("tick_duration_ms.count = %d, want 0", got)
	}
	for _, field := range []string{"p50", "p90", "p99"} {
		raw, ok := dur[field]
		if !ok {
			t.Fatalf("tick_duration_ms.%s missing", field)
		}
		if raw != nil {
			t.Errorf("tick_duration_ms.%s = %v, want null on an empty sample (0 would be a fabricated measurement)", field, raw)
		}
	}
}

// TestSCHEDGAP156_NearestRankRule pins the percentile rule itself on
// independently computed tables: idx = ceil(pct/100 * n) - 1, clamped to
// [0, n-1].
func TestSCHEDGAP156_NearestRankRule(t *testing.T) {
	cases := []struct {
		name   string
		sample []int
		pct    int
		want   int
	}{
		// n = 4: ascending 10,20,30,40.
		{"n4 p50", []int{10, 20, 30, 40}, 50, 20}, // ceil(2)   -1 = 1
		{"n4 p90", []int{10, 20, 30, 40}, 90, 40}, // ceil(3.6) -1 = 3
		{"n4 p99", []int{10, 20, 30, 40}, 99, 40}, // ceil(3.96)-1 = 3
		// n = 5: ascending 1,2,3,4,5.
		{"n5 p50", []int{1, 2, 3, 4, 5}, 50, 3}, // ceil(2.5) -1 = 2
		{"n5 p90", []int{1, 2, 3, 4, 5}, 90, 5}, // ceil(4.5) -1 = 4
		{"n5 p99", []int{1, 2, 3, 4, 5}, 99, 5}, // ceil(4.95)-1 = 4
		// n = 1: the single sample is every percentile (clamp lower bound).
		{"n1 p50", []int{7}, 50, 7}, // ceil(0.5)-1 = 0 (clamped from -1)
		{"n1 p99", []int{7}, 99, 7}, // ceil(0.99)-1 = 0 (clamped from -1)
		// n = 100: p99 selects the last element, p50 the 50th.
		{"n100 p50", schedgap156Hundred(), 50, 50},
		{"n100 p90", schedgap156Hundred(), 90, 90},
		{"n100 p99", schedgap156Hundred(), 99, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearestRankPercentile(tc.sample, tc.pct); got != tc.want {
				t.Errorf("nearestRankPercentile(%v, %d) = %d, want %d", tc.sample, tc.pct, got, tc.want)
			}
		})
	}
}

// schedgap156Hundred returns an ascending sample 1..100 (value == index).
func schedgap156Hundred() []int {
	out := make([]int, 0, 100)
	for i := 1; i <= 100; i++ {
		out = append(out, i)
	}
	return out
}

// TestSCHEDGAP156_OpenAPIDocumentsMetrics covers AC6: the served spec parses
// as valid JSON and carries the new path.
func TestSCHEDGAP156_OpenAPIDocumentsMetrics(t *testing.T) {
	if !json.Valid(openapiSpec) {
		t.Fatal("AC6: openapiSpec is not valid JSON")
	}
	var spec map[string]interface{}
	if err := json.Unmarshal(openapiSpec, &spec); err != nil {
		t.Fatalf("AC6: unmarshal openapiSpec: %v", err)
	}
	paths, ok := spec["paths"].(map[string]interface{})
	if !ok {
		t.Fatal("AC6: openapiSpec has no paths object")
	}
	if _, ok := paths["/api/v1/metrics"]; !ok {
		t.Error("AC6: openapiSpec has no /api/v1/metrics path")
	}

	// The same bytes the daemon serves must parse too.
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	s := NewServer(db, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/v1/openapi.json = %d, want 200", rec.Code)
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Error("AC6: served /api/v1/openapi.json is not valid JSON")
	}
}
