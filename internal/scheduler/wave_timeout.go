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
