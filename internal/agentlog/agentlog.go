// Package agentlog reads the agent's own output (the Hermes state.db
// sessions/messages tables) so dashboard pages can show what the agent
// actually generated during a tick.
//
// SCHED-GAP-1593: the scheduler's ticks.gateway_trace JSON carries the REAL
// gateway session id (ticks.session_id is the scheduler's own tick id and
// does not resolve anywhere). That session id resolves into the agent state
// database (~/.hermes/state.db) whose messages table holds every turn the
// agent produced — user prompt, assistant text, tool calls and results.
//
// Contract:
//   - the database is opened READ-ONLY (file:...?mode=ro) and LAZILY — a
//     dashboard render must never fail because the file is missing, locked
//     or corrupt. Every failure degrades into a typed Status with a human
//     explanation; callers render that explanation instead of an empty pane
//     (an operator must never mistake "could not fetch" for "nothing").
//   - the reader never writes, never creates the file, never blocks longer
//     than the busy timeout.
package agentlog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Status classifies the outcome of a session lookup. Every non-resolved
// status MUST be rendered with its Detail text — they exist so the UI can
// distinguish "the agent produced nothing" (never the case here) from
// "we could not fetch it" (every other status).
type Status string

const (
	// StatusResolved: the session was found and turns were loaded.
	StatusResolved Status = "resolved"
	// StatusSessionNotFound: state.db opened fine but holds no such session id.
	StatusSessionNotFound Status = "session-not-found"
	// StatusUnavailable: state.db could not be opened or queried at all.
	StatusUnavailable Status = "unavailable"
)

const (
	// maxTurns caps the transcript rows loaded per render.
	maxTurns = 500
	// maxContentBytes caps a single turn's content; larger content is
	// truncated with an explicit marker (never silently).
	maxContentBytes = 4000
	// busyTimeoutMS bounds how long a read waits on a locked state.db.
	busyTimeoutMS = 2000
)

// Turn is one row of the agent transcript.
type Turn struct {
	Role      string // user | assistant | tool | system
	Content   string // the generated text / tool result payload (already truncated)
	ToolName  string // set for tool-result rows
	Timestamp time.Time
	Truncated bool // Content was cut at maxContentBytes
}

// SessionInfo describes the resolved session row.
type SessionInfo struct {
	ID            string
	Model         string
	Provider      string // billing_provider in state.db
	StartedAt     time.Time
	EndedAt       *time.Time
	MessageCount  int
	ToolCallCount int
}

// Result is what a session lookup degrades into. Status + Detail always
// tell the truth; Session/Turns are only populated on StatusResolved.
type Result struct {
	Status Status
	Detail string // human-readable explanation, always set when Status != resolved
	Path   string // the state.db path attempted (for "unavailable" reporting)

	Session SessionInfo
	Turns   []Turn
	Capped  bool // Turns hit maxTurns; more rows exist in state.db
}

// epochToTime converts a state.db epoch-seconds REAL (with fractional
// seconds) into a UTC time.
func epochToTime(sec float64) time.Time {
	whole := int64(sec)
	return time.Unix(whole, int64((sec-float64(whole))*1e9)).UTC()
}

// Reader opens and caches a read-only handle on the agent state database.
// The zero value is not useful; construct with NewReader. A Reader is safe
// for concurrent use.
type Reader struct {
	path string

	mu sync.Mutex
	db *sql.DB // nil until first successful open
}

// NewReader returns a reader for the state database at path. No file access
// happens until the first lookup — the path may not exist yet.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Path returns the state database path this reader is bound to.
func (r *Reader) Path() string { return r.path }

// Close releases the cached handle (safe on a never-opened reader).
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// open returns the cached read-only handle, opening it on first use.
// mode=ro guarantees no writes (and no file creation); busy_timeout bounds
// contention with the live agent process that owns the database.
func (r *Reader) open() (*sql.DB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		return r.db, nil
	}
	if _, err := os.Stat(r.path); err != nil {
		return nil, fmt.Errorf("state database %s not accessible: %w", r.path, err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", r.path, busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database %s read-only: %w", r.path, err)
	}
	// Force the connection now so a bad file fails here, once, and the
	// caller can degrade immediately.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("read state database %s: %w", r.path, err)
	}
	r.db = db
	return db, nil
}

// FetchSession resolves one gateway session id into its transcript. It never
// returns an error — every failure mode becomes a non-resolved Status with a
// Detail string, so rendering can degrade honestly.
func (r *Reader) FetchSession(ctx context.Context, sessionID string) Result {
	res := Result{Path: r.path, Status: StatusUnavailable, Detail: "not fetched"}
	db, err := r.open()
	if err != nil {
		res.Detail = err.Error()
		return res
	}

	var (
		sess      SessionInfo
		startedAt float64
		endedAt   sql.NullFloat64
	)
	err = db.QueryRowContext(ctx, `
SELECT id, COALESCE(model,''), COALESCE(billing_provider,''), started_at, ended_at,
       COALESCE(message_count,0), COALESCE(tool_call_count,0)
FROM sessions WHERE id = ?`, sessionID).Scan(
		&sess.ID, &sess.Model, &sess.Provider, &startedAt, &endedAt,
		&sess.MessageCount, &sess.ToolCallCount)
	if err == sql.ErrNoRows {
		return Result{
			Path:   r.path,
			Status: StatusSessionNotFound,
			Detail: fmt.Sprintf("no session %s exists in the agent state database (%s) — the session may have been pruned, or the trace's id belongs to a different state database", sessionID, r.path),
		}
	}
	if err != nil {
		res.Detail = fmt.Sprintf("query state database %s failed: %v", r.path, err)
		return res
	}

	sess.StartedAt = epochToTime(startedAt)
	if endedAt.Valid {
		t := epochToTime(endedAt.Float64)
		sess.EndedAt = &t
	}

	rows, err := db.QueryContext(ctx, `
SELECT role, COALESCE(tool_name,''), COALESCE(content,''), timestamp
FROM messages WHERE session_id = ?
ORDER BY timestamp ASC, id ASC
LIMIT ?`, sessionID, maxTurns+1)
	if err != nil {
		res.Detail = fmt.Sprintf("query transcript of session %s failed: %v", sessionID, err)
		return res
	}
	defer rows.Close()

	turns := make([]Turn, 0, 16)
	seen := 0
	for rows.Next() {
		var (
			role, toolName, content string
			ts                      float64
		)
		if err := rows.Scan(&role, &toolName, &content, &ts); err != nil {
			res.Detail = fmt.Sprintf("scan transcript row of session %s failed: %v", sessionID, err)
			return res
		}
		seen++
		if seen > maxTurns {
			// The LIMIT was maxTurns+1: at least one further row exists.
			break
		}
		turn := Turn{
			Role:      role,
			ToolName:  toolName,
			Content:   content,
			Timestamp: epochToTime(ts),
		}
		if len(content) > maxContentBytes {
			turn.Content = content[:maxContentBytes] +
				fmt.Sprintf("\n… [truncated — full turn is %d chars; read it in the session log]", len(content))
			turn.Truncated = true
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		res.Detail = fmt.Sprintf("iterate transcript of session %s failed: %v", sessionID, err)
		return res
	}
	capped := seen > maxTurns

	return Result{
		Status:  StatusResolved,
		Path:    r.path,
		Session: sess,
		Turns:   turns,
		Capped:  capped,
	}
}
