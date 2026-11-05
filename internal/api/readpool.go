package api

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ── PERF-001-R3-R3: the handler read pool + the warm health snapshot ──────
//
// The daemon writes through ONE serialized SQLite connection
// (database.InitDB, SetMaxOpenConns(1)) — that invariant is load-bearing for
// the FK OFF/ON pairing and stays untouched. The boot orphan-resume pass
// legitimately holds that connection for its event INSERTs for the first
// seconds after boot, and every DB-backed handler SELECT used to queue
// behind it: measured on a live-sized 16.7 GB copy (2026-11-05), /health
// answered 504 at 1.001s at t+1s AND t+5s, /status 5.17s at t+5s, and the
// first dashboard render degraded to a 40 KB partial page. The read
// surfaces now go through a separate mode=ro pool (database.OpenReadPool,
// WAL readers run concurrently with the single writer), and /health's two
// computes are additionally served from a snapshot refreshed at boot so the
// hot path issues no cold SELECT at all.

// SetReadDB installs the read-only handler pool (database.OpenReadPool).
// Nil is ignored, mirroring dashboard.Generator.SetDB. A Server without a
// read pool is not degraded: rdb() falls back to the shared write handle,
// which is the historical behavior every existing test relies on.
func (s *Server) SetReadDB(db *sql.DB) {
	if db != nil {
		s.readPool = db
	}
}

// rdb returns the handle the READ surfaces must use: the read-only pool
// when one was installed, else the shared handle (tests, embedded use).
// Every call site routed through rdb() is a pure SELECT surface; anything
// that writes (or runs inside a transaction with writes) must use s.db
// directly.
func (s *Server) rdb() *sql.DB {
	if s.readPool != nil {
		return s.readPool
	}
	return s.db
}

// healthSnapshot is one immutable /api/v1/health DB payload: the bounded
// active-tick count (SCHED-GAP-1622) and the stall census (SCHED-GAP-1755).
// The refresher replaces the whole pointer and never mutates a stored
// snapshot, so a handler holding an old one races nothing.
type healthSnapshot struct {
	activeTicks int
	stall       map[string]*StallCensusEntry
}

// healthRefreshIntervalDefault is how often the refresher recomputes the
// /api/v1/health snapshot. The two queries are partial-index bounded
// (SCHED-GAP-1622) plus a 24h-window GROUP BY; at once per second the cost
// is negligible against the read pool, and the count stays fresh enough for
// the ops watchdog's 1s probes.
const healthRefreshIntervalDefault = 1 * time.Second

// healthRefreshQueryTimeout bounds ONE refresher compute. It is deliberately
// independent of the request deadlines: a compute that cannot finish in 2s
// (OS page cache still cold at boot) simply leaves the previous snapshot in
// place for the next tick to replace.
const healthRefreshQueryTimeout = 2 * time.Second

// StartHealthRefresher begins refreshing the /api/v1/health snapshot every
// interval (<= 0 selects the default). The whole loop — FIRST compute
// included — runs on its own goroutine: main.go calls this during wiring,
// BEFORE the HTTP listener starts, and an inline first compute here delayed
// the listener by seconds on a live-sized DB (measured rework1: the 16.7 GB
// copy's cold stall-census scan pushed the first bind past t+3s, so the
// t+1s probes hit a closed port). Called once; a second call is a logged
// no-op.
//
// The refresh cadence reads the WALL clock on purpose — clock.Real(), not
// the server's clock seam. The daemon's clock seam is a SIM clock in
// --simulate mode (scale 1000 by default), and a sim-scaled ticker would run
// this refresher 1000x too fast, turning a 1 Hz monitor into a hot loop
// against the database. HTTP health consumers observe wall time, so the
// wall clock is the semantically correct timeline for the refresh cadence;
// the seam stays authoritative for every uptime/age FIELD the handler
// renders.
func (s *Server) StartHealthRefresher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = healthRefreshIntervalDefault
	}
	if !s.healthRefresherStarted.CompareAndSwap(false, true) {
		log.Printf("WARN: health refresher already started — ignoring second StartHealthRefresher")
		return
	}
	go s.refreshHealthSnapshotLoop(ctx, interval)
}

// refreshHealthSnapshotLoop computes the snapshot once, then recomputes on
// every tick until ctx is cancelled. A failed compute keeps the previous
// snapshot: the handler's inline fallback re-derives the same values under
// its own deadline, so there is nothing to escalate.
func (s *Server) refreshHealthSnapshotLoop(ctx context.Context, interval time.Duration) {
	s.refreshHealthSnapshot()
	tk := clock.Real().NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.refreshHealthSnapshot()
		}
	}
}

// refreshHealthSnapshot computes one snapshot from the read pool and stores
// it. A nil snapshot only ever exists before the first compute — in that
// state /api/v1/health computes inline exactly as it did before this row.
func (s *Server) refreshHealthSnapshot() {
	ctx, cancel := context.WithTimeout(context.Background(), healthRefreshQueryTimeout)
	defer cancel()
	db := s.rdb()
	snap := &healthSnapshot{
		activeTicks: countActiveTicks(ctx, db),
		stall:       stallCensus(ctx, db),
	}
	s.healthSnapshot.Store(snap)
}
