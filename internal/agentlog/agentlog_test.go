package agentlog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newStateDB builds a minimal but schema-faithful state database: the
// sessions/messages tables carry the columns the reader queries. Rows are
// inserted directly — the reader never writes, so the fixture owns the only
// writes in this file.
func newStateDB(t *testing.T) (dbPath string, sessID string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture state.db: %v", err)
	}
	defer db.Close()

	schema := `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    model TEXT,
    billing_provider TEXT,
    started_at REAL NOT NULL,
    ended_at REAL,
    message_count INTEGER DEFAULT 0,
    tool_call_count INTEGER DEFAULT 0
);
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL,
    content TEXT,
    tool_name TEXT,
    timestamp REAL NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	sessID = "623c7302-a7ef-47d9-96dd-e47cf1c42738"
	if _, err := db.Exec(`INSERT INTO sessions (id, model, billing_provider, started_at, ended_at, message_count, tool_call_count)
VALUES (?, 'glm-5.3-flash', 'xkiro', 1790279310.06, 1790279520.5, 4, 2)`, sessID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	turns := []struct {
		role, content, tool string
		ts                  float64
	}{
		{"user", "[Scheduler tick: t-123] Fix the flaky test.", "", 1790279310.1},
		{"assistant", "Reading the failing test first.", "", 1790279315.2},
		{"tool", `{"output":"FAIL: TestFlaky"}`, "terminal", 1790279320.3},
		{"assistant", "Fixed it: the clock seam was nil. Committed abc123.", "", 1790279520.4},
	}
	for _, tr := range turns {
		if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, tool_name, timestamp) VALUES (?,?,?,?,?)`,
			sessID, tr.role, tr.content, tr.tool, tr.ts); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	return path, sessID
}

func TestFetchSession_ResolvesTranscript(t *testing.T) {
	path, sessID := newStateDB(t)
	r := NewReader(path)

	res := r.FetchSession(context.Background(), sessID)
	if res.Status != StatusResolved {
		t.Fatalf("status = %q (detail: %s), want resolved", res.Status, res.Detail)
	}
	if res.Session.ID != sessID {
		t.Errorf("session id = %q, want %q", res.Session.ID, sessID)
	}
	if res.Session.Model != "glm-5.3-flash" || res.Session.Provider != "xkiro" {
		t.Errorf("model/provider = %q/%q, want glm-5.3-flash/xkiro", res.Session.Model, res.Session.Provider)
	}
	if len(res.Turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(res.Turns))
	}
	// Chronological order, first the prompt then the agent's text.
	if res.Turns[0].Role != "user" || !strings.Contains(res.Turns[0].Content, "Fix the flaky test") {
		t.Errorf("first turn = %s/%q, want the user prompt", res.Turns[0].Role, res.Turns[0].Content)
	}
	last := res.Turns[3]
	if last.Role != "assistant" || !strings.Contains(last.Content, "Fixed it") {
		t.Errorf("last turn = %s/%q, want the assistant's generated text", last.Role, last.Content)
	}
	// Tool row carries its tool name.
	if res.Turns[2].ToolName != "terminal" {
		t.Errorf("tool name = %q, want terminal", res.Turns[2].ToolName)
	}
}

func TestFetchSession_SessionNotFound(t *testing.T) {
	path, _ := newStateDB(t)
	r := NewReader(path)

	res := r.FetchSession(context.Background(), "00000000-0000-0000-0000-000000000000")
	if res.Status != StatusSessionNotFound {
		t.Fatalf("status = %q, want session-not-found", res.Status)
	}
	if res.Detail == "" {
		t.Fatal("empty Detail — the UI must be able to say WHY nothing is shown")
	}
	if !strings.Contains(res.Detail, "no session") {
		t.Errorf("detail %q should name the missing session", res.Detail)
	}
}

func TestFetchSession_MissingFileIsUnavailableNotError(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "does-not-exist.db"))

	res := r.FetchSession(context.Background(), "whatever")
	if res.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", res.Status)
	}
	if res.Detail == "" {
		t.Fatal("empty Detail on unavailable")
	}
	// mode=ro must never create the file: assert the reader's own path is
	// still absent (SQLite in rw mode would have created an empty db).
	if _, err := os.Stat(r.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reader path %s should still not exist (mode=ro must never create)", r.path)
	}
}

func TestFetchSession_CorruptFileIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReader(path)

	res := r.FetchSession(context.Background(), "whatever")
	if res.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable for a corrupt file", res.Status)
	}
	if !strings.Contains(res.Detail, "state database") {
		t.Errorf("detail %q should name the state database failure", res.Detail)
	}
}

func TestFetchSession_TruncatesHugeTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT, started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_name TEXT, timestamp REAL NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", maxContentBytes+5000)
	if _, err := db.Exec(`INSERT INTO sessions (id, started_at) VALUES ('s1', 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant',?,101)`, big); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r := NewReader(path)
	res := r.FetchSession(context.Background(), "s1")
	if res.Status != StatusResolved {
		t.Fatalf("status = %q, want resolved", res.Status)
	}
	if len(res.Turns) != 1 || !res.Turns[0].Truncated {
		t.Fatalf("want one truncated turn, got %+v", res.Turns)
	}
	if len(res.Turns[0].Content) >= len(big) {
		t.Errorf("content not truncated: %d chars", len(res.Turns[0].Content))
	}
	if !strings.Contains(res.Turns[0].Content, "[truncated") {
		t.Error("truncation must be marked, never silent")
	}
}

func TestFetchSession_CapsTurnCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, billing_provider TEXT, started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_name TEXT, timestamp REAL NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, started_at) VALUES ('s1', 100)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxTurns+7; i++ {
		if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','m',?)`, float64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	r := NewReader(path)
	res := r.FetchSession(context.Background(), "s1")
	if res.Status != StatusResolved || !res.Capped || len(res.Turns) != maxTurns {
		t.Fatalf("status=%q capped=%v turns=%d, want resolved/true/%d", res.Status, res.Capped, len(res.Turns), maxTurns)
	}
}
