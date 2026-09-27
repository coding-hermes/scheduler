package scheduler

import (
	"database/sql"
	"encoding/json"
	"testing"
)

// insertLaneOutputProject inserts a project row bound to a namespace so the
// lane-family accounting can resolve it. namespace may be "" to leave the
// row unscheduled (NULL namespace_id). The namespace row is created when
// missing — the gap060 template carries the schema but no seed rows, and
// namespace_id is a real FK.
func insertLaneOutputProject(t *testing.T, db *sql.DB, name, namespace string) {
	t.Helper()
	if namespace != "" {
		if _, err := db.Exec(`INSERT OR IGNORE INTO namespaces (id) VALUES (?)`,
			namespace); err != nil {
			t.Fatalf("insert namespace %s: %v", namespace, err)
		}
	}
	var ns any
	if namespace != "" {
		ns = namespace
	}
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at, namespace_id)
		VALUES (?, 'https://github.com/example/x', '/tmp/x', 10, 5, 900, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'), ?)`,
		name, ns,
	); err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

// insertLaneOutputTick inserts a terminal tick row for project with the
// given status and commit anatomy, mirroring the columns
// persistGitCommitSignals stamps (code_commits/board_commits carry the -1
// unmeasured sentinel; commits is the raw claim).
func insertLaneOutputTick(t *testing.T, db *sql.DB, id, project, status string, code, board, commits int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at, commits,
		 code_commits, board_commits, created_at)
		VALUES (?, ?, ?, 'committed', datetime('now'), datetime('now'), ?, ?, ?, datetime('now'))`,
		id, project, status, commits, code, board,
	); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// readLaneOutputCounters reads the eight SCHED-GAP-177 counters for one row.
func readLaneOutputCounters(t *testing.T, db *sql.DB, name string) (qaOut, qaStreak, pmOut, pmStreak, syncOut, syncStreak, dogOut, dogStreak int) {
	t.Helper()
	err := db.QueryRow(`SELECT qa_output_count, qa_zero_output_streak,
	       pm_output_count, pm_zero_output_streak,
	       sync_output_count, sync_zero_output_streak,
	       dogfood_output_count, dogfood_zero_output_streak
	FROM projects WHERE name = ?`, name).
		Scan(&qaOut, &qaStreak, &pmOut, &pmStreak, &syncOut, &syncStreak, &dogOut, &dogStreak)
	if err != nil {
		t.Fatalf("read lane-output counters for %s: %v", name, err)
	}
	return
}

// countLaneOutputHighEvents counts HIGH events whose message names project.
func countLaneOutputHighEvents(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events
	WHERE severity = 'HIGH' AND component = 'lane-output' AND message LIKE ?`,
		"%"+project+"%").Scan(&n); err != nil {
		t.Fatalf("count lane-output HIGH events for %s: %v", project, err)
	}
	return n
}

func TestLaneFamilyResolution(t *testing.T) {
	cases := map[string]string{
		"qa": "qa", "pm": "pm", "dogfood": "dogfood",
		"sync": "sync", "duckbrain-sync": "sync",
		// Non-families resolve to "" — the accounting must skip them.
		"": "", "coding-hermes": "", "releases": "", "qa-extra": "", "sync2": "",
	}
	for ns, want := range cases {
		if got := laneFamily(ns); got != want {
			t.Errorf("laneFamily(%q) = %q, want %q", ns, got, want)
		}
	}
}

func TestLaneOutput_AccountingPerFamily(t *testing.T) {
	db := newTestDB(t)

	for ns, name := range map[string]string{
		"qa": "lane-qa", "pm": "lane-pm", "duckbrain-sync": "lane-sync", "dogfood": "lane-dog",
	} {
		insertLaneOutputProject(t, db, name, ns)
	}

	// One output tick (code commits) and one zero-output tick per family.
	insertLaneOutputTick(t, db, "t-qa-1", "lane-qa", "completed", 2, 0, 2)
	recordLaneFamilyOutput(db, "lane-qa", "t-qa-1")
	insertLaneOutputTick(t, db, "t-qa-2", "lane-qa", "completed", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-qa", "t-qa-2")

	// Each family's counters live on that family's own lane row.
	insertLaneOutputTick(t, db, "t-pm-1", "lane-pm", "completed", 0, 3, 3)
	recordLaneFamilyOutput(db, "lane-pm", "t-pm-1")
	_, _, pmOut, pmStreak, _, _, _, _ := readLaneOutputCounters(t, db, "lane-pm")

	// Sync (duckbrain-sync namespace) zero-output tick.
	insertLaneOutputTick(t, db, "t-sy-1", "lane-sync", "completed", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-sync", "t-sy-1")
	_, _, _, _, syncOut, syncStreak, _, _ := readLaneOutputCounters(t, db, "lane-sync")

	// Dogfood: timeout tick with no commits still counts as zero-output.
	insertLaneOutputTick(t, db, "t-dog-1", "lane-dog", "timeout", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-dog", "t-dog-1")
	_, _, _, _, _, _, dogOut, dogStreak := readLaneOutputCounters(t, db, "lane-dog")

	qaOut, qaStreak, _, _, _, _, _, _ := readLaneOutputCounters(t, db, "lane-qa")
	if qaOut != 1 || qaStreak != 1 {
		t.Errorf("qa counters = (%d out, %d streak), want (1, 1)", qaOut, qaStreak)
	}
	if pmOut != 1 || pmStreak != 0 {
		t.Errorf("pm counters = (%d out, %d streak), want (1, 0)", pmOut, pmStreak)
	}
	if syncOut != 0 || syncStreak != 1 {
		t.Errorf("sync counters = (%d out, %d streak), want (0, 1)", syncOut, syncStreak)
	}
	if dogOut != 0 || dogStreak != 1 {
		t.Errorf("dogfood counters = (%d out, %d streak), want (0, 1)", dogOut, dogStreak)
	}

	// Cross-family isolation: the counters live on the row that ticked, and
	// a family's own pair is the only one written.
	_, _, _, _, _, _, dogOut2, _ := readLaneOutputCounters(t, db, "lane-qa")
	if dogOut2 != 0 {
		t.Errorf("qa lane dogfood_output_count = %d, want 0 (family isolation)", dogOut2)
	}

	// No HIGH events yet — every streak is far below the threshold.
	for _, name := range []string{"lane-qa", "lane-pm", "lane-sync", "lane-dog"} {
		if n := countLaneOutputHighEvents(t, db, name); n != 0 {
			t.Errorf("%s: %d HIGH lane-output events before threshold, want 0", name, n)
		}
	}
}

func TestLaneOutput_OutputResetsStreak(t *testing.T) {
	db := newTestDB(t)
	insertLaneOutputProject(t, db, "lane-qa", "qa")

	// Build a 5-tick zero-output streak...
	for i := 0; i < 5; i++ {
		id := "t-z-" + string(rune('a'+i))
		insertLaneOutputTick(t, db, id, "lane-qa", "completed", 0, 0, 0)
		recordLaneFamilyOutput(db, "lane-qa", id)
	}
	_, qaStreak, _, _, _, _, _, _ := readLaneOutputCounters(t, db, "lane-qa")
	if qaStreak != 5 {
		t.Fatalf("streak after 5 zero ticks = %d, want 5", qaStreak)
	}

	// ...then an output tick must reset it and count the output.
	insertLaneOutputTick(t, db, "t-ok", "lane-qa", "completed", 1, 0, 1)
	recordLaneFamilyOutput(db, "lane-qa", "t-ok")
	qaOut, qaStreak, _, _, _, _, _, _ := readLaneOutputCounters(t, db, "lane-qa")
	if qaOut != 1 || qaStreak != 0 {
		t.Errorf("after output tick: (%d out, %d streak), want (1, 0)", qaOut, qaStreak)
	}
}

func TestLaneOutput_HighEventAtThreshold(t *testing.T) {
	db := newTestDB(t)
	insertLaneOutputProject(t, db, "lane-pm", "pm")

	// 8 consecutive zero-output ticks: the 8th crosses the threshold and
	// must emit a HIGH event naming the lane.
	for i := 1; i <= 8; i++ {
		id := "t-h-" + string(rune('a'+i))
		insertLaneOutputTick(t, db, id, "lane-pm", "completed", 0, 0, 0)
		recordLaneFamilyOutput(db, "lane-pm", id)
	}
	if n := countLaneOutputHighEvents(t, db, "lane-pm"); n < 1 {
		t.Fatalf("HIGH lane-output events after 8 zero ticks = %d, want >= 1", n)
	}

	// The event carries the family and the streak in its details.
	var severity, component, message, details string
	err := db.QueryRow(`SELECT severity, component, message, details FROM events
	WHERE component = 'lane-output' AND message LIKE '%lane-pm%' ORDER BY id DESC LIMIT 1`).
		Scan(&severity, &component, &message, &details)
	if err != nil {
		t.Fatalf("read emitted event: %v", err)
	}
	if severity != "HIGH" {
		t.Errorf("event severity = %q, want HIGH", severity)
	}
	if component != "lane-output" {
		t.Errorf("event component = %q, want lane-output", component)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(details), &d); err != nil {
		t.Fatalf("event details not JSON: %v (%q)", err, details)
	}
	if d["lane_family"] != "pm" {
		t.Errorf("event details lane_family = %v, want pm", d["lane_family"])
	}
	if d["project"] != "lane-pm" {
		t.Errorf("event details project = %v, want lane-pm", d["project"])
	}

	// Streak keeps counting past the threshold (>= semantics).
	_, _, _, pmStreak, _, _, _, _ := readLaneOutputCounters(t, db, "lane-pm")
	if pmStreak != 8 {
		t.Errorf("streak after 8 zero ticks = %d, want 8", pmStreak)
	}
}

func TestLaneOutput_SkipsNonFamilyAndDeferred(t *testing.T) {
	db := newTestDB(t)

	// A coding lane (non-family namespace): accounting must be a no-op.
	insertLaneOutputProject(t, db, "lane-code", "coding-hermes")
	insertLaneOutputTick(t, db, "t-code-1", "lane-code", "completed", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-code", "t-code-1")
	qaOut, qaStreak, pmOut, pmStreak, syncOut, syncStreak, dogOut, dogStreak :=
		readLaneOutputCounters(t, db, "lane-code")
	if qaOut|qaStreak|pmOut|pmStreak|syncOut|syncStreak|dogOut|dogStreak != 0 {
		t.Errorf("coding lane counters moved: (%d,%d,%d,%d,%d,%d,%d,%d), want all 0",
			qaOut, qaStreak, pmOut, pmStreak, syncOut, syncStreak, dogOut, dogStreak)
	}

	// A NULL-namespace project: also a no-op.
	insertLaneOutputProject(t, db, "lane-null", "")
	insertLaneOutputTick(t, db, "t-null-1", "lane-null", "completed", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-null", "t-null-1")

	// A deferred tick never ran a foreman turn — it must not extend the
	// streak (same class of defect SCHED-GAP-203 closed for adaptive).
	insertLaneOutputProject(t, db, "lane-df", "dogfood")
	insertLaneOutputTick(t, db, "t-df-1", "lane-df", "deferred", 0, 0, 0)
	recordLaneFamilyOutput(db, "lane-df", "t-df-1")
	_, _, _, _, _, _, _, dogStreak2 := readLaneOutputCounters(t, db, "lane-df")
	if dogStreak2 != 0 {
		t.Errorf("deferred tick extended dogfood streak to %d, want 0", dogStreak2)
	}
}

func TestLaneOutput_UnmeasuredFallsOpen(t *testing.T) {
	db := newTestDB(t)
	insertLaneOutputProject(t, db, "lane-qa", "qa")

	// Unmeasured sentinel (-1/-1) with a real commit claim: the tick must
	// fall open to the raw commit count, not read as zero-output — a
	// measurement gap must never raise a false HIGH event.
	insertLaneOutputTick(t, db, "t-unm", "lane-qa", "completed", -1, -1, 2)
	recordLaneFamilyOutput(db, "lane-qa", "t-unm")
	qaOut, _, _, _, _, _, _, _ := readLaneOutputCounters(t, db, "lane-qa")
	if qaOut != 1 {
		t.Errorf("unmeasured tick with commits: qa_output_count = %d, want 1 (legacy fallback)", qaOut)
	}
}
