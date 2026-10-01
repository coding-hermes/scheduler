package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// REMOTE-003 (§1 scheduler identity): every row the daemon writes carries the
// scheduler_id of the scheduler that wrote it, so a merged/federated view can
// always say WHO WROTE THIS. The identity is resolved ONCE at boot (hostname
// default, overridable via config/env — see cmd/schedulerd/main.go) and
// installed here via SetSchedulerID; the insert paths for projects, ticks and
// events stamp it from this process-level value. It is deliberately NOT
// derived per-tick: identity must be stable across restarts.
//
// DefaultSchedulerID is the fallback when SetSchedulerID was never called
// (unit tests, tooling): the empty string keeps every historical writer shape
// byte-identical and the schema default (NOT NULL DEFAULT ”) applies.
const DefaultSchedulerID = ""

var (
	schedulerIDOnce sync.Once
	schedulerIDVal  string
)

// SetSchedulerID installs the daemon's scheduler identity for the lifetime of
// the process. Called once at boot by cmd/schedulerd/main.go; later calls are
// ignored (the first writer wins — identity is stable across restarts and
// must never flap mid-process). Passing the empty string keeps the default.
func SetSchedulerID(id string) {
	schedulerIDOnce.Do(func() {
		schedulerIDVal = strings.TrimSpace(id)
	})
}

// SchedulerID returns the installed scheduler identity (possibly "" when
// never set — e.g. unit tests). Exposed so surfaces that need to DISPLAY the
// identity (API config snapshot, federation later rows) read one value.
func SchedulerID() string {
	return schedulerIDVal
}

// ResolveDefaultSchedulerID derives the host-derived default identity: the
// short hostname (no domain part). Exported so cmd/schedulerd and tests share
// exactly one derivation — a scheduler must never disagree with itself about
// its own name.
func ResolveDefaultSchedulerID() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "unknown"
	}
	// Short hostname: cut at the first dot (fleet boxes are named, not
	// FQDN-addressed, and two schedulers on one host distinguished via an
	// explicit SCHEDULER_ID override).
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

// stampSchedulerID returns the identity to store on a new row: the installed
// id when set, "" otherwise (the column's NOT NULL DEFAULT ” semantics — an
// empty string is the honest "written before identity existed" mark, which
// the migration backfill then claims for the booting scheduler).
func stampSchedulerID() string {
	return schedulerIDVal
}

// BackfillSchedulerID claims every row still carrying the empty identity for
// the given scheduler id. Called ONCE at first boot after Migrate (main.go),
// so pre-existing rows — written by this scheduler before identity existed —
// are attributed to it. A scheduler that boots with an id DIFFERENT from the
// one that wrote the legacy rows claims them anyway: the rows live in ITS
// database and the spec's ownership law (§4) says one scheduler owns every
// row in its own store.
func BackfillSchedulerID(ctx context.Context, db *sql.DB, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("backfill scheduler_id: empty id")
	}
	for _, table := range []string{"projects", "ticks", "events"} {
		q := fmt.Sprintf(`UPDATE %s SET scheduler_id = ? WHERE scheduler_id = ''`, table) // table is a fixed literal, never user input
		if _, err := db.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("backfill %s.scheduler_id: %w", table, err)
		}
	}
	return nil
}
