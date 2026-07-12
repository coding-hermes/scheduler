// Package database provides the SQLite-backed operational store for the
// coding-hermes fleet scheduler. It manages three core tables — projects,
// ticks (individual scheduler runs), and events (operational log) — plus a
// migrations table that tracks applied schema versions.
//
// All access goes through database/sql against a pure-Go modernc.org/sqlite
// driver (no cgo). The store runs in WAL mode with foreign keys enforced.
package database
