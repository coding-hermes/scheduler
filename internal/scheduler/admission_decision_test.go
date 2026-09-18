package scheduler

// SCHED-GAP-155 — regression tests for the structured admission-decision log
// and its counters.
//
// The defect these pin: an operator could see that a pass selected nothing
// (EVAL-ZERO-SELECT) but never WHY an individual project was passed over —
// answering "why did project X not spawn in window Y" required reading the
// packer's aggregate lines, SQLite and the cooldown policy script side by
// side. evaluate() now emits one grep-stable `ADMIT ` line per candidate
// project and folds each reason into per-process counters surfaced on
// /api/v1/status as admission_counters.
//
// Every subtest drives the REAL evaluate() with a pinned clock seam
// (fixedEvalNow, ADV-R04/G6) in simulation mode, so the selection — and
// therefore the reason — is deterministic at one constructed instant with no
// sleeps and no wall-clock bounds.

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// admitLogBuf is a mutex-guarded capture of log output: evaluate() may log
// from the sim-spawn goroutines too, and the assertions only ever look at
// lines whose project field is one of this test's fixtures.
type admitLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *admitLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines returns every emitted line carrying the grep-stable ADMIT prefix.
func (l *admitLogBuf) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range strings.Split(l.b.String(), "\n") {
		if strings.HasPrefix(ln, "ADMIT ") {
			out = append(out, ln)
		}
	}
	return out
}

// projectLines returns the ADMIT lines for one project (the line-level
// contract is one line per project per pass).
func (l *admitLogBuf) projectLines(project string) []string {
	var out []string
	for _, ln := range l.lines() {
		if admitField(ln, "project") == project {
			out = append(out, ln)
		}
	}
	return out
}

// admitCaptureLog redirects the standard logger into a capture buffer and
// restores the previous writer on cleanup.
func admitCaptureLog(t *testing.T) *admitLogBuf {
	t.Helper()
	buf := &admitLogBuf{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// admitField extracts a key=value field from an ADMIT line ("" when absent).
func admitField(line, key string) string {
	prefix := key + "="
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return ""
}

// admitInt parses an integer field; it fails the test when absent/invalid.
func admitInt(t *testing.T, line, key string) int {
	t.Helper()
	raw := admitField(line, key)
	if raw == "" {
		t.Fatalf("ADMIT line missing %s=: %s", key, line)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("ADMIT %s=%q is not an integer: %s", key, raw, line)
	}
	return n
}

// admitFloat parses a float field ("" when absent).
func admitFloat(t *testing.T, line, key string) (float64, bool) {
	t.Helper()
	raw := admitField(line, key)
	if raw == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("ADMIT %s=%q is not a float: %s", key, raw, line)
	}
	return f, true
}

// admitAssertLineShape enforces the shared line contract: grep-stable
// prefix, both mandatory fields, a vocabulary reason, and the ~250-char
// budget.
func admitAssertLineShape(t *testing.T, line string) {
	t.Helper()
	if !strings.HasPrefix(line, "ADMIT ") {
		t.Fatalf("line does not start with the grep-stable 'ADMIT ' prefix: %q", line)
	}
	if admitField(line, "project") == "" {
		t.Fatalf("ADMIT line has no project= field: %q", line)
	}
	reason := admitField(line, "reason")
	if reason == "" {
		t.Fatalf("ADMIT line has no reason= field: %q", line)
	}
	if !admissionReasonIsKnown(reason) {
		t.Fatalf("ADMIT reason=%q is outside the vocabulary %v", reason, admissionReasonVocabulary)
	}
	if len(line) > 250 {
		t.Fatalf("ADMIT line is %d chars (budget ~250): %q", len(line), line)
	}
}

// admitProjectSpec is one fixture project for the admission tests.
type admitProjectSpec struct {
	Name           string
	NS             string // "" = no namespace
	Weight         int    // 0 = 10
	CooldownS      int
	Last           time.Time // zero = never completed
	Failures       int
	AdmissionMode  string // "" = inherit
	BoardOwnership string // "" = auto
	Workdir        string // "" = /tmp/<name> (no board on disk)
}

// admitInsertProject seeds one enabled project row.
func admitInsertProject(t *testing.T, db *sql.DB, s admitProjectSpec) {
	t.Helper()
	weight := s.Weight
	if weight == 0 {
		weight = 10
	}
	workdir := s.Workdir
	if workdir == "" {
		workdir = filepath.Join("/tmp", s.Name)
	}
	var ns any
	if s.NS != "" {
		ns = s.NS
	}
	var last any
	if !s.Last.IsZero() {
		last = s.Last.UTC().Format(time.RFC3339)
	}
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, namespace_id, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at, last_tick_completed,
		 consecutive_failures, admission_mode, board_ownership)
		VALUES (?, ?, ?, ?, ?, 5, ?, 1.0, 'm', 'p', 1, datetime('now'), datetime('now'), ?, ?, ?, ?)`,
		s.Name, "https://example.com/"+s.Name, workdir, ns, weight, s.CooldownS,
		last, s.Failures, s.AdmissionMode, s.BoardOwnership); err != nil {
		t.Fatalf("insert project %s: %v", s.Name, err)
	}
}

// admitInsertNamespace seeds one namespace row (cap 0 = unlimited).
func admitInsertNamespace(t *testing.T, db *sql.DB, id string, maxConcurrent int, mode string) {
	t.Helper()
	if mode == "" {
		mode = "cooldown"
	}
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, max_concurrent, admission_mode)
		VALUES (?, 10, ?, ?)`, id, maxConcurrent, mode); err != nil {
		t.Fatalf("insert namespace %s: %v", id, err)
	}
}

// admitInsertRunningTick seeds a DB tick row in 'running' — how a live tick
// (or a restarted daemon's survivor) occupies a namespace.
// admitInsertRunningTick seeds one tick row in status='running'.
//
// The row is stamped at the PINNED decision instant (fixedEvalNow), never the
// wall clock: every loop in this file runs on that pinned clock, and the
// stale-tick cleanup in the evaluate path (Loop -> lifecycle.CleanupStale,
// SCHED-GAP-169 routed through the loop's clock) reaps running rows older than
// 90 minutes RELATIVE TO THE LOOP'S CLOCK. A wall-clock stamp would be years
// "old" on a 2031-pinned loop and the row would be reaped before evaluation —
// a fixture that mixes two timelines, not a behavior under test.
func admitInsertRunningTick(t *testing.T, db *sql.DB, tickID, project string) {
	t.Helper()
	now := fixedEvalNow().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES (?, ?, 'running', ?, ?)`, tickID, project, now, now); err != nil {
		t.Fatalf("insert running tick %s: %v", tickID, err)
	}
}

// admitWorkdirWithBoard creates a temp workdir holding a board file with the
// given JSONL rows. An empty body is the "lane owns its board, no work
// left" shape the tasks_no_work reason describes.
func admitWorkdirWithBoard(t *testing.T, rows ...string) string {
	t.Helper()
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	body := ""
	if len(rows) > 0 {
		body = strings.Join(rows, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write board file: %v", err)
	}
	return wd
}

// admitNewLoop builds the loop every subtest drives: simulation mode (the
// real spawner is never touched) + the pinned clock seam.
func admitNewLoop(t *testing.T, db *sql.DB, now time.Time) *Loop {
	t.Helper()
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(now))
	return l
}

// TestAdmissionDecision drives one real evaluation pass per subtest and
// asserts the ADMIT line + counter for the deferral reason under test.
func TestAdmissionDecision(t *testing.T) {
	now := fixedEvalNow()

	t.Run("T-ADMIT-1 admitted project emits one line with reason=ok", func(t *testing.T) {
		db := newTestDB(t)
		admitInsertNamespace(t, db, "qa", 0, "cooldown")
		// cooldown 60s, completed 90s ago at the pinned instant → eligible.
		admitInsertProject(t, db, admitProjectSpec{
			Name: "admit-ok", NS: "qa", CooldownS: 60, Last: now.Add(-90 * time.Second),
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("admit-ok")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for admit-ok = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)

		if got := admitField(line, "reason"); got != AdmissionReasonOK {
			t.Errorf("reason = %q, want %q: %s", got, AdmissionReasonOK, line)
		}
		// The namespace at ADMIT time, not a global.
		if got := admitField(line, "ns"); got != "qa" {
			t.Errorf("ns = %q, want qa: %s", got, line)
		}
		for key, want := range map[string]int{
			"cap": 0, "inflight_running": 0, "inflight_queued": 0,
			"eligible": 1, "admitted": 1, "deferred": 0,
		} {
			if got := admitInt(t, line, key); got != want {
				t.Errorf("%s = %d, want %d: %s", key, got, want, line)
			}
		}
		// The pass actually spawned the simulated tick.
		if got := simSelectedProjects(t, db); len(got) != 1 || got[0] != "admit-ok" {
			t.Errorf("simulated selection = %v, want [admit-ok]", got)
		}

		counters := l.AdmissionCounters()
		if counters[AdmissionReasonOK] != 1 {
			t.Errorf("admission_counters[ok] = %d, want 1 (%v)", counters[AdmissionReasonOK], counters)
		}
		if counters["admitted:qa"] != 1 {
			t.Errorf("admission_counters[admitted:qa] = %d, want 1 (%v)", counters["admitted:qa"], counters)
		}
		if counters["passes"] != 1 {
			t.Errorf("admission_counters[passes] = %d, want 1", counters["passes"])
		}
	})

	t.Run("T-ADMIT-2 cooldown deferral carries cooldown_remaining_s", func(t *testing.T) {
		db := newTestDB(t)
		// cooldown 3600s, completed 600s ago → 3000s of pin left.
		admitInsertProject(t, db, admitProjectSpec{
			Name: "admit-cool", CooldownS: 3600, Last: now.Add(-600 * time.Second),
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("admit-cool")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for admit-cool = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)

		if got := admitField(line, "reason"); got != AdmissionReasonCooldown {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonCooldown, line)
		}
		rem, ok := admitFloat(t, line, "cooldown_remaining_s")
		if !ok {
			t.Fatalf("reason=cooldown line has no cooldown_remaining_s: %s", line)
		}
		if rem <= 0 || rem < 2900 || rem > 3100 {
			t.Errorf("cooldown_remaining_s = %v, want ~3000 (>0): %s", rem, line)
		}
		if got := admitInt(t, line, "admitted"); got != 0 {
			t.Errorf("admitted = %d, want 0: %s", got, line)
		}
		if got := admitInt(t, line, "deferred"); got != 1 {
			t.Errorf("deferred = %d, want 1: %s", got, line)
		}
		// A deferral must not spawn: no tick row of any kind.
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("simulated selection = %v, want none", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonCooldown] != 1 {
			t.Errorf("admission_counters[cooldown] = %d, want 1 (%v)", counters[AdmissionReasonCooldown], counters)
		}
	})

	t.Run("T-ADMIT-3 namespace cap deferral names the namespace and its occupancy", func(t *testing.T) {
		db := newTestDB(t)
		admitInsertNamespace(t, db, "qacap", 1, "cooldown")
		admitInsertProject(t, db, admitProjectSpec{Name: "cap-run", NS: "qacap", CooldownS: 60, Last: now.Add(-90 * time.Second)})
		admitInsertProject(t, db, admitProjectSpec{Name: "cap-wait", NS: "qacap"})
		// cap-run holds the namespace's single slot.
		admitInsertRunningTick(t, db, "tick-cap-run", "cap-run")
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		// The project with a tick in flight is not a candidate: no line.
		if lines := cap.projectLines("cap-run"); len(lines) != 0 {
			t.Errorf("running project must not get an ADMIT line, got %v", lines)
		}
		lines := cap.projectLines("cap-wait")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for cap-wait = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)

		if got := admitField(line, "reason"); got != AdmissionReasonCap {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonCap, line)
		}
		if got := admitField(line, "ns"); got != "qacap" {
			t.Errorf("ns = %q, want qacap: %s", got, line)
		}
		if got := admitInt(t, line, "cap"); got != 1 {
			t.Errorf("cap = %d, want 1: %s", got, line)
		}
		if got := admitInt(t, line, "inflight_running"); got != 1 {
			t.Errorf("inflight_running = %d, want 1: %s", got, line)
		}
		if got := admitInt(t, line, "admitted"); got != 0 {
			t.Errorf("admitted = %d, want 0: %s", got, line)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonCap] != 1 {
			t.Errorf("admission_counters[cap] = %d, want 1 (%v)", counters[AdmissionReasonCap], counters)
		}
	})

	t.Run("T-ADMIT-4 tasks lane with an owned but empty board reports tasks_no_work", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t) // board exists, zero rows → no work
		admitInsertProject(t, db, admitProjectSpec{
			Name: "tasks-idle", CooldownS: 3600, Last: now.Add(-600 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("tasks-idle")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for tasks-idle = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)
		if got := admitField(line, "reason"); got != AdmissionReasonTasksNoWork {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonTasksNoWork, line)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonTasksNoWork] != 1 {
			t.Errorf("admission_counters[tasks_no_work] = %d, want 1 (%v)",
				counters[AdmissionReasonTasksNoWork], counters)
		}
	})

	t.Run("T-ADMIT-5 tasks lane without its own board reports board_unowned", func(t *testing.T) {
		db := newTestDB(t)
		// TempDir with no .coding-hermes/ board at all → ownership refused.
		wd := t.TempDir()
		admitInsertProject(t, db, admitProjectSpec{
			Name: "tasks-shared", CooldownS: 3600, Last: now.Add(-600 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("tasks-shared")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for tasks-shared = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)
		if got := admitField(line, "reason"); got != AdmissionReasonBoardUnowned {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonBoardUnowned, line)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonBoardUnowned] != 1 {
			t.Errorf("admission_counters[board_unowned] = %d, want 1 (%v)",
				counters[AdmissionReasonBoardUnowned], counters)
		}
	})

	t.Run("T-ADMIT-6 tasks lane with work inside failure backoff reports tasks_deferred", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t, `{"id":"ADMIT-1","status":"pending","title":"work"}`)
		// consecutive_failures=2 → FailureBackoff(60s,2)=120s; last tick was
		// 100s ago, so the SCHED-GAP-133 backoff still defers despite work.
		admitInsertProject(t, db, admitProjectSpec{
			Name: "tasks-backoff", CooldownS: 60, Failures: 2, Last: now.Add(-100 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("tasks-backoff")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for tasks-backoff = %d, want exactly 1: %v", len(lines), lines)
		}
		line := lines[0]
		admitAssertLineShape(t, line)
		if got := admitField(line, "reason"); got != AdmissionReasonTasksDeferred {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonTasksDeferred, line)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonTasksDeferred] != 1 {
			t.Errorf("admission_counters[tasks_deferred] = %d, want 1 (%v)",
				counters[AdmissionReasonTasksDeferred], counters)
		}
	})

	t.Run("T-ADMIT-7 counters expose the whole vocabulary at boot", func(t *testing.T) {
		// A loop that has never evaluated must still answer with every
		// reason key at zero — "no deferrals" and "counter missing" are
		// different states for the operator.
		l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
		counters := l.AdmissionCounters()
		for _, reason := range admissionReasonVocabulary {
			v, ok := counters[reason]
			if !ok {
				t.Errorf("admission_counters missing vocabulary key %q: %v", reason, counters)
				continue
			}
			if v != 0 {
				t.Errorf("admission_counters[%q] = %d on a boot-fresh loop, want 0", reason, v)
			}
		}
		if got := counters["passes"]; got != 0 {
			t.Errorf("admission_counters[passes] = %d on a boot-fresh loop, want 0", got)
		}
		if len(counters) != len(admissionReasonVocabulary)+1 {
			t.Errorf("admission_counters has %d keys, want %d (vocabulary + passes): %v",
				len(counters), len(admissionReasonVocabulary)+1, counters)
		}
	})
}

// TestADMIT_LineShape drives the emitter directly (the brief's allowed
// helper-level test): it pins the exact key=value shape, the field
// sanitization, the cooldown suffix, and the counter contract for a line
// carrying a reason outside the vocabulary.
func TestADMIT_LineShape(t *testing.T) {
	l := &Loop{} // zero-value Loop: the counter maps lazily initialize
	cap := admitCaptureLog(t)

	l.emitAdmissionDecision(7, 3, 1, 2, admissionDecision{
		Project: "my proj=x", NS: "qa ns", Reason: AdmissionReasonCap,
		Cap: 1, InflightRunning: 1, InflightQueued: 0,
	})

	lines := cap.lines()
	if len(lines) != 1 {
		t.Fatalf("emitted %d ADMIT lines, want 1: %v", len(lines), lines)
	}
	line := lines[0]
	admitAssertLineShape(t, line)
	for key, want := range map[string]string{
		"pass_id": "7", "eligible": "3", "admitted": "1", "deferred": "2",
		"ns": "qa_ns", "cap": "1", "inflight_running": "1", "inflight_queued": "0",
		"project": "my_proj_x", "reason": "cap",
	} {
		if got := admitField(line, key); got != want {
			t.Errorf("%s = %q, want %q: %s", key, got, want, line)
		}
	}
	if _, ok := admitFloat(t, line, "cooldown_remaining_s"); ok {
		t.Errorf("cooldown_remaining_s must be absent unless reason=cooldown: %s", line)
	}
	counters := l.AdmissionCounters()
	if counters[AdmissionReasonCap] != 1 {
		t.Errorf("admission_counters[cap] = %d, want 1 (%v)", counters[AdmissionReasonCap], counters)
	}
	if counters[AdmissionReasonOK] != 0 {
		t.Errorf("admission_counters[ok] = %d, want 0 (%v)", counters[AdmissionReasonOK], counters)
	}
	if counters["passes"] != 0 {
		t.Errorf("admission_counters[passes] = %d, want 0 (only a pass counts a pass)", counters["passes"])
	}

	// A cooldown line carries the countdown suffix.
	l.emitAdmissionDecision(7, 3, 1, 2, admissionDecision{
		Project: "cool", Reason: AdmissionReasonCooldown,
		CooldownRemainingS: 12.5, HasCooldownRem: true,
	})
	lines = cap.projectLines("cool")
	if len(lines) != 1 {
		t.Fatalf("emitted %d ADMIT lines for cool, want 1: %v", len(lines), lines)
	}
	if got, ok := admitFloat(t, lines[0], "cooldown_remaining_s"); !ok || got != 12.5 {
		t.Errorf("cooldown_remaining_s = %v (present=%v), want 12.5: %s", got, ok, lines[0])
	}
	if got := l.AdmissionCounters()[AdmissionReasonCooldown]; got != 1 {
		t.Errorf("admission_counters[cooldown] = %d, want 1", got)
	}

	// A reason outside the vocabulary is emitted verbatim but never counted:
	// a classification bug must stay visible instead of inflating a tick.
	before := l.AdmissionCounters()
	l.emitAdmissionDecision(7, 3, 1, 2, admissionDecision{Project: "weird", Reason: "bogus"})
	if got := admitField(cap.projectLines("weird")[0], "reason"); got != "bogus" {
		t.Errorf("unknown reason not emitted verbatim: got %q", got)
	}
	after := l.AdmissionCounters()
	if len(after) != len(before) {
		t.Errorf("unknown reason created a counter key: before=%v after=%v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("unknown reason changed counter %q: %d → %d", k, v, after[k])
		}
	}
}
