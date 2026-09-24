package database

// SCHED-GAP-089 — stale api_server session reaper over the REAL agent state
// store.
//
// The first SCHED-GAP-089 implementation was fake: it invented a
// scheduler-local `sessions` table (migration v23: platform TEXT,
// created_at TEXT) that no real Hermes session ever lived in, and "reaped"
// rows only it had written. This file replaces that with an honest reaper
// shaped by the real Hermes state.db schema:
//
//	sessions(id TEXT PK, source TEXT NOT NULL, started_at REAL NOT NULL,
//	         ended_at REAL, end_reason TEXT, last_activity_at REAL, ...)
//
// — epoch-seconds REALs, not text timestamps; `source`, not `platform`;
// no created_at/updated_at columns at all.
//
// Safety model (why this is not wired deeper into the daemon):
//
//   - state.db is the LIVE agent process's own database (WAL). The reaper
//     opens it with mode=rw (never creates) and a busy timeout so it queues
//     behind the agent instead of corrupting or starving it.
//   - The scheduler has NO liveness handshake with the gateway, so "stale"
//     is an inactivity heuristic, not proof the API client is gone. Writing
//     is therefore NEVER a default: a pass mutates nothing unless the
//     caller explicitly sets Apply.
//   - The reaper FAILS CLOSED on any database whose sessions table lacks the
//     real columns — in particular the old fake v23 shape (platform /
//     created_at / updated_at) is rejected with ErrHermesSessionsShape
//     rather than silently "reaped".
//   - The scheduler's own --db is deliberately NOT involved: it holds no
//     sessions table (v23 is tombstoned, see migrations.go) and must never
//     grow one again.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// DefaultHermesReapStaleAfter is the default inactivity age past which an
// open api_server session is considered stale. Applied when
// HermesReaperConfig.StaleAfter is <= 0.
const DefaultHermesReapStaleAfter = 24 * time.Hour

// EndReasonReaped is the end_reason the reaper stamps on sessions it closes
// (SCHED-GAP-089). Distinct from the agent's own reasons (agent_close,
// cli_close, cron_complete, ...) so a reaped row is always auditable.
const EndReasonReaped = "reaped"

// hermesBusyTimeoutMS bounds how long a reaper connection waits on the live
// agent's locks before failing the pass (best-effort — a busy database must
// never wedge the daemon).
const hermesBusyTimeoutMS = 5000

// ErrHermesSessionsShape is returned when the target database's sessions
// table does not carry the real Hermes schema. Callers should treat it as
// "wrong database", never as "nothing to do".
var ErrHermesSessionsShape = errors.New("sessions table does not match the real Hermes state.db schema (want source TEXT + started_at/ended_at/last_activity_at REAL + end_reason TEXT)")

// hermesSessionColumns are the real-schema columns the reaper reads and
// writes. All five must exist, or the pass fails closed.
var hermesSessionColumns = []string{"source", "started_at", "ended_at", "end_reason", "last_activity_at"}

// HermesReaperConfig controls one reap pass. The ZERO VALUE IS SAFE: with
// Apply unset the pass is a dry-run — it selects and reports exactly what it
// would close and mutates nothing.
type HermesReaperConfig struct {
	// StaleAfter is the inactivity age past which an open api_server
	// session is stale. <= 0 selects DefaultHermesReapStaleAfter.
	StaleAfter time.Duration

	// Apply turns the pass from a dry-run into a write: selected sessions
	// get ended_at = COALESCE(last_activity_at, started_at) and
	// end_reason = 'reaped'. This is an explicit operator decision, never
	// a default.
	Apply bool
}

// Normalized returns cfg with defaults filled in (StaleAfter <= 0 becomes
// DefaultHermesReapStaleAfter). Apply is passed through untouched — there is
// deliberately no default that writes.
func (c HermesReaperConfig) Normalized() HermesReaperConfig {
	if c.StaleAfter <= 0 {
		c.StaleAfter = DefaultHermesReapStaleAfter
	}
	return c
}

// Result reports what one pass selected and (on Apply) closed. On a dry-run
// Reaped is always 0.
type Result struct {
	// Candidates is the number of open api_server sessions older than the
	// staleness cutoff — what the pass WOULD close.
	Candidates int
	// Reaped is the number of sessions actually closed; 0 on a dry-run.
	Reaped int
	// OldestIdle is the inactivity age of the stalest selected session
	// (0 when none were selected).
	OldestIdle time.Duration
	// Apply mirrors the config: true when this pass wrote.
	Apply bool
}

// DefaultHermesSessionDBPath returns the default agent state database path
// ($HOME/.hermes/state.db), or "" when the home directory cannot be
// resolved.
func DefaultHermesSessionDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes", "state.db")
}

// OpenHermesStateDB opens the agent state database for the reaper. The file
// must already exist (mode=rw never creates one — a missing file is an
// error, not a fresh database). The handle is write-capable but pass-gated:
// ReapStaleHermesSessions only issues an UPDATE when Apply is set. The busy
// timeout keeps the pass queuing behind the live agent's own locks instead
// of erroring on first contention.
func OpenHermesStateDB(path string) (*sql.DB, error) {
	if path == "" {
		path = DefaultHermesSessionDBPath()
	}
	if path == "" {
		return nil, errors.New("no state database path (home directory unresolved; pass --session-db)")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("state database %s not accessible: %w", path, err)
	}
	dsn := fmt.Sprintf("file:%s?mode=rw&_pragma=busy_timeout(%d)", path, hermesBusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	return db, nil
}

// reapSelectQuery is the single source of truth for which rows a reap pass
// targets: open (ended_at IS NULL) api_server sessions whose last activity —
// falling back to started_at when last_activity_at was never recorded
// (observed on 15 of 140k live rows) — is older than the cutoff. Both the
// dry-run COUNT and the Apply UPDATE use this exact WHERE shape.
const reapSelectPredicate = `
 WHERE source = 'api_server'
   AND ended_at IS NULL
   AND COALESCE(last_activity_at, started_at) < ?`

// ReapStaleHermesSessions runs one pass over an OPEN state database handle.
//
//   - Dry-run (cfg.Apply false, the zero-value default): returns the count
//     and stalest-idle of the sessions a write pass would close and mutates
//     nothing — provably, because no UPDATE statement exists on this path.
//   - Apply (cfg.Apply true): closes exactly the selected rows with
//     ended_at = COALESCE(last_activity_at, started_at) (the session's own
//     last activity, never the reap time) and end_reason = 'reaped'.
//
// The pass is idempotent: a closed row no longer matches ended_at IS NULL,
// so a second identical pass selects zero rows.
//
// The clock comes from the context's clock seam (clock.WithClock) so tests
// can inject time instead of sleeping. A sessions table that predates or
// diverges from the real schema fails closed with ErrHermesSessionsShape.
func ReapStaleHermesSessions(ctx context.Context, db *sql.DB, cfg HermesReaperConfig) (Result, error) {
	cfg = cfg.Normalized()

	if err := validateHermesSessionsSchema(ctx, db); err != nil {
		return Result{}, err
	}

	// The real columns are epoch-seconds REALs (with fractional seconds),
	// so the cutoff is computed in Go and bound as a float — no string
	// comparison, no julianday() interpretation.
	now := clock.FromContext(ctx).Now().UTC()
	nowReal := float64(now.UnixNano()) / 1e9
	cutoff := now.Add(-cfg.StaleAfter)
	cutoffReal := float64(cutoff.UnixNano()) / 1e9

	// Select first: Candidates + OldestIdle are reported on BOTH paths, so
	// a dry-run and its matching write pass log the same numbers. MIN over
	// the activity timestamps is the STALEST candidate (most idle), not the
	// freshest one.
	var candidates int
	var oldestReal float64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(COALESCE(last_activity_at, started_at)), 0)
		   FROM sessions`+reapSelectPredicate,
		cutoffReal).Scan(&candidates, &oldestReal)
	if err != nil {
		return Result{}, fmt.Errorf("select stale sessions: %w", err)
	}

	res := Result{Candidates: candidates, Apply: cfg.Apply}
	if candidates > 0 {
		if idle := nowReal - oldestReal; idle > 0 {
			res.OldestIdle = time.Duration(idle * float64(time.Second))
		}
	}
	if !cfg.Apply || candidates == 0 {
		return res, nil
	}

	// Write pass: same predicate as the select above (pinned by
	// reapSelectPredicate) — nothing else is eligible.
	updated, err := db.ExecContext(ctx,
		`UPDATE sessions
		    SET ended_at = COALESCE(last_activity_at, started_at),
		        end_reason = ?`+reapSelectPredicate,
		EndReasonReaped, cutoffReal)
	if err != nil {
		return Result{}, fmt.Errorf("close stale sessions: %w", err)
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return Result{}, fmt.Errorf("close stale sessions rows affected: %w", err)
	}
	res.Reaped = int(n)
	return res, nil
}

// validateHermesSessionsSchema fails closed unless the sessions table
// carries every real-schema column the reaper touches. A missing table (a
// scheduler DB, an empty test file) and the old fake v23 shape
// (platform/created_at/updated_at) both land here.
func validateHermesSessionsSchema(ctx context.Context, db *sql.DB) error {
	for _, col := range hermesSessionColumns {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col).Scan(&n)
		if err != nil {
			return fmt.Errorf("inspect sessions.%s: %w", col, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: missing column %q", ErrHermesSessionsShape, col)
		}
	}
	return nil
}
