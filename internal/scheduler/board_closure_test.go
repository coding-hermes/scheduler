package scheduler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── SCHED-GAP-085: board closure-evidence gate ────────────────────────────
//
// A tasks.jsonl row is CLOSED only when status=="complete" AND
// worker_status=="complete" AND completed_at is non-empty. A violation is a
// closed row where ALL of reasoning/commit_hash/worker_summary are
// empty/whitespace. Rows that are pending, or status=complete with
// worker_status!=complete or completed_at null (legacy/perpetual fixtures),
// are NOT closures and must be ignored. Malformed lines are skipped, never
// crash.

// writeClosureBoard writes a JSONL board file (id field first for readability;
// field order does not matter to the parser) into a temp dir and returns the
// tasks.jsonl path.
func writeClosureBoard(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.jsonl")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestBoardClosureViolations_ClosedWithEvidencePasses(t *testing.T) {
	path := writeClosureBoard(t,
		`{"id":"T1","status":"complete","worker_status":"complete","completed_at":"2026-08-30 10:00:00","reasoning":"fixed it","commit_hash":"abc123","worker_summary":"done"}`,
		`{"id":"T2","status":"complete","worker_status":"complete","completed_at":"2026-08-30T11:00:00Z","reasoning":"","commit_hash":"def456","worker_summary":"ship"}`,
	)
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %d, want 0 (rows with any evidence must pass): %+v", len(violations), violations)
	}
}

func TestBoardClosureViolations_ClosedWithoutEvidenceFlagged(t *testing.T) {
	path := writeClosureBoard(t,
		`{"id":"BAD-1","status":"complete","worker_status":"complete","completed_at":"2026-08-30 10:00:00","reasoning":null,"commit_hash":null,"worker_summary":null}`,
		`{"id":"GOOD","status":"complete","worker_status":"complete","completed_at":"2026-08-30 10:00:00","reasoning":"x","commit_hash":null,"worker_summary":null}`,
		`{"id":"BAD-2","status":"complete","worker_status":"complete","completed_at":"2026-08-30 10:00:00","reasoning":"  ","commit_hash":"","worker_summary":"   "}`,
	)
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %d, want 2 (BAD-1 + BAD-2): %+v", len(violations), violations)
	}
	for _, v := range violations {
		if len(v.MissingFields) != 3 {
			t.Errorf("%s missing fields = %v, want all three [reasoning commit_hash worker_summary]", v.ID, v.MissingFields)
		}
	}
	ids := violations[0].ID + "," + violations[1].ID
	if !strings.Contains(ids, "BAD-1") || !strings.Contains(ids, "BAD-2") {
		t.Errorf("violation ids = %q, want BAD-1 and BAD-2", ids)
	}
}

func TestBoardClosureViolations_PendingIgnored(t *testing.T) {
	path := writeClosureBoard(t,
		`{"id":"PENDING","status":"pending","worker_status":"pending","completed_at":null,"reasoning":null,"commit_hash":null,"worker_summary":null}`,
	)
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %d, want 0 (pending rows are not closures)", len(violations))
	}
}

func TestBoardClosureViolations_StatusCompleteWorkerPendingIgnored(t *testing.T) {
	// The legacy/perpetual shape: status=complete but worker_status=pending
	// (and/or completed_at null) — e.g. AUDIT-DESCENDANT-LIFECYCLE,
	// GITREINS-JUDGE, GUARD-*, INFRA-005/007/009/010/011. NOT closures.
	path := writeClosureBoard(t,
		`{"id":"LEGACY-1","status":"complete","worker_status":"pending","completed_at":"2026-08-01 00:58:00","reasoning":null,"commit_hash":null,"worker_summary":null}`,
		`{"id":"LEGACY-2","status":"complete","worker_status":"pending","completed_at":null,"reasoning":null,"commit_hash":null,"worker_summary":null}`,
		`{"id":"LEGACY-3","status":"complete","worker_status":"complete","completed_at":null,"reasoning":null,"commit_hash":null,"worker_summary":null}`,
	)
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %d, want 0 (legacy/perpetual rows are not closures): %+v", len(violations), violations)
	}
}

func TestBoardClosureViolations_MalformedLineSkipped(t *testing.T) {
	path := writeClosureBoard(t,
		`{"id":"OK","status":"complete","worker_status":"complete","completed_at":"2026-08-30 10:00:00","reasoning":"x","commit_hash":"y","worker_summary":"z"}`,
		`this is not json at all`,
		`{"id":`,
		``,
	)
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %d, want 0 (malformed lines skipped, valid rows unaffected)", len(violations))
	}
}

func TestBoardClosureViolations_NonexistentPathErrors(t *testing.T) {
	_, err := BoardClosureViolations(filepath.Join(t.TempDir(), "missing.jsonl"))
	if err == nil {
		t.Fatal("expected error for nonexistent board path")
	}
}

func TestBoardClosureViolations_NonJSONLNoViolations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("## [ ] task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	violations, err := BoardClosureViolations(path)
	if err != nil {
		t.Fatalf("BoardClosureViolations: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %d, want 0 for non-JSONL boards", len(violations))
	}
}

func TestParseBoardCompletedAt(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in       string
		wantOK   bool
		wantTime time.Time // zero = only check ok
	}{
		{"2026-08-30 12:00:00", true, now},
		{"2026-08-30 12:00:00.000000", true, now},
		{"2026-08-30T12:00:00Z", true, now},
		{"2026-08-30T12:00:00+00:00", true, now},
		{"2026-08-14T04:55:29Z", true, time.Date(2026, 8, 14, 4, 55, 29, 0, time.UTC)},
		{"", false, time.Time{}},
		{"  ", false, time.Time{}},
		{"not-a-date", false, time.Time{}},
	}
	for _, c := range cases {
		got, ok := parseBoardCompletedAt(c.in)
		if ok != c.wantOK {
			t.Errorf("parseBoardCompletedAt(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && !got.Equal(c.wantTime) {
			t.Errorf("parseBoardCompletedAt(%q) = %v, want %v (naive stamps are UTC)", c.in, got, c.wantTime)
		}
	}
}

// ── Wait()-level gate tests (SCHED-GAP-085) ────────────────────────────────
//
// Modeled on schedgap079_test.go: a gateway spawn whose handler writes a
// fixture board into the project workdir before responding. Wait() then runs
// the closure gate on the board.

// closureBoardHandler writes the given board lines into workdir/.coding-hermes/
// board/tasks.jsonl (creating the dir), then responds as a normal completed
// gateway tick with a real session id + output text. Lines may contain the
// placeholder "{{NOW}}" which is replaced with the current UTC stamp AT
// REQUEST TIME — i.e. inside the tick window [reqStart, completeAt] — so a
// test can build a row closed by the tick itself.
func closureBoardHandler(t *testing.T, workdir string, lines ...string) http.HandlerFunc {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", boardDir, err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	return func(w http.ResponseWriter, r *http.Request) {
		content := strings.Join(lines, "\n")
		if content != "" {
			content += "\n"
		}
		content = strings.ReplaceAll(content, "{{NOW}}",
			time.Now().UTC().Format("2006-01-02 15:04:05.000000"))
		if err := os.WriteFile(boardPath, []byte(content), 0o644); err != nil {
			t.Errorf("WriteFile %s: %v", boardPath, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_closure",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "tick work done"},
					},
				},
			},
			"usage": map[string]int{},
		})
	}
}

// boardClosureEventCount counts HIGH board_closure events for a project+tick.
func boardClosureEventCount(t *testing.T, db *sql.DB, project, tickID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='board_closure' AND json_extract(details, '$.project') = ? AND json_extract(details, '$.tick_id') = ?`, project, tickID).Scan(&n); err != nil {
		t.Fatalf("count board_closure events: %v", err)
	}
	return n
}

// TestSCHEDGAP085_ClosedInWindowWithoutEvidenceRejectsTick — a board row
// closed WITHIN the tick's window (completed_at inside [reqStart, completeAt])
// but carrying no reasoning/commit_hash/worker_summary must reject the tick:
// Wait() yields TickFailed so lifecycle.Complete records status=failed /
// outcome=failed — never completed/committed.
func TestSCHEDGAP085_ClosedInWindowWithoutEvidenceRejectsTick(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()

	// completed_at = {{NOW}}: the handler stamps it at REQUEST time, inside
	// the tick window [reqStart, completeAt] — the row was closed by this tick.
	boardLine := `{"id":"SCHED-GAP-085-REJECT","status":"complete","worker_status":"complete","completed_at":"{{NOW}}","reasoning":null,"commit_hash":null,"worker_summary":null}`
	spawner := schedGap079Spawner(t, db, closureBoardHandler(t, workdir, boardLine))
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap085-reject", Workdir: workdir}
	tick, err := spawner.Spawn(project, "gap085-reject-2026-08-30-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s — a row closed in-window without evidence must reject the tick", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "SCHED-GAP-085-REJECT") {
		t.Errorf("outcome.Error = %q, want it to name the violating row", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "reasoning") || !strings.Contains(outcome.Error, "commit_hash") || !strings.Contains(outcome.Error, "worker_summary") {
		t.Errorf("outcome.Error = %q, want it to name the missing evidence fields", outcome.Error)
	}
	if outcome.Error == "" {
		t.Error("outcome.Error is empty — a gated rejection must carry the reason")
	}
}

// TestSCHEDGAP085_ClosedInWindowWithEvidenceStaysCompleted — the same
// in-window row WITH commit_hash evidence must NOT be rejected: the gate only
// fires when ALL of reasoning/commit_hash/worker_summary are empty.
func TestSCHEDGAP085_ClosedInWindowWithEvidenceStaysCompleted(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()

	// completed_at = {{NOW}}: in-window (stamped at request time).
	boardLine := `{"id":"SCHED-GAP-085-OK","status":"complete","worker_status":"complete","completed_at":"{{NOW}}","reasoning":"fixed","commit_hash":"deadbeef","worker_summary":"done"}`
	spawner := schedGap079Spawner(t, db, closureBoardHandler(t, workdir, boardLine))

	project := PackedProject{Name: "gap085-ok", Workdir: workdir}
	tick, err := spawner.Spawn(project, "gap085-ok-2026-08-30-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — evidence-bearing closures must stay completed", outcome.Status, TickCompleted)
	}
}

// TestSCHEDGAP085_PreexistingViolationFlagsButCompletes — a violation whose
// completed_at is BEFORE the tick window (closed by an earlier tick) must NOT
// fail the tick: it is flagged via WARN log + HIGH board_closure event and the
// tick completes normally.
func TestSCHEDGAP085_PreexistingViolationFlagsButCompletes(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()

	// completed_at far in the past — pre-existing, closed before this tick.
	boardLine := `{"id":"SCHED-GAP-077","status":"complete","worker_status":"complete","completed_at":"2026-08-28 00:15:20.000000","reasoning":null,"commit_hash":null,"worker_summary":null}`
	spawner := schedGap079Spawner(t, db, closureBoardHandler(t, workdir, boardLine))
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap085-preexisting", Workdir: workdir}
	const tickID = "gap085-preexisting-2026-08-30-10-00-00"
	tick, err := spawner.Spawn(project, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — pre-existing violations must not fail the tick", outcome.Status, TickCompleted)
	}
	if n := boardClosureEventCount(t, db, project.Name, tickID); n != 1 {
		t.Errorf("HIGH board_closure events = %d, want 1 — the pre-existing violation must be flagged", n)
	}
}

// TestSCHEDGAP085_NoBoardFileNoOp — a project workdir without a board file
// must be a no-op: the gate never fails (or flags) a tick with no board.
func TestSCHEDGAP085_NoBoardFileNoOp(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir() // no .coding-hermes/board at all

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_noboard",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "work"},
					},
				},
			},
			"usage": map[string]int{},
		})
	})

	project := PackedProject{Name: "gap085-noboard", Workdir: workdir}
	tick, err := spawner.Spawn(project, "gap085-noboard-2026-08-30-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — no board file must be a no-op", outcome.Status, TickCompleted)
	}
}
