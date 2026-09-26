package api

import (
	"context"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// budgetSpendCacheTTL is how long /api/v1/projects may reuse a previously
// loaded per-project spend snapshot (SCHED-GAP-1636).
//
// Why a cache on this path: the handler used to run the full ticks aggregate
// on EVERY request — the single most expensive step of the endpoint, and the
// only one whose cost grows with the whole tick history rather than with the
// lane count (measured 137ms of a 259ms handler on a 500-lane/120k-tick
// database, vs 6ms for the projects SELECT). Because the daemon serializes
// every query through one SQLite connection (SetMaxOpenConns(1)), that scan
// also queued behind the evaluation loop and helped push the endpoint past
// its 5s handler budget, which is the row's failure mode (504 naming
// ListProjects, breaking the dogfood picker).
//
// The endpoint's consumers are dashboards and picker scripts: reads where
// seconds of staleness are irrelevant. A lane crossing its daily cap 15s late
// changes nothing, because the budget GATE (scheduler.NewBudgetGate) still
// queries fresh on every evaluation cycle — this TTL only softens what the
// READ surface reports.
const budgetSpendCacheTTL = 15 * time.Second

// budgetSpendCache is a single-entry TTL cache for a spends snapshot. It is
// safe for concurrent use; the load itself deliberately happens OUTSIDE the
// lock so a slow (or deadline-bound) query can never block another request's
// cache lookup — concurrent misses simply both run the query and the last
// writer wins, which is correct because both results describe the same
// windows.
type budgetSpendCache struct {
	mu     sync.Mutex
	at     time.Time
	spends map[string]scheduler.BudgetSpend
	valid  bool
}

// get returns the cached snapshot when one was stored within ttl of now.
// A ttl <= 0 disables caching entirely (always a miss).
func (c *budgetSpendCache) get(now time.Time, ttl time.Duration) (map[string]scheduler.BudgetSpend, bool) {
	if ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || now.Sub(c.at) >= ttl {
		return nil, false
	}
	return c.spends, true
}

// put stores spends as the snapshot valid from now. A ttl <= 0 stores
// nothing.
func (c *budgetSpendCache) put(now time.Time, spends map[string]scheduler.BudgetSpend, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.spends, c.valid = now, spends, true
}

// cacheTTL returns the effective TTL: the immutable-after-startup override
// when one was installed, else the package default.
func (s *Server) cacheTTL() time.Duration {
	if s.spendCacheTTL != nil {
		return *s.spendCacheTTL
	}
	return budgetSpendCacheTTL
}

// SetBudgetSpendCacheTTL overrides how long the /api/v1/projects spend
// snapshot may be reused (SCHED-GAP-1636). Every persistent caller installs
// nothing and takes the 15s default; the knob exists so tests and benchmarks
// can pin the cache off (0) or observe an expiry deterministically, and so an
// operator can disable caching outright if a consumer ever needs the exact
// spend on every read.
func (s *Server) SetBudgetSpendCacheTTL(d time.Duration) {
	s.spendCacheTTL = &d
}

// loadBudgetSpends returns the per-project spend snapshot for the current
// windows: the cached one when it is younger than the effective TTL, else a
// fresh scheduler.LoadBudgetSpends that is then cached.
//
// Failure is never cached — the next request retries the query — so the
// endpoint's fail-open behavior (serve the plain project rows when the spend
// query breaks) keeps its old "recovers on the next request" semantics.
func (s *Server) loadBudgetSpends(ctx context.Context, now time.Time) (map[string]scheduler.BudgetSpend, error) {
	ttl := s.cacheTTL()
	if spends, ok := s.budgetSpends.get(now, ttl); ok {
		return spends, nil
	}
	spends, err := scheduler.LoadBudgetSpends(ctx, s.db, now)
	if err != nil {
		return nil, err
	}
	s.budgetSpends.put(now, spends, ttl)
	return spends, nil
}
