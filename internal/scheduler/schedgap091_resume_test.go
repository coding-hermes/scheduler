package scheduler

// SCHED-GAP-091 — session resume-after-restart tests.
//
// Criteria (board row + judge): kill the gateway mid-tick; on restart the
// tick's session is re-nudged and completes without manual intervention;
// no duplicate commits; nudge cap prevents zombie resurrection; live
// (healthy) ticks are never orphaned.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resumeGateway is the test gateway wired into a loop's spawner: /health
// and /v1/responses both flip on healthy; every accepted spawn is counted
// and its request body captured.
type resumeGateway struct {
	srv     *httptest.Server
	healthy atomic.Bool
	spawns  atomic.Int32
	body    atomic.Pointer[string]
}

func newResumeGateway(t *testing.T) *resumeGateway {
	t.Helper()
	g := &resumeGateway{}
	g.healthy.Store(true)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !g.healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			if !g.healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			b, _ := io.ReadAll(r.Body)
			s := string(b)
			g.body.Store(&s)
			g.spawns.Add(1)
			schedGap080CompletedResponse(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// wire attaches the gateway to the loop's spawner (no exec fallback).
func (g *resumeGateway) wire(l *Loop) {
	l.SetGatewayClient(NewGatewayClient(g.srv.URL, "sk-daemon-shared", 5*time.Second))
	l.spawner.SetNoExecFallback(true)
}

// orphanTickRow inserts a terminal orphaned tick row the way the drop paths
// leave them (status failed/timeout + orphaned_at/orphan_reason).
func orphanTickRow(t *testing.T, db *sql.DB, tickID, project, status, reason string, nudgeCount int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, created_at, orphaned_at, orphan_reason, nudge_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tickID, project, status, now, now, now, now, reason, nudgeCount); err != nil {
		t.Fatalf("insert orphaned tick %s: %v", tickID, err)
	}
}

// insertQueuedTick inserts a queued (in-flight) tick row.
func insertQueuedTick(t *testing.T, db *sql.DB, tickID, project string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		 VALUES (?, ?, 'queued', ?, ?)`, tickID, project, now, now); err != nil {
		t.Fatalf("insert queued tick %s: %v", tickID, err)
	}
}

// setProjectPrompt writes the project-level prompt the scan joins in.
func setProjectPrompt(t *testing.T, db *sql.DB, project, prompt string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET prompt = ? WHERE name = ?`, prompt, project); err != nil {
		t.Fatalf("set prompt on %s: %v", project, err)
	}
}

// waitForTerminal polls until the tick row reaches a terminal status.
func waitForTerminal(t *testing.T, db *sql.DB, tickID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
		if err == sql.ErrNoRows {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("query tick %s: %v", tickID, err)
		}
		if status == "completed" || status == "failed" || status == "timeout" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tick %s did not reach a terminal state within %v", tickID, timeout)
}

// resumeHighEventCount counts HIGH resume events naming the tick.
func resumeHighEventCount(t *testing.T, db *sql.DB, tickID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='resume'
		 AND json_extract(details,'$.tick_id')=?`, tickID).Scan(&n); err != nil {
		t.Fatalf("resume event count: %v", err)
	}
	return n
}

func TestSCHEDGAP091_ContinuationPromptShape(t *testing.T) {
	p := buildContinuationPrompt("Do the thing.", "tick-1", "sess-9", OrphanReasonDrainTimeout)
	if !strings.Contains(p, "CONTINUATION, not a restart") {
		t.Fatalf("continuation preamble missing: %q", p)
	}
	if !strings.Contains(p, "tick-1") || !strings.Contains(p, OrphanReasonDrainTimeout) {
		t.Fatalf("preamble must name tick and reason: %q", p)
	}
	if !strings.Contains(p, "sess-9") {
		t.Fatalf("preamble must carry previous session id: %q", p)
	}
	if !strings.Contains(p, "Do the thing.") {
		t.Fatalf("project prompt must be embedded: %q", p)
	}
}

func TestSCHEDGAP091_OrphansToResumeSelection(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "res-a")
	mustCreateProjectINFRA012(t, db, "res-b")
	mustCreateProjectINFRA012(t, db, "res-capped") // nudge_count at cap
	mustCreateProjectINFRA012(t, db, "res-live")   // has a queued tick — not stalled

	orphanTickRow(t, db, "orphan-1", "res-a", "failed", OrphanReasonStartupReap, 0)
	orphanTickRow(t, db, "orphan-2", "res-b", "timeout", OrphanReasonZombieReap, 0)
	orphanTickRow(t, db, "orphan-3", "res-capped", "failed", OrphanReasonDrainTimeout, MaxNudgesPerTick)

	// res-live: an orphaned tick BUT the project already has a queued tick.
	orphanTickRow(t, db, "orphan-4", "res-live", "failed", OrphanReasonStartupReap, 0)
	insertQueuedTick(t, db, "queued-1", "res-live")

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	got := l.orphansToResume(context.Background())
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.id] = true
	}
	if !ids["orphan-1"] || !ids["orphan-2"] {
		t.Fatalf("orphan-1/orphan-2 must be resumable, got %v", ids)
	}
	if ids["orphan-3"] {
		t.Fatal("nudge-capped tick must NOT be resumable")
	}
	if ids["orphan-4"] {
		t.Fatal("project with an in-flight tick must NOT be re-nudged")
	}
}

func TestSCHEDGAP091_DrainTimeoutStampsOrphan(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "drain-proj")
	insertRunningTick(t, db, "drain-tick", "drain-proj", 0)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.abortInFlightTicks()

	if got := tickStatusOf(t, db, "drain-tick"); got != "failed" {
		t.Fatalf("status = %q, want failed", got)
	}
	var reason string
	if err := db.QueryRow(`SELECT orphan_reason FROM ticks WHERE id = ?`, "drain-tick").Scan(&reason); err != nil {
		t.Fatalf("orphan_reason query: %v", err)
	}
	if reason != OrphanReasonDrainTimeout {
		t.Fatalf("orphan_reason = %q, want %q", reason, OrphanReasonDrainTimeout)
	}
}

func TestSCHEDGAP091_ZombieReapStampsOrphan(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "zombie-proj")
	// Dead pid (12345 does not exist) — the reaper's /proc check kills it.
	insertRunningTick(t, db, "zombie-tick", "zombie-proj", 12345)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.reapZombies()

	if got := tickStatusOf(t, db, "zombie-tick"); got != "timeout" {
		t.Fatalf("status = %q, want timeout", got)
	}
	var reason string
	if err := db.QueryRow(`SELECT orphan_reason FROM ticks WHERE id = ?`, "zombie-tick").Scan(&reason); err != nil {
		t.Fatalf("orphan_reason query: %v", err)
	}
	if reason != OrphanReasonZombieReap {
		t.Fatalf("orphan_reason = %q, want %q", reason, OrphanReasonZombieReap)
	}
}

func TestSCHEDGAP091_StartupCleanupLeavesLiveGatewayTickUnstamped(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "live-proj")
	// Fresh heartbeat pid=0 row — LIVE, must survive startup cleanup
	// untouched (INFRA-012 guard) and must NOT get an orphan stamp.
	insertRunningTick(t, db, "live-tick", "live-proj", 0)
	if _, err := db.Exec(`UPDATE ticks SET heartbeat_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), "live-tick"); err != nil {
		t.Fatalf("set heartbeat: %v", err)
	}

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.cleanDanglingOnStartup()

	if got := tickStatusOf(t, db, "live-tick"); got != "running" {
		t.Fatalf("live gateway tick status = %q, want running", got)
	}
	var orphanedAt any
	if err := db.QueryRow(`SELECT orphaned_at FROM ticks WHERE id = ?`, "live-tick").Scan(&orphanedAt); err != nil {
		t.Fatalf("orphaned_at query: %v", err)
	}
	if orphanedAt != nil {
		t.Fatalf("live tick must not be stamped orphaned, got %v", orphanedAt)
	}
}

func TestSCHEDGAP091_ResumeNudgesOrphanAndCompletes(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "resume-proj")
	orphanTickRow(t, db, "resume-tick", "resume-proj", "failed", OrphanReasonStartupReap, 0)
	setProjectPrompt(t, db, "resume-proj", "Board task: finish the resume feature.")

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)

	l.resumeOrphans("test")

	// The nudge tick row must exist and reach a terminal state.
	waitForTerminal(t, db, "resume-tick-nudge1", 10*time.Second)
	if gw.spawns.Load() != 1 {
		t.Fatalf("gateway spawn count = %d, want 1", gw.spawns.Load())
	}
	// The nudge spawn must carry the CONTINUATION preamble + project prompt.
	if gw.body.Load() == nil {
		t.Fatal("no spawn body captured")
	}
	var req struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(*gw.body.Load()), &req); err != nil {
		t.Fatalf("decode spawn body: %v", err)
	}
	if !strings.Contains(req.Input, "CONTINUATION, not a restart") {
		t.Fatalf("nudge prompt missing continuation preamble: %q", req.Input)
	}
	if !strings.Contains(req.Input, "finish the resume feature") {
		t.Fatalf("nudge prompt missing project prompt: %q", req.Input)
	}
	// The original row keeps its nudge counter.
	var count int
	if err := db.QueryRow(`SELECT nudge_count FROM ticks WHERE id = ?`, "resume-tick").Scan(&count); err != nil {
		t.Fatalf("nudge_count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("nudge_count = %d, want 1", count)
	}
	// One resume HIGH event for the nudge.
	if n := resumeHighEventCount(t, db, "resume-tick"); n != 1 {
		t.Fatalf("resume HIGH events = %d, want 1", n)
	}
}

func TestSCHEDGAP091_NudgeCapEmitsNeedsHuman(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "capped-proj")
	orphanTickRow(t, db, "capped-tick", "capped-proj", "failed", OrphanReasonDrainTimeout, MaxNudgesPerTick)

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)

	l.resumeOrphans("test") // must NOT nudge (cap reached)
	l.resumeNeedsHuman("test")

	if gw.spawns.Load() != 0 {
		t.Fatalf("capped tick must not spawn, got %d spawns", gw.spawns.Load())
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE id LIKE 'capped-tick-nudge%'`).Scan(&rows); err != nil {
		t.Fatalf("nudge row count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("capped tick must not create nudge rows, got %d", rows)
	}
	// The needs-human HIGH event names the tick.
	if n := resumeHighEventCount(t, db, "capped-tick"); n != 1 {
		t.Fatalf("needs-human HIGH events = %d, want 1", n)
	}
}

func TestSCHEDGAP091_DisabledProjectNeedsHuman(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "disabled-proj")
	orphanTickRow(t, db, "disabled-tick", "disabled-proj", "failed", OrphanReasonStartupReap, 0)
	if _, err := db.Exec(`UPDATE projects SET enabled = 0 WHERE name = 'disabled-proj'`); err != nil {
		t.Fatalf("disable project: %v", err)
	}

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)

	l.resumeOrphans("test")
	l.resumeNeedsHuman("test")

	if gw.spawns.Load() != 0 {
		t.Fatalf("disabled project must not spawn, got %d", gw.spawns.Load())
	}
	if n := resumeHighEventCount(t, db, "disabled-tick"); n != 1 {
		t.Fatalf("needs-human HIGH events = %d, want 1", n)
	}
}

func TestSCHEDGAP091_DeadGatewayNoNudge(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "dead-gw-proj")
	orphanTickRow(t, db, "dead-gw-tick", "dead-gw-proj", "failed", OrphanReasonStartupReap, 0)

	gw := newResumeGateway(t)
	gw.healthy.Store(false)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)

	l.resumeOrphans("test")
	l.resumeNeedsHuman("test")

	if gw.spawns.Load() != 0 {
		t.Fatalf("dead gateway must not spawn, got %d", gw.spawns.Load())
	}
	if n := resumeHighEventCount(t, db, "dead-gw-tick"); n != 0 {
		t.Fatalf("dead gateway must not emit resume events, got %d", n)
	}
}
