package scheduler

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §4.3 (SCHED-GAP-111): effective tick deadline ────────────────────
//
// DECISION 1: the tick wall-clock deadline for a wave-enabled namespace is a
// STATIC, namespace-scoped override of --tick-timeout. It is resolved in ONE
// place — this resolver, called from Spawn() — and never extended at runtime
// (no heartbeat lease, no mid-tick renegotiation). Every failure path falls
// back to the base --tick-timeout, so a fleet with waves off is byte-identical
// to pre-S12 behavior (spec §15).
//
// Resolution order for a project whose namespace has wave_enabled=1:
//
//	1. SCHEDULER_WAVE_TICK_TIMEOUT env (when set and parseable)
//	2. namespaces.wave_tick_timeout (when non-empty and parseable)
//	3. s.timeout (--tick-timeout)
//
// An unparseable env value logs a WARN and falls back to the namespace value —
// never panics, never silently zeroes the deadline. An unparseable namespace
// value is impossible from config-managed fleets (LoadFleetConfig/Validate
// reject it), but rows written by hand or via the API are tolerated the same
// way: WARN + inherit.
//
// Recommended value 3h (1.5x base: workers run concurrently, only merges are
// serial); hard ceiling 4h enforced at config validation (S12 §4.3 item 2).

// waveTickTimeoutCeiling mirrors the config-layer 4h ceiling
// (validateWaveTickTimeout in internal/config). It is a SECOND line of
// defense here, not the primary gate: config is the authority, this bound
// keeps a hand-edited DB row or an operator env override from granting a
// tick-immortality knob (S12 §13). NOT enforced against s.timeout itself —
// --tick-timeout > 4h is explicitly out of scope (the brief and §4.3).
const waveTickTimeoutCeiling = 4 * time.Hour

// envWaveTickTimeout is the env override for wave-enabled namespaces. Read
// per-call (not cached at spawner construction) so tests can flip it with
// t.Setenv; the scheduler process runs one resolution per spawn, so the
// getenv cost is negligible next to the gateway HTTP call it bounds.
const envWaveTickTimeout = "SCHEDULER_WAVE_TICK_TIMEOUT"

// nsWaveRow is the single-indexed-lookup shape used by waveNamespace: the
// two wave columns the resolver needs, nothing else.
type nsWaveRow struct {
	waveEnabled bool
	waveTimeout string
}

// waveNamespace loads the wave columns for the project's namespace. Returns
// ok=false (with no error) for unknown/NULL namespaces and logs nothing —
// those are normal states (unnamespaced projects, flat mode) and the caller
// inherits the base timeout silently. A DB error is logged and also inherits:
// the spawn path must never die because wave resolution failed (spec §15:
// every failure path falls back to --tick-timeout).
//
// Perf note (spec §14 "Serial ticks byte-identical"): this is ONE indexed
// SELECT (namespaces.id is the PRIMARY KEY) executed once per spawn, only
// when the project carries a namespace id. There is no per-tick query storm:
// spawn frequency is bounded by max_concurrent and cooldowns, and a spawn
// already performs several writes (ticks row, projects.last_tick_started,
// heartbeat). No namespace cache exists on the spawner today and the packer's
// namespace list is per-evaluation, not shareable at spawn time — a single
// PK lookup at spawn time is the cheapest correct option.
func (s *Spawner) waveNamespace(ctx context.Context, namespaceID string) (nsWaveRow, bool) {
	if namespaceID == "" {
		return nsWaveRow{}, false
	}
	var enabled int
	var waveTimeout string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(wave_enabled, 0), COALESCE(wave_tick_timeout, '') FROM namespaces WHERE id = ?`,
		namespaceID).Scan(&enabled, &waveTimeout)
	if err == sql.ErrNoRows {
		return nsWaveRow{}, false
	}
	if err != nil {
		log.Printf("WARN: wave timeout lookup for namespace %q failed (%v) — inheriting --tick-timeout", namespaceID, err)
		return nsWaveRow{}, false
	}
	return nsWaveRow{waveEnabled: enabled != 0, waveTimeout: waveTimeout}, true
}

// effectiveTickTimeout returns the deadline Spawn() must apply to the foreman
// session it is about to start (the ONE resolution site, S12 §4.3 item 2).
//
// The returned duration is already clamped to waveTickTimeoutCeiling: a
// wave-enabled namespace whose resolved override exceeds 4h (possible only
// via env or a hand-edited row — config validation rejects it) gets 4h, not
// an immortal tick. A non-wave resolution (base --tick-timeout) is returned
// untouched.
func (s *Spawner) effectiveTickTimeout(project PackedProject) time.Duration {
	ns, ok := s.waveNamespace(context.Background(), project.NamespaceID)
	if !ok || !ns.waveEnabled {
		return s.timeout
	}

	// Layer 1: env override. Set → wins over the namespace value (operator
	// knob for incident response). Unparseable → WARN + fall through to the
	// namespace value, never a zero deadline.
	if v := os.Getenv(envWaveTickTimeout); v != "" {
		d, perr := time.ParseDuration(v)
		if perr == nil {
			return clampWaveTimeout(d, "env "+envWaveTickTimeout)
		}
		log.Printf("WARN: %s=%q unparseable (%v) — falling back to namespace wave_tick_timeout", envWaveTickTimeout, v, perr)
	}

	// Layer 2: namespace wave_tick_timeout. Empty → inherit (spec §15:
	// wave_enabled without a timeout still inherits --tick-timeout).
	// Unparseable (hand-edited row) → WARN + inherit, same as a lookup miss.
	if ns.waveTimeout != "" {
		if d, err := time.ParseDuration(ns.waveTimeout); err == nil {
			return clampWaveTimeout(d, "namespace wave_tick_timeout "+ns.waveTimeout)
		}
		log.Printf("WARN: namespace %q wave_tick_timeout=%q unparseable — inheriting --tick-timeout", project.NamespaceID, ns.waveTimeout)
	}
	return s.timeout
}

// clampWaveTimeout bounds a resolved WAVE override to the 4h ceiling. Base
// --tick-timeout is never passed here and never clamped.
func clampWaveTimeout(d time.Duration, source string) time.Duration {
	if d > waveTickTimeoutCeiling {
		log.Printf("WARN: wave tick timeout %v from %s exceeds the 4h ceiling — clamped to %v (S12 §4.3)", d, source, waveTickTimeoutCeiling)
		return waveTickTimeoutCeiling
	}
	return d
}

// nsIDOf renders a database.Project's optional namespace id as a plain
// string for PackedProject.NamespaceID ("" when unassigned).
func nsIDOf(p database.Project) string {
	if p.NamespaceID == nil {
		return ""
	}
	return *p.NamespaceID
}

// ── SCHED-GAP-217: per-eval stale-tick backstop derived from the live
// effective tick deadline ────────────────────────────────────────────────
//
// HISTORY. tick_process.go's per-evaluation CleanupStaleProjects call passed a
// hardcoded 90 * time.Minute. That pre-dated SCHED-GAP-111 (S12 §4.3), which
// let wave-enabled namespaces extend --tick-timeout to 3h (env > ns > flag,
// ceiling 4h). A 90m backstop on a 3h tick is a 2x-earlier kill: live ticks
// were reaped with error="stale - timeout at 1h30m0s" while the in-process
// deadline ctx (spawn.go:1193) was still 1.5h away. The phantom timeouts fed
// the failure-rate counters, the auto-disable window, and the board foreman
// view — indistinguishable from real failures.
//
// DERIVATION. The backstop is the MAX effective tick deadline across the
// in-flight running ticks (join ticks→projects→namespaces, apply the same
// env > ns > --tick-timeout cascade as Spawner.effectiveTickTimeout), plus a
// documented grace margin (backstopGrace = 30m). If no running ticks exist
// the backstop falls back to spawner.timeout + backstopGrace, so an empty
// fleet never uses a backstop smaller than the configured base deadline.
//
// FLOOR. The return value is always >= backstopFloor (90m). This matches
// the pre-fix value byte-for-byte as a hard floor, so a project whose
// configured deadline is shorter than 1h30m (a unit-test fixture) still gets
// a sane backstop and the prior 1h-arg TestLifecycle_CleanupStale continues
// to flip a 2h tick — the regression guard is unchanged.
//
// SCOPE. backstopMaxAge is a function on the package (not a method on
// Spawner) because the call site is the Loop in tick_process.go; reading
// l.spawner.timeout is the same shape Spawner.effectiveTickTimeout uses.
const (
	// backstopGrace is the extra time the reaper waits BEYOND the longest
	// in-flight effective deadline before it considers a tick wedged. Sized
	// to absorb a normal in-spawn goroutine lag (a few hundred ms) and the
	// slow-path of a foreman's last tool call (a few seconds) with a wide
	// safety margin; tested in TestBackstopMaxAge_derivesFromInFlight.
	backstopGrace = 30 * time.Minute

	// backstopFloor is the smallest value backstopMaxAge ever returns. The
	// pre-fix code passed 90*time.Minute, so this floor preserves the
	// byte-equivalent worst case — important for the existing
	// lifecycle_test.go's TestLifecycle_CleanupStale which exercises a 1h
	// window against a 2h-old tick.
	backstopFloor = 90 * time.Minute
)

// backstopMaxAge returns the reaper cutoff for CleanupStaleProjects: the
// oldest age at which a running tick is still considered alive. Derived from
// the live effective tick deadline so it can never precede the deadline it
// backs up (SCHED-GAP-217).
//
// ALGORITHM:
//
//  1. SELECT the distinct namespace_ids of currently-running ticks via a
//     ticks→projects join (ticks carries no namespace_id column; the join
//     resolves it through the project's current namespace assignment).
//  2. For each non-empty namespace_id, look up wave_enabled / wave_tick_timeout
//     in one query per namespace and apply the env > ns > --tick-timeout
//     cascade — same resolution as Spawner.effectiveTickTimeout. A wave-off
//     namespace (or unnamespaced tick) inherits s.timeout.
//  3. Take the MAX across the per-tick effective deadlines.
//  4. Add backstopGrace. Apply the backstopFloor. Return.
//
// FAILURE PATHS.
//
//   - DB error reading running ticks: return s.timeout + backstopGrace +
//     (worst case the backstop is conservative, never aggressive).
//   - No running ticks: same fallback (s.timeout + backstopGrace).
//   - Unparseable wave_tick_timeout: WARN + inherit s.timeout for that
//     namespace (matches the Spawner contract).
//   - Env override unparseable: WARN + fall through to the namespace value
//     (same as Spawner).
//
// COST. One SELECT for the running-tick/namespace join (idx_ticks_status),
// then at most one SELECT per DISTINCT namespace id (namespaces.id is the
// PRIMARY KEY). For a saturated fleet of ~30 namespaces, the total is
// O(running) + O(distinct namespaces) round-trips per evaluation, each
// evaluation is a single OS-thread tick at ~30s cadence — well under 1ms
// aggregate on a healthy DB. No new indexes are required.
func (l *Loop) backstopMaxAge() time.Duration {
	if l == nil || l.spawner == nil {
		// Defensive: a Loop without a Spawner (test-only shape) gets the
		// floored conservative value. Never the zero duration.
		return backstopFloor
	}
	base := l.spawner.timeout
	graceBase := base + backstopGrace

	rows, err := l.db.Query(`
		SELECT DISTINCT p.namespace_id
		FROM ticks t
		JOIN projects p ON p.name = t.project_name
		WHERE t.status = ? AND p.namespace_id IS NOT NULL AND p.namespace_id <> ''
	`, TickRunning)
	if err != nil {
		log.Printf("WARN: backstopMaxAge: tick→namespace join failed (%v) — falling back to %v", err, graceBase)
		return maxDuration(graceBase, backstopFloor)
	}
	// Drain rows immediately: the test DB holds a single connection
	// (SetMaxOpenConns(1) in newTestDB / gap060), so a second QueryRow
	// inside the loop would deadlock on itself. Collect, then close, then
	// look up — the per-namespace resolution is a fresh conn-acquire.
	var namespaceIDs []string
	for rows.Next() {
		var nsID string
		if err := rows.Scan(&nsID); err != nil {
			continue
		}
		namespaceIDs = append(namespaceIDs, nsID)
	}
	rows.Close()

	var maxEffective time.Duration
	seen := make(map[string]bool) // dedupe namespaces (DISTINCT applies to NULL handling)
	for _, nsID := range namespaceIDs {
		if seen[nsID] {
			continue
		}
		seen[nsID] = true
		if d, ok := l.resolveNamespaceDeadline(nsID); ok && d > maxEffective {
			maxEffective = d
		}
	}
	if maxEffective == 0 {
		// No running ticks OR all running ticks are unnamespaced: use the
		// configured base timeout + grace. Same value either way.
		return maxDuration(graceBase, backstopFloor)
	}
	return maxDuration(maxEffective+backstopGrace, backstopFloor)
}

// resolveNamespaceDeadline returns the effective tick deadline for a single
// namespace id, applying the same env > ns > --tick-timeout cascade as
// Spawner.effectiveTickTimeout. ok=false means "unnamespaced / not found /
// wave-off" — caller inherits s.timeout in that case.
func (l *Loop) resolveNamespaceDeadline(namespaceID string) (time.Duration, bool) {
	if l == nil || l.spawner == nil {
		return 0, false
	}
	var enabled int
	var waveTimeout string
	err := l.db.QueryRow(
		`SELECT COALESCE(wave_enabled, 0), COALESCE(wave_tick_timeout, '') FROM namespaces WHERE id = ?`,
		namespaceID).Scan(&enabled, &waveTimeout)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		log.Printf("WARN: backstopMaxAge: namespace %q lookup failed (%v) — inheriting --tick-timeout", namespaceID, err)
		return 0, false
	}
	if enabled == 0 {
		// Wave-off namespace: inherits the base --tick-timeout, so its
		// effective deadline is the same as an unnamespaced tick. We
		// return ok=false so the caller's max() math treats it as
		// "nothing bigger than base", and the fallback at the call site
		// (graceBase) covers it.
		return 0, false
	}

	// Layer 1: env override (matches Spawner.effectiveTickTimeout).
	if v := os.Getenv(envWaveTickTimeout); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			return clampWaveTimeout(d, "env "+envWaveTickTimeout), true
		}
		log.Printf("WARN: %s=%q unparseable (%v) — falling back to namespace wave_tick_timeout", envWaveTickTimeout, v, err)
	}
	// Layer 2: namespace wave_tick_timeout.
	if waveTimeout != "" {
		if d, perr := time.ParseDuration(waveTimeout); perr == nil {
			return clampWaveTimeout(d, "namespace wave_tick_timeout "+waveTimeout), true
		}
		log.Printf("WARN: namespace %q wave_tick_timeout=%q unparseable — inheriting --tick-timeout", namespaceID, waveTimeout)
	}
	return 0, false
}

// maxDuration is a tiny helper to avoid importing "math" just for one max.
// Returns the larger of a and b.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
