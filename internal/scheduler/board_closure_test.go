package scheduler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
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

// ── SCHED-GAP-163: pre-existing closure-evidence event throttle ────────────
//
// Pre-existing (unrepairable, e.g. 2026-08 legacy) closure-evidence violations
// used to re-emit a HIGH board_closure event on EVERY tick touching the
// project — measured 337 HIGH rows between 2026-09-17 and 2026-09-18, the
// SCHED-GAP-061 desensitization failure repeating. The gate now fingerprints
// the flagged set per project (ClosureViolationFingerprint) and emits at most
// one HIGH per closureViolationReMindGap while the set is unchanged; a CHANGED
// set emits HIGH immediately, and an unchanged set re-minds as a MEDIUM
// "(still unresolved)" row after the window. The in-window REJECT path is
// untouched: a fresh violation still fails the tick, unthrottled.

// closureViolationLine renders a CLOSED board row that carries no closure
// evidence (all of reasoning/commit_hash/worker_summary empty) and was closed
// in the past, i.e. pre-existing relative to any tick window a test opens.
func closureViolationLine(id string) string {
	return `{"id":"` + id + `","status":"complete","worker_status":"complete","completed_at":"2026-08-28 00:15:20.000000","reasoning":null,"commit_hash":null,"worker_summary":null}`
}

// boardClosureGateBoard writes a board JSONL at the path findBoardFile reads
// (workdir/.coding-hermes/board/tasks.jsonl) and returns that path.
func boardClosureGateBoard(t *testing.T, workdir string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	path := filepath.Join(dir, "tasks.jsonl")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// closureCompletedHandler answers every request as a normal completed gateway
// tick (real session id + output text) and touches no files — the SCHED-GAP-163
// tests write the board themselves so a test can change the flagged set
// between ticks.
func closureCompletedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_closure_gate",
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

// runClosureGateTick spawns one gateway tick and returns the outcome — the
// closure gate runs inside Wait().
func runClosureGateTick(t *testing.T, spawner *Spawner, project, workdir, tickID string) TickOutcome {
	t.Helper()
	tick, err := spawner.Spawn(PackedProject{Name: project, Workdir: workdir}, tickID)
	if err != nil {
		t.Fatalf("Spawn %s: %v", tickID, err)
	}
	if tick == nil {
		t.Fatalf("Spawn %s returned nil tick", tickID)
	}
	return tick.Wait()
}

// closureEventRow is one board_closure event row for the project under test.
type closureEventRow struct {
	Severity string
	Message  string
	Details  string
}

// closureEventRows returns every board_closure event for a project, oldest
// first (id order = emission order).
func closureEventRows(t *testing.T, db *sql.DB, project string) []closureEventRow {
	t.Helper()
	rows, err := db.Query(`SELECT severity, message, details FROM events
		WHERE component='board_closure' AND json_extract(details, '$.project') = ?
		ORDER BY id ASC`, project)
	if err != nil {
		t.Fatalf("query board_closure events: %v", err)
	}
	defer rows.Close()
	var out []closureEventRow
	for rows.Next() {
		var r closureEventRow
		if err := rows.Scan(&r.Severity, &r.Message, &r.Details); err != nil {
			t.Fatalf("scan board_closure event: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter board_closure events: %v", err)
	}
	return out
}

// closureEventDetails decodes one event's details JSON.
func closureEventDetails(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal event details %q: %v", raw, err)
	}
	return m
}

// closureEventFingerprint reads details.fingerprint off an emitted row.
func closureEventFingerprint(t *testing.T, raw string) string {
	t.Helper()
	fp, _ := closureEventDetails(t, raw)["fingerprint"].(string)
	return fp
}

// TestGAP163_ClosureViolationFingerprint — the fingerprint is derived from the
// SORTED violation set: row order and missing-field order cannot change it; a
// row entering or leaving the set (or a different missing-field set) does.
func TestGAP163_ClosureViolationFingerprint(t *testing.T) {
	all := []string{"reasoning", "commit_hash", "worker_summary"}
	base := []ClosureViolation{
		{ID: "BAD-1", MissingFields: []string{"reasoning", "commit_hash", "worker_summary"}, CompletedAt: "2026-08-28 00:15:20"},
		{ID: "BAD-2", MissingFields: []string{"reasoning", "commit_hash", "worker_summary"}, CompletedAt: "2026-08-27 10:00:00"},
	}
	reordered := []ClosureViolation{
		{ID: "BAD-2", MissingFields: []string{"worker_summary", "reasoning", "commit_hash"}, CompletedAt: "2026-08-27 10:00:00"},
		{ID: "BAD-1", MissingFields: []string{"commit_hash", "worker_summary", "reasoning"}, CompletedAt: "2026-08-28 00:15:20"},
	}

	fpBase := ClosureViolationFingerprint(base)
	fpReordered := ClosureViolationFingerprint(reordered)
	if fpBase == "" {
		t.Fatal("fingerprint of a non-empty violation set is empty")
	}
	if len(fpBase) != 16 {
		t.Errorf("fingerprint = %q (len %d), want 16 hex chars", fpBase, len(fpBase))
	}
	if fpBase != fpReordered {
		t.Errorf("fingerprint order-dependent: %q != %q — same SET in a different order must hash identically", fpBase, fpReordered)
	}

	extra := append([]ClosureViolation{{ID: "BAD-3", MissingFields: all, CompletedAt: "2026-08-26 09:00:00"}}, base...)
	if fpExtra := ClosureViolationFingerprint(extra); fpExtra == fpBase {
		t.Errorf("fingerprint unchanged after a row was ADDED: %q — a changed set must produce a different fingerprint", fpExtra)
	}

	removed := base[:1]
	if fpRemoved := ClosureViolationFingerprint(removed); fpRemoved == fpBase {
		t.Errorf("fingerprint unchanged after a row was REMOVED: %q", fpRemoved)
	}

	// A different missing-field set for the same row id is a changed set.
	fieldShift := []ClosureViolation{
		{ID: "BAD-1", MissingFields: []string{"reasoning", "commit_hash"}, CompletedAt: "2026-08-28 00:15:20"},
		{ID: "BAD-2", MissingFields: all, CompletedAt: "2026-08-27 10:00:00"},
	}
	if fp := ClosureViolationFingerprint(fieldShift); fp == fpBase {
		t.Errorf("fingerprint unchanged after a missing-field set changed: %q", fp)
	}

	if fp := ClosureViolationFingerprint(nil); fp != "" {
		t.Errorf("fingerprint of an empty set = %q, want \"\"", fp)
	}
}

// TestGAP163_BoardRescanOrderIndependence — the same board rows appended in a
// different line order must fingerprint identically (the scanner returns board
// order; the fingerprint must not).
func TestGAP163_BoardRescanOrderIndependence(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	pathA := boardClosureGateBoard(t, dirA, closureViolationLine("BAD-1"), closureViolationLine("BAD-2"))
	pathB := boardClosureGateBoard(t, dirB, closureViolationLine("BAD-2"), closureViolationLine("BAD-1"))

	vA, err := BoardClosureViolations(pathA)
	if err != nil {
		t.Fatalf("BoardClosureViolations(A): %v", err)
	}
	vB, err := BoardClosureViolations(pathB)
	if err != nil {
		t.Fatalf("BoardClosureViolations(B): %v", err)
	}
	if len(vA) != 2 || len(vB) != 2 {
		t.Fatalf("violations A=%d B=%d, want 2 each", len(vA), len(vB))
	}
	if fpA, fpB := ClosureViolationFingerprint(vA), ClosureViolationFingerprint(vB); fpA != fpB {
		t.Errorf("board line order changed the fingerprint: %q != %q", fpA, fpB)
	}
}

// TestGAP163_UnchangedViolationsSuppressedWithinWindow — two ticks on a board
// whose (immutable, pre-existing) violation set did not change inside the
// re-mind window emit exactly ONE board_closure event in total.
func TestGAP163_UnchangedViolationsSuppressedWithinWindow(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	spawner := schedGap079Spawner(t, db, closureCompletedHandler())
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-suppress"
	out1 := runClosureGateTick(t, spawner, project, workdir, "gap163-suppress-2026-09-18-10-00-00")
	if out1.Status != TickCompleted {
		t.Fatalf("tick 1 status = %s, want %s — pre-existing violations must not fail the tick (%s)", out1.Status, TickCompleted, out1.Error)
	}
	rows := closureEventRows(t, db, project)
	if len(rows) != 1 || rows[0].Severity != "HIGH" {
		t.Fatalf("after tick 1: %d event(s) %+v, want exactly 1 HIGH (first occurrence)", len(rows), rows)
	}
	if fp := closureEventFingerprint(t, rows[0].Details); fp == "" {
		t.Error("first emission carries no details.fingerprint — the throttle has nothing to dedup on")
	}

	out2 := runClosureGateTick(t, spawner, project, workdir, "gap163-suppress-2026-09-18-11-00-00")
	if out2.Status != TickCompleted {
		t.Errorf("tick 2 status = %s, want %s", out2.Status, TickCompleted)
	}
	rows = closureEventRows(t, db, project)
	if len(rows) != 1 {
		t.Errorf("board_closure events = %d (%+v), want 1 — an unchanged violation set inside the re-mind window must be suppressed", len(rows), rows)
	}
}

// TestGAP163_UnchangedViolationsReMindedAsMediumAfterWindow — once the
// re-mind window has elapsed the unchanged set is re-minded as a MEDIUM
// "(still unresolved)" row carrying details.reminder=true — never another HIGH.
func TestGAP163_UnchangedViolationsReMindedAsMediumAfterWindow(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	spawner := schedGap079Spawner(t, db, closureCompletedHandler())
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-remind"
	out1 := runClosureGateTick(t, spawner, project, workdir, "gap163-remind-2026-09-18-10-00-00")
	if out1.Status != TickCompleted {
		t.Fatalf("tick 1 status = %s, want %s", out1.Status, TickCompleted)
	}

	// Backdate the first emission past the re-mind gap.
	backdated := time.Now().UTC().Add(-(closureViolationReMindGap + time.Hour)).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE events SET created_at = ? WHERE component='board_closure' AND json_extract(details,'$.project') = ?`,
		backdated, project); err != nil {
		t.Fatalf("backdate first emission: %v", err)
	}
	var stored string
	if err := db.QueryRow(`SELECT created_at FROM events WHERE component='board_closure' AND json_extract(details,'$.project') = ?`, project).Scan(&stored); err != nil {
		t.Fatalf("read back backdated created_at: %v", err)
	}
	if stored != backdated {
		t.Fatalf("premise failed: created_at = %q, want %q (the re-mind window must have elapsed)", stored, backdated)
	}

	out2 := runClosureGateTick(t, spawner, project, workdir, "gap163-remind-2026-09-18-11-00-00")
	if out2.Status != TickCompleted {
		t.Errorf("tick 2 status = %s, want %s", out2.Status, TickCompleted)
	}
	rows := closureEventRows(t, db, project)
	if len(rows) != 2 {
		t.Fatalf("board_closure events = %d (%+v), want 2 — an unchanged set after the window must re-mind once", len(rows), rows)
	}
	if rows[1].Severity != "MEDIUM" {
		t.Errorf("re-mind severity = %s, want MEDIUM (HIGH is reserved for first onset / changed set)", rows[1].Severity)
	}
	if rows[1].Message != closureViolationReminderMessage {
		t.Errorf("re-mind message = %q, want %q", rows[1].Message, closureViolationReminderMessage)
	}
	details := closureEventDetails(t, rows[1].Details)
	if details["reminder"] != true {
		t.Errorf("re-mind details.reminder = %v, want true", details["reminder"])
	}
	if got, want := closureEventFingerprint(t, rows[1].Details), closureEventFingerprint(t, rows[0].Details); got != want {
		t.Errorf("re-mind fingerprint = %q, want the unchanged fingerprint %q", got, want)
	}
	if n, _ := details["violation_count"].(float64); int(n) != 1 {
		t.Errorf("re-mind details.violation_count = %v, want 1", details["violation_count"])
	}
}

// TestGAP163_ChangedSetEmitsHighImmediately — a second row entering the
// violation set is a CHANGED set: the next tick emits HIGH at once, not a
// suppressed tick and not a demoted reminder.
func TestGAP163_ChangedSetEmitsHighImmediately(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	spawner := schedGap079Spawner(t, db, closureCompletedHandler())
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-changed"
	out1 := runClosureGateTick(t, spawner, project, workdir, "gap163-changed-2026-09-18-10-00-00")
	if out1.Status != TickCompleted {
		t.Fatalf("tick 1 status = %s, want %s", out1.Status, TickCompleted)
	}

	// BAD-2 is now also closed-without-evidence — same project, different set.
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"), closureViolationLine("BAD-2"))
	out2 := runClosureGateTick(t, spawner, project, workdir, "gap163-changed-2026-09-18-11-00-00")
	if out2.Status != TickCompleted {
		t.Errorf("tick 2 status = %s, want %s", out2.Status, TickCompleted)
	}

	rows := closureEventRows(t, db, project)
	if len(rows) != 2 {
		t.Fatalf("board_closure events = %d (%+v), want 2 — a changed set must emit immediately", len(rows), rows)
	}
	if rows[1].Severity != "HIGH" {
		t.Errorf("changed-set severity = %s, want HIGH (a changed set is never throttled/demoted)", rows[1].Severity)
	}
	if rows[1].Message != closureViolationMessage {
		t.Errorf("changed-set message = %q, want %q", rows[1].Message, closureViolationMessage)
	}
	fp1, fp2 := closureEventFingerprint(t, rows[0].Details), closureEventFingerprint(t, rows[1].Details)
	if fp1 == fp2 {
		t.Errorf("fingerprints identical (%q) after the set changed", fp1)
	}
	details := closureEventDetails(t, rows[1].Details)
	if n, _ := details["violation_count"].(float64); int(n) != 2 {
		t.Errorf("changed-set details.violation_count = %v, want 2", details["violation_count"])
	}
}

// TestGAP163_NewProjectEmitsHigh — no prior board_closure emission for the
// project: the first observation is HIGH.
func TestGAP163_NewProjectEmitsHigh(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	spawner := schedGap079Spawner(t, db, closureCompletedHandler())
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-newproject"
	if prior := closureEventRows(t, db, project); len(prior) != 0 {
		t.Fatalf("premise failed: %d pre-existing board_closure event(s)", len(prior))
	}
	out := runClosureGateTick(t, spawner, project, workdir, "gap163-newproject-2026-09-18-10-00-00")
	if out.Status != TickCompleted {
		t.Fatalf("status = %s, want %s", out.Status, TickCompleted)
	}
	rows := closureEventRows(t, db, project)
	if len(rows) != 1 {
		t.Fatalf("board_closure events = %d, want 1", len(rows))
	}
	if rows[0].Severity != "HIGH" {
		t.Errorf("first-occurrence severity = %s, want HIGH", rows[0].Severity)
	}
	if rows[0].Message != closureViolationMessage {
		t.Errorf("first-occurrence message = %q, want %q", rows[0].Message, closureViolationMessage)
	}
}

// TestGAP163_LegacyRowWithoutFingerprintEmitsHigh — a prior event written
// before SCHED-GAP-163 (no details.fingerprint) cannot be matched, so the set
// is treated as changed one last time and emits HIGH — the upgrade path never
// silences an existing violation.
func TestGAP163_LegacyRowWithoutFingerprintEmitsHigh(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	spawner := schedGap079Spawner(t, db, closureCompletedHandler())
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-legacy"
	// Prior emission as the pre-GAP-163 gate wrote it: no fingerprint field.
	NewEventLogger(db).Emit(context.Background(), SeverityHigh, "board_closure", closureViolationMessage,
		map[string]any{"project": project, "tick_id": "gap163-legacy-seed", "violations": []any{}})

	var seededFP string
	if err := db.QueryRow(`SELECT COALESCE(json_extract(details,'$.fingerprint'),'') FROM events
		WHERE component='board_closure' AND json_extract(details,'$.tick_id') = 'gap163-legacy-seed'`).Scan(&seededFP); err != nil {
		t.Fatalf("read seeded fingerprint: %v", err)
	}
	if seededFP != "" {
		t.Fatalf("premise failed: seeded legacy row carries fingerprint %q, want none", seededFP)
	}

	out := runClosureGateTick(t, spawner, project, workdir, "gap163-legacy-2026-09-18-10-00-00")
	if out.Status != TickCompleted {
		t.Fatalf("status = %s, want %s", out.Status, TickCompleted)
	}
	rows := closureEventRows(t, db, project)
	if len(rows) != 2 {
		t.Fatalf("board_closure events = %d (%+v), want 2 — a prior row without a fingerprint must not suppress", len(rows), rows)
	}
	if rows[1].Severity != "HIGH" {
		t.Errorf("severity after a legacy prior row = %s, want HIGH", rows[1].Severity)
	}
	if rows[1].Message != closureViolationMessage {
		t.Errorf("message = %q, want %q", rows[1].Message, closureViolationMessage)
	}
}

// TestGAP163_NilDBFailsOpen — a spawner without a db (and without an event
// logger) must not panic: the gate logs the WARN line naming the violations,
// fails OPEN (no error return, no suppression machinery), and emits nothing.
func TestGAP163_NilDBFailsOpen(t *testing.T) {
	workdir := t.TempDir()
	boardClosureGateBoard(t, workdir, closureViolationLine("BAD-1"))

	st := &SpawnedTick{
		TickID:  "gap163-nildb-2026-09-18-10-00-00",
		Project: "gap163-nildb",
		workdir: workdir,
		spawner: &Spawner{}, // db nil, events nil
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	now := time.Now()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("boardClosureGate panicked with a nil-db spawner: %v", r)
			}
		}()
		err = st.boardClosureGate(now.Add(-time.Hour), now)
	}()

	if err != nil {
		t.Errorf("boardClosureGate = %v, want nil — pre-existing violations never fail the tick", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "[board_closure]") {
		t.Errorf("WARN path did not run: log output = %q", logged)
	}
	if !strings.Contains(logged, "BAD-1") {
		t.Errorf("WARN line does not name the violating row: %q", logged)
	}
}

// TestGAP163_RejectedInWindowPathUnthrottled — the in-window REJECT path is
// untouched by the throttle: a row closed by THIS tick without evidence makes
// the gate return an error on every tick, even when a recent board_closure
// event already exists for the project, and the reject path emits no event.
func TestGAP163_RejectedInWindowPathUnthrottled(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()

	boardLine := `{"id":"GAP163-REJECT","status":"complete","worker_status":"complete","completed_at":"{{NOW}}","reasoning":null,"commit_hash":null,"worker_summary":null}`
	spawner := schedGap079Spawner(t, db, closureBoardHandler(t, workdir, boardLine))
	spawner.SetEventLogger(NewEventLogger(db))

	const project = "gap163-reject"
	// A recent prior emission for the project (as a flagged run would leave):
	// suppression machinery must not swallow a genuine in-window rejection.
	NewEventLogger(db).Emit(context.Background(), SeverityHigh, "board_closure", closureViolationMessage,
		map[string]any{"project": project, "tick_id": "gap163-reject-seed", "fingerprint": "deadbeefdeadbeef"})

	for i, tickID := range []string{
		"gap163-reject-2026-09-18-10-00-00",
		"gap163-reject-2026-09-18-11-00-00",
	} {
		out := runClosureGateTick(t, spawner, project, workdir, tickID)
		if out.Status != TickFailed {
			t.Errorf("tick %d status = %s, want %s — an in-window closure without evidence must reject the tick", i+1, out.Status, TickFailed)
		}
		if !strings.Contains(out.Error, "GAP163-REJECT") {
			t.Errorf("tick %d error = %q, want it to name the violating row", i+1, out.Error)
		}
	}

	rows := closureEventRows(t, db, project)
	if len(rows) != 1 {
		t.Errorf("board_closure events = %d (%+v), want 1 (the seed only) — the reject path emits no event", len(rows), rows)
	}
	if rows[0].Severity != "HIGH" || rows[0].Message != closureViolationMessage {
		t.Errorf("seed event = %s/%q, want HIGH/%q", rows[0].Severity, rows[0].Message, closureViolationMessage)
	}
}
