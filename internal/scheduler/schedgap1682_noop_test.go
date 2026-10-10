package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1682 acceptance at the SCHEDULER layer.
//
// A2: a foreman lane with unchanged commit+branch+board → tick flagged
// noop_flag=1, the re-entry path invoked (the gateway client receives the
// do-it-or-explain instruction on the SAME session key).
// A3: a satellite lane with unchanged state → tick closes clean, noop
// allowed, NO re-entry.
// A4: the config override — noop_allowed=false on a satellite flips it to
// re-entry; true on a foreman closes clean.
// Plus the verdict table itself and the honest-unmeasured arms.

func noop1682InitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".coding-hermes", "board"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	board := filepath.Join(dir, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.WriteFile(board, []byte("{\"id\":\"R1\",\"status\":\"pending\"}\n"), 0o600); err != nil {
		t.Fatalf("write board: %v", err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-m", "baseline", "--allow-empty"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return board
}

// noop1682Gateway is the httptest gateway that records every /v1/responses
// POST: the session key (X-Hermes-Session-Key) and the prompt body, so the
// re-entry assertion is exact — SAME session key, do-it-or-explain text.
type noop1682Gateway struct {
	mu       sync.Mutex
	prompts  []string
	sessions []string
}

func (g *noop1682Gateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.sessions = append(g.sessions, r.Header.Get("X-Hermes-Session-Key"))
		var body struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.prompts = append(g.prompts, body.Input)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1682",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "ok"},
					},
				},
			},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
		})
	}
}

func (g *noop1682Gateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.prompts)
}

func (g *noop1682Gateway) lastSession() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sessions) == 0 {
		return ""
	}
	return g.sessions[len(g.sessions)-1]
}

func (g *noop1682Gateway) lastPrompt() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.prompts) == 0 {
		return ""
	}
	return g.prompts[len(g.prompts)-1]
}

// noop1682Seed creates the lane row (and its parent when set) through
// the real CreateProject path. db is the test database (*sql.DB).
func noop1682Seed(t *testing.T, db *sql.DB, name, parent string, noopAllowed *bool) {
	t.Helper()
	ctx := context.Background()
	p := &database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/noop1682-" + name,
		Weight: 10, Priority: 5, CooldownS: 900,
		Parent: parent, Enabled: true,
		CreatedAt: "2026-10-10T00:00:00Z", UpdatedAt: "2026-10-10T00:00:00Z",
		NoopAllowed: noopAllowed,
	}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
}

func noop1682StoredFlag(t *testing.T, db *sql.DB, tickID string) int {
	t.Helper()
	var flag int
	if err := db.QueryRow(`SELECT noop_flag FROM ticks WHERE id = ?`, tickID).Scan(&flag); err != nil {
		t.Fatalf("read noop_flag for %s: %v", tickID, err)
	}
	return flag
}

func noop1682Events(t *testing.T, db *sql.DB, eventType string) int {
	t.Helper()
	var n int
	// Match the event_type key exactly — the details payload of EVERY
	// noop_guard event carries a "noop_allowed" boolean, so a bare
	// substring match would count across event types.
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE component = ? AND details LIKE ?`,
		NoopGuardEventComponent, `%"event_type":"`+eventType+`"%`).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

// TestSCHEDGAP1682_VerdictTable pins the verdict computation: unchanged
// measured pair → no-op; ANY moved leg or an unmeasured capture → not a
// no-op (an evidence gap must never re-enter a session).
func TestSCHEDGAP1682_VerdictTable(t *testing.T) {
	cases := []struct {
		name           string
		preC, preB     string
		postC, postB   string
		boardUnchanged bool
		want           bool
	}{
		{"unchanged + board unchanged", "aaa", "main", "aaa", "main", true, true},
		{"commit moved", "aaa", "main", "bbb", "main", true, false},
		{"branch moved", "aaa", "main", "aaa", "feature", true, false},
		{"board moved", "aaa", "main", "aaa", "main", false, false},
		{"unmeasured pre (no repo)", "", "", "bbb", "main", true, false},
		{"unmeasured pre, post empty too", "", "", "", "", true, false},
	}
	for _, tc := range cases {
		if got := noopVerdict(tc.preC, tc.preB, tc.postC, tc.postB, tc.boardUnchanged); got != tc.want {
			t.Errorf("%s: noopVerdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSCHEDGAP1682_OverrideResolution pins the two-direction override:
// nil derives from the class; an explicit value wins in BOTH directions.
func TestSCHEDGAP1682_OverrideResolution(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		name        string
		override    *bool
		class       string
		wantAllowed bool
	}{
		{"nil + satellite derives allowed", nil, LaneClassSatellite, true},
		{"nil + foreman derives forbidden", nil, LaneClassForeman, false},
		{"false on satellite overrides to forbidden", &f, LaneClassSatellite, false},
		{"true on foreman overrides to allowed", &tr, LaneClassForeman, true},
	}
	for _, tc := range cases {
		if got := resolveNoopAllowed(tc.override, tc.class); got != tc.wantAllowed {
			t.Errorf("%s: resolveNoopAllowed = %v, want %v", tc.name, got, tc.wantAllowed)
		}
	}
}

// TestSCHEDGAP1682_CaptureGitPair pins the capture: a real repo yields the
// sha + branch; an empty dir yields the honest unmeasured pair.
func TestSCHEDGAP1682_CaptureGitPair(t *testing.T) {
	dir := t.TempDir()
	noop1682InitRepo(t, dir)
	commit, branch := captureGitPair(dir)
	if commit == "" || branch == "" {
		t.Fatalf("capture on a real repo = (%q,%q), want both non-empty", commit, branch)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if commit != strings.TrimSpace(string(out)) {
		t.Errorf("capture commit %q != HEAD %q", commit, strings.TrimSpace(string(out)))
	}

	if c, b := captureGitPair(t.TempDir()); c != "" || b != "" {
		t.Errorf("capture on a non-repo = (%q,%q), want the unmeasured pair", c, b)
	}
	if c, b := captureGitPair(""); c != "" || b != "" {
		t.Errorf("capture on empty workdir = (%q,%q), want the unmeasured pair", c, b)
	}
}

// TestSCHEDGAP1682_ForemanNoopReentersSameSession is A2: a FOREMAN lane
// whose commit+branch+board are unchanged gets noop_flag=1 AND the gateway
// receives the do-it-or-explain instruction on the SAME session key.
func TestSCHEDGAP1682_ForemanNoopReentersSameSession(t *testing.T) {
	db := newTestDB(t)

	// A solo primary lane (no parent, no satellites) = foreman class.
	noop1682Seed(t, db, "foreman-lane", "", nil)

	gw := &noop1682Gateway{}
	srv := httptest.NewServer(gw.handler())
	defer srv.Close()

	s := NewSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 5*time.Second))
	s.SetEventLogger(NewEventLogger(db))
	s.SetNoExecFallback(true)

	// Real workdir with a real repo (the trigger reads it).
	workdir := t.TempDir()
	noop1682InitRepo(t, workdir)

	// Tick row exists (the verdict is an UPDATE).
	tickID := "foreman-lane-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'foreman-lane', 'running', datetime('now'))`, tickID); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	proj := PackedProject{Name: "foreman-lane", Workdir: workdir}

	// BEFORE capture.
	preC, preB, preBoard := s.recordNoopPreCapture(tickID, workdir)
	if preC == "" || preB == "" {
		t.Fatalf("pre capture unmeasured: (%q,%q)", preC, preB)
	}

	// The tick runs and does NOTHING: post == pre on both legs, board unmoved.
	s.closeNoopVerdict(s.gateway, context.Background(), proj, tickID,
		preC, preB, preBoard, "test-model", "test-provider", "test-key")

	// A2 verdict: flagged.
	if flag := noop1682StoredFlag(t, db, tickID); flag != 1 {
		t.Fatalf("noop_flag = %d, want 1 (foreman no-op must be flagged)", flag)
	}
	// A2 re-entry: the gateway received the instruction on the SAME session.
	// (The tick's own dispatch is driven directly through closeNoopVerdict
	// here, so the ONLY POST this gateway sees is the re-entry turn —
	// count==1, not 2.)
	waitFor1682(t, 2*time.Second, "re-entry POST", func() bool { return gw.count() == 1 })
	if got := gw.lastPrompt(); !strings.Contains(got, "You did no work this tick") ||
		!strings.Contains(got, "explaining exactly why you did nothing") {
		t.Errorf("re-entry prompt missing the instruction: %q", got)
	}
	if got := gw.lastSession(); got != tickID {
		t.Errorf("re-entry session key = %q, want the tick id %q (SAME session)", got, tickID)
	}
	// The events fired in order.
	if n := noop1682Events(t, db, "noop_detected"); n != 1 {
		t.Errorf("noop_detected events = %d, want 1", n)
	}
	if n := noop1682Events(t, db, "reentry_requested"); n != 1 {
		t.Errorf("reentry_requested events = %d, want 1", n)
	}
	// The stored post capture is on the row (auditable evidence).
	var postC string
	if err := db.QueryRow(`SELECT post_commit FROM ticks WHERE id = ?`, tickID).Scan(&postC); err != nil {
		t.Fatalf("read post capture: %v", err)
	}
	if postC != preC {
		t.Errorf("stored post_commit %q != pre %q", postC, preC)
	}
}

// TestSCHEDGAP1682_SatelliteNoopClosesClean is A3: a satellite lane with
// unchanged state → noop_flag=0, no re-entry POST, one noop_allowed event.
func TestSCHEDGAP1682_SatelliteNoopClosesClean(t *testing.T) {
	db := newTestDB(t)

	// Parent + satellite: the satellite is satellite-class by suffix.
	noop1682Seed(t, db, "sat-parent", "", nil)
	noop1682Seed(t, db, "sat-parent-sync", "sat-parent", nil)

	gw := &noop1682Gateway{}
	srv := httptest.NewServer(gw.handler())
	defer srv.Close()

	s := NewSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 5*time.Second))
	s.SetEventLogger(NewEventLogger(db))

	workdir := t.TempDir()
	noop1682InitRepo(t, workdir)
	tickID := "sat-parent-sync-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'sat-parent-sync', 'running', datetime('now'))`, tickID); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	preC, preB, preBoard := s.recordNoopPreCapture(tickID, workdir)
	s.closeNoopVerdict(s.gateway, context.Background(),
		PackedProject{Name: "sat-parent-sync", Workdir: workdir}, tickID,
		preC, preB, preBoard, "test-model", "test-provider", "test-key")

	if flag := noop1682StoredFlag(t, db, tickID); flag != 0 {
		t.Fatalf("noop_flag = %d, want 0 (satellite no-op closes clean)", flag)
	}
	if n := noop1682Events(t, db, "noop_allowed"); n != 1 {
		t.Errorf("noop_allowed events = %d, want 1", n)
	}
	if n := noop1682Events(t, db, "noop_detected"); n != 0 {
		t.Errorf("noop_detected events = %d, want 0", n)
	}
	// No re-entry: the gateway saw exactly ZERO POSTs (any POST here would
	// be the re-entry).
	if n := gw.count(); n != 0 {
		t.Errorf("gateway POSTs = %d, want 0 (no re-entry on a satellite)", n)
	}
}

// TestSCHEDGAP1682_OverrideFlipsSatelliteToReentry is A4: the explicit
// noop_allowed=false override on a SATELLITE flips the verdict to
// enforcement (flag=1 + re-entry), and true on a FOREMAN closes clean.
func TestSCHEDGAP1682_OverrideFlipsSatelliteToReentry(t *testing.T) {
	db := newTestDB(t)

	noop1682Seed(t, db, "ov-parent", "", nil)
	f := false
	noop1682Seed(t, db, "ov-parent-pm", "ov-parent", &f)

	gw := &noop1682Gateway{}
	srv := httptest.NewServer(gw.handler())
	defer srv.Close()

	s := NewSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 5*time.Second))
	s.SetEventLogger(NewEventLogger(db))

	workdir := t.TempDir()
	noop1682InitRepo(t, workdir)
	tickID := "ov-parent-pm-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'ov-parent-pm', 'running', datetime('now'))`, tickID); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	preC, preB, preBoard := s.recordNoopPreCapture(tickID, workdir)
	s.closeNoopVerdict(s.gateway, context.Background(),
		PackedProject{Name: "ov-parent-pm", Workdir: workdir}, tickID,
		preC, preB, preBoard, "test-model", "test-provider", "test-key")

	if flag := noop1682StoredFlag(t, db, tickID); flag != 1 {
		t.Fatalf("noop_flag = %d, want 1 (override false flips the satellite to enforcement)", flag)
	}
	waitFor1682(t, 2*time.Second, "re-entry POST", func() bool { return gw.count() == 1 })

	// The mirror arm: true on a foreman closes clean.
	tr := true
	noop1682Seed(t, db, "ov-foreman", "", &tr)
	tickID2 := "ov-foreman-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'ov-foreman', 'running', datetime('now'))`, tickID2); err != nil {
		t.Fatalf("seed tick 2: %v", err)
	}
	preC2, preB2, preBoard2 := s.recordNoopPreCapture(tickID2, workdir)
	s.closeNoopVerdict(s.gateway, context.Background(),
		PackedProject{Name: "ov-foreman", Workdir: workdir}, tickID2,
		preC2, preB2, preBoard2, "test-model", "test-provider", "test-key")

	if flag := noop1682StoredFlag(t, db, tickID2); flag != 0 {
		t.Fatalf("noop_flag = %d, want 0 (override true closes a foreman clean)", flag)
	}
	if n := noop1682Events(t, db, "noop_allowed"); n != 1 {
		t.Errorf("noop_allowed events = %d, want 1 (the foreman-with-true arm)", n)
	}
}

// TestSCHEDGAP1682_WorkedTickNeverFlagged proves the negative arm end to
// end: a foreman whose repo gained a commit between the captures is NOT a
// no-op — flag 0, no re-entry, no enforcement events.
func TestSCHEDGAP1682_WorkedTickNeverFlagged(t *testing.T) {
	db := newTestDB(t)
	noop1682Seed(t, db, "worked-lane", "", nil)

	gw := &noop1682Gateway{}
	srv := httptest.NewServer(gw.handler())
	defer srv.Close()
	s := NewSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 5*time.Second))
	s.SetEventLogger(NewEventLogger(db))

	workdir := t.TempDir()
	board := noop1682InitRepo(t, workdir)
	tickID := "worked-lane-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'worked-lane', 'running', datetime('now'))`, tickID); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	preC, preB, preBoard := s.recordNoopPreCapture(tickID, workdir)

	// The foreman "works": a commit + a board write.
	if out, err := exec.Command("git", "-C", workdir, "commit", "-q", "--allow-empty", "-m", "real work").CombinedOutput(); err != nil {
		t.Fatalf("work commit: %v\n%s", err, out)
	}
	if err := os.WriteFile(board, []byte("{\"id\":\"R1\",\"status\":\"pending\"}\n{\"id\":\"R2\",\"status\":\"done\"}\n"), 0o600); err != nil {
		t.Fatalf("board write: %v", err)
	}

	s.closeNoopVerdict(s.gateway, context.Background(),
		PackedProject{Name: "worked-lane", Workdir: workdir}, tickID,
		preC, preB, preBoard, "test-model", "test-provider", "test-key")

	if flag := noop1682StoredFlag(t, db, tickID); flag != 0 {
		t.Fatalf("noop_flag = %d, want 0 (a worked tick is never a no-op)", flag)
	}
	if n := gw.count(); n != 0 {
		t.Errorf("gateway POSTs = %d, want 0 (no re-entry on real work)", n)
	}
	if n := noop1682Events(t, db, "noop_detected"); n != 0 {
		t.Errorf("noop_detected events = %d, want 0", n)
	}
}

// TestSCHEDGAP1682_ReentryUnavailableEmitsEvent pins the no-channel arm:
// a nil gateway client degrades to the noop_reentry_unavailable event —
// the miss is never fabricated into a delivery.
func TestSCHEDGAP1682_ReentryUnavailableEmitsEvent(t *testing.T) {
	db := newTestDB(t)
	noop1682Seed(t, db, "chan-lane", "", nil)

	s := NewSpawner(db, 4) // no gateway client: exec-fallback shape
	s.SetEventLogger(NewEventLogger(db))
	workdir := t.TempDir()
	noop1682InitRepo(t, workdir)
	tickID := "chan-lane-1682-t1"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'chan-lane', 'running', datetime('now'))`, tickID); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	preC, preB, preBoard := s.recordNoopPreCapture(tickID, workdir)
	s.closeNoopVerdict(nil, nil,
		PackedProject{Name: "chan-lane", Workdir: workdir}, tickID,
		preC, preB, preBoard, "test-model", "test-provider", "")

	if flag := noop1682StoredFlag(t, db, tickID); flag != 1 {
		t.Fatalf("noop_flag = %d, want 1 (the verdict is flagged even without a channel)", flag)
	}
	if n := noop1682Events(t, db, "noop_reentry_unavailable"); n != 1 {
		t.Errorf("noop_reentry_unavailable events = %d, want 1", n)
	}
	if n := noop1682Events(t, db, "reentry_requested"); n != 0 {
		t.Errorf("reentry_requested events = %d, want 0 (never fabricated)", n)
	}
}

// waitFor1682 polls cond until it holds or the timeout elapses.
func waitFor1682(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}
