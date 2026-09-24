package database

// SCHED-GAP-089 tombstone — this file deliberately no longer contains the
// fake session-reaper implementation.
//
// The first SCHED-GAP-089 attempt (migration v23 + ReapZombieSessions here)
// was a fake: it created a scheduler-local `sessions` table shaped
// (id, platform TEXT, created_at TEXT, updated_at, ended_at TEXT) that no
// real Hermes session ever lived in, then "reaped" rows only it had
// written. Real Hermes sessions live in ~/.hermes/state.db with a
// completely different shape (source TEXT NOT NULL, started_at/ended_at/
// last_activity_at REAL epoch seconds, end_reason TEXT) — see
// hermesstate.go for the real-schema-shaped reaper, HermesReaperConfig for
// its dry-run-default safety model, and the v44 tombstone migration in
// migrations.go for how databases that already ran the fake v23 are healed.
//
// The scheduler's own database must never grow a sessions table again.
