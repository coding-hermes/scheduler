package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sync"
)

// SCHED-GAP-125 — load-average admission gate (Bane 2026-09-16):
//
//	"we might want to add another gate that people can decide to enable but
//	 to also use the load average to decide if they should run or not — for
//	 example we are a 16 core machine so if the load average is below 12
//	 maybe that is a good time to be running things so we don't overload
//	 the machine"
//
// Design law applied (zero hardcoding, Bane's standing rule):
//   - The threshold is CONFIG, not code: --load-gate-threshold flag
//     (env SCHEDULER_LOAD_GATE_THRESHOLD, TOML [scheduler] load_gate_threshold).
//     0 = disabled (the default — fleet behavior is byte-identical when unset).
//   - Namespaces can OPT OUT via load_gate='off' (migration v31): an
//     always-on infra lane keeps ticking even under load.
//   - The gate lives in SlotPool.spawn (the G7 ruling placement for
//     admission gates) and DEFERS, it does not cancel: the project stays
//     selected, gets re-picked on the next evaluation once load drops.
//     No cooldown damage, no tick consumed, no progress penalty.
//   - Sampled fresh at decision time (x/sys cpu load, 1-minute Linux
//     loadavg decay — the same number uptime prints).

var (
	loadThreshold float64
	loadMu        sync.RWMutex
)

// SetLoadGateThreshold installs the global threshold (0 disables the gate).
// Called once from main/loop wiring; last writer wins.
func SetLoadGateThreshold(threshold float64) {
	loadMu.Lock()
	defer loadMu.Unlock()
	loadThreshold = threshold
}

// loadGateThreshold returns the active threshold (0 = disabled).
func loadGateThreshold() float64 {
	loadMu.RLock()
	defer loadMu.RUnlock()
	return loadThreshold
}

// currentLoad1m returns the 1-minute load average; ok=false when the
// platform provides no reading (gate fails open on missing telemetry).
// The sampler is PLATFORM-SPLIT (SCHED-GAP-201): the real reading is
// Linux-only (unix.Sysinfo + SI_LOAD_SHIFT) and lives in
// load_gate_linux.go (//go:build linux); non-Linux builds get the
// always-missing stub in load_gate_other.go (//go:build !linux) —
// see that file for the gate-inactive consequence. Var (not func) so
// tests can inject a deterministic sampler on every platform.

// loadGateActive reports whether the gate is enabled AND the current load
// average is at or above the threshold (i.e. new spawns should defer).
// Missing telemetry → fail open (never block on absent data).
func loadGateActive() bool {
	t := loadGateThreshold()
	if t <= 0 {
		return false
	}
	l1, ok := currentLoad1m()
	if !ok {
		return false
	}
	return l1 >= t
}

// namespaceLoadGateOff reports whether the given namespace opted out
// (load_gate='off'). Unknown namespace / ” / DB error → false (gate applies).
func namespaceLoadGateOff(db *sql.DB, nsID string) bool {
	if db == nil || nsID == "" {
		return false
	}
	var mode string
	err := db.QueryRowContext(context.Background(),
		`SELECT load_gate FROM namespaces WHERE id = ?`, nsID).Scan(&mode)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("LOAD-GATE: namespace lookup %s: %v (fail open — gate applies)", nsID, err)
		}
		return false
	}
	return mode == "off"
}

// LoadGateShouldDefer is the single decision point called from
// SlotPool.spawn. True = defer this spawn (work stays queued, re-picked
// on the next evaluation when load drops below threshold).
func LoadGateShouldDefer(db *sql.DB, nsID string) bool {
	if !loadGateActive() {
		return false
	}
	return !namespaceLoadGateOff(db, nsID)
}
