package scheduler

// SCHED-GAP-128 — a per-(project, finding-class) throttle for shared-infra
// alerts.
//
// THE DEFECT. GET /api/v1/events is the page an operator reads: one row per
// alert, newest first. A SHARED-INFRA finding is not a per-project event —
// port-pool exhaustion on the bunker host, a dropped config, a stale binary:
// the same condition describes every lane standing on that infra, so a
// recorder that re-runs on the health pass re-emits the identical finding on
// every pass. The operator then reads the same handful of sentences over and
// over, interleaved with the real signals the log exists to carry (measured
// live on 2026-10-19: the top duplicate class in this fleet's events table is
// a per-pass alert repeated ~59 times per project per day).
//
// The scheduler already learned this shape twice for LANE health
// (starvationThrottleWindow, SCHED-GAP-014; consecutiveFailureThrottleWindow,
// SCHED-GAP-137c): both key the throttle on the project and survive the
// per-pass escalator by reading the last emission back out of the events
// table. What was missing is the same for INFRA findings, where the unit is
// not the project alone but the (project, finding-class) tuple: port-pool
// exhaustion and a stale binary are DIFFERENT findings about the same lane,
// and each deserves its own cadence. The scheduler's own in-repo producer of
// an unthrottled shared-infra alert was CheckDuplicateWorkdirs, which
// re-emitted HIGH on every health pass for as long as the duplicate existed;
// the fleet's qa/dogfood lanes report the remaining classes as findings.
//
// THE FIX — one emission path, two levels of memory:
//
//  1. AlertEscalator.EmitInfraFinding is the single writer for shared-infra
//     findings. It stamps details.project and details.finding_class on the
//     payload, so the throttle tuple is QUERYABLE from the events table — the
//     same shape the throttle reads back on the next pass.
//  2. The window is DefaultInfraAlertThrottleWindow (1h), overridable per
//     daemon through SCHEDULER_INFRA_ALERT_THROTTLE or per instance through
//     SetInfraAlertThrottleWindow. One hour is the operator cadence: a repeat
//     inside the hour is the SAME finding, not new information.
//  3. The memory is an in-memory map keyed on the tuple (fast path — it also
//     suppresses a second identical finding inside ONE health pass) PLUS a
//     durable lookup in the events table. The escalator is constructed fresh
//     on every evaluation pass (tick_process.go), so a map alone would forget
//     between passes and the throttle would never fire in production.
//
// Nothing here changes LANE-health semantics: a starved or budget-held lane
// still escalates on its own 30-minute cadence (SCHED-GAP-014 /
// SCHED-GAP-1614). Those events describe the LANE; this file describes the
// shared INFRA it stands on.

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// DefaultInfraAlertThrottleWindow is the minimum spacing between two
// consecutive infra alerts sharing a (project, finding-class) tuple. A repeat
// inside this window is the same finding re-observed, not a new one
// (SCHED-GAP-128).
const DefaultInfraAlertThrottleWindow = time.Hour

// InfraAlertThrottleEnv overrides DefaultInfraAlertThrottleWindow for a whole
// daemon. The value is a Go duration ("90m", "2h"); an unparseable or
// non-positive value is logged and ignored so a typo can never disable the
// throttle or park it at a nonsense window.
const InfraAlertThrottleEnv = "SCHEDULER_INFRA_ALERT_THROTTLE"

// InfraAlertComponent is the events.component stamped when a caller does not
// name one — the visible marker of the shared-infra emission path.
const InfraAlertComponent = "infra"

// InfraAlertClass names one class of shared-infrastructure finding. The class
// is half of the throttle key: two findings of DIFFERENT classes about the
// same project are independent signals and are never suppressed by each
// other.
type InfraAlertClass string

const (
	// InfraAlertPortPool is shared-host port-pool exhaustion (bunker and the
	// other remote agent hosts).
	InfraAlertPortPool InfraAlertClass = "port_pool"
	// InfraAlertDroppedConfig is configuration that was present on disk and
	// is no longer (or never reached) the running component.
	InfraAlertDroppedConfig InfraAlertClass = "dropped_config"
	// InfraAlertStaleBinary is a component running a binary older than the
	// commit/config it is supposed to serve.
	InfraAlertStaleBinary InfraAlertClass = "stale_binary"
	// InfraAlertDuplicateWorkdir is two enabled lanes pointed at one workdir
	// (CheckDuplicateWorkdirs).
	InfraAlertDuplicateWorkdir InfraAlertClass = "duplicate_workdir"
	// InfraAlertUnspecified is the fallback class for a caller that supplied
	// none. It still throttles — a class-less finding repeats just as loudly.
	InfraAlertUnspecified InfraAlertClass = "unspecified"
)

// infraAlertKey renders the throttle key for a (project, finding-class) tuple.
// NUL separates the halves because it cannot occur in a project name or a
// class, so ("a", "b_c") and ("a_b", "c") can never collide.
func infraAlertKey(project string, class InfraAlertClass) string {
	return project + "\x00" + string(class)
}

// infraAlertThrottle is the in-process memory of when each tuple last
// emitted. It is not the only memory — the events table is the durable one —
// but it is the fast path and the only one that can suppress a repeat inside
// a single health pass. A nil *infraAlertThrottle is legal and degrades to
// "nothing remembered in-process"; the durable lookup still applies.
type infraAlertThrottle struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// newInfraAlertThrottle builds empty throttle memory.
func newInfraAlertThrottle() *infraAlertThrottle {
	return &infraAlertThrottle{seen: make(map[string]time.Time)}
}

// lastEmit returns when the tuple last emitted, or ok=false when this
// escalator has not seen it.
func (t *infraAlertThrottle) lastEmit(key string) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.seen[key]
	return at, ok
}

// note records that the tuple emitted at at.
func (t *infraAlertThrottle) note(key string, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[key] = at
}

// infraAlertWindowFromEnv resolves the configured window: the env override
// when it parses to a positive duration, DefaultInfraAlertThrottleWindow
// otherwise. A bad value is loud in the log and inert in behaviour, matching
// clock.FromEnv's fail-closed posture without turning a typo into a boot
// failure.
func infraAlertWindowFromEnv() time.Duration {
	raw := os.Getenv(InfraAlertThrottleEnv)
	if raw == "" {
		return DefaultInfraAlertThrottleWindow
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("INFRA-ALERT: ignoring %s=%q (want a positive Go duration) — using %v",
			InfraAlertThrottleEnv, raw, DefaultInfraAlertThrottleWindow)
		return DefaultInfraAlertThrottleWindow
	}
	return d
}

// SetInfraAlertThrottleWindow overrides the throttle window for this
// escalator (SCHED-GAP-128). Zero or negative restores
// DefaultInfraAlertThrottleWindow — there is no way to configure the throttle
// off, because "off" is the defect.
func (ae *AlertEscalator) SetInfraAlertThrottleWindow(d time.Duration) {
	if d <= 0 {
		d = DefaultInfraAlertThrottleWindow
	}
	ae.infraWindow = d
}

// infraAlertWindowOrDefault returns the window this escalator applies, never
// zero.
func (ae *AlertEscalator) infraAlertWindowOrDefault() time.Duration {
	if ae.infraWindow > 0 {
		return ae.infraWindow
	}
	return DefaultInfraAlertThrottleWindow
}

// lastInfraAlertEvent returns the created_at timestamp of the most recent
// event whose details carry this exact (project, finding_class) tuple, or
// ok=false when none exists. Parameter-bound json_extract — the same
// exact-match discipline as lastStarvationEvent, so similarly-named projects
// never share a throttle bucket.
func (ae *AlertEscalator) lastInfraAlertEvent(ctx context.Context, project string, class InfraAlertClass) (time.Time, bool) {
	if ae.db == nil {
		return time.Time{}, false
	}
	var ts string
	err := ae.db.QueryRowContext(ctx,
		`SELECT created_at FROM events
		 WHERE json_extract(details, '$.project') = ? AND json_extract(details, '$.finding_class') = ?
		 ORDER BY created_at DESC LIMIT 1`,
		project, string(class)).Scan(&ts)
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// infraAlertDue reports whether a finding for this tuple may emit now: false
// when either the in-process map or the durable events table holds an
// emission inside the window. When the durable lookup is what suppressed the
// emission, the timestamp is cached in the map so later findings in the same
// pass take the fast path.
func (ae *AlertEscalator) infraAlertDue(ctx context.Context, project string, class InfraAlertClass, now time.Time) bool {
	window := ae.infraAlertWindowOrDefault()
	key := infraAlertKey(project, class)
	if last, ok := ae.infraThrottle.lastEmit(key); ok && now.Sub(last) < window {
		return false
	}
	if last, ok := ae.lastInfraAlertEvent(ctx, project, class); ok && now.Sub(last) < window {
		ae.infraThrottle.note(key, last)
		return false
	}
	return true
}

// EmitInfraFinding writes ONE shared-infrastructure finding, throttled per
// (project, finding-class) to at most one event per infraAlertWindowOrDefault
// (SCHED-GAP-128).
//
// It is the single emission path for infra classes: the payload always
// carries details["project"] and details["finding_class"] (copied over the
// caller's details, which are never mutated), which is both what an operator
// filters GET /api/v1/events by and what the throttle reads back on the next
// pass. An empty class is stamped InfraAlertUnspecified; an empty component
// is stamped InfraAlertComponent.
//
// Returns true when the event was written, false when it was suppressed (or
// when the escalator has no event logger, in which case there is nothing to
// write through).
func (ae *AlertEscalator) EmitInfraFinding(ctx context.Context, severity EventSeverity, component, project string, class InfraAlertClass, message string, details map[string]any) bool {
	if ae == nil || ae.events == nil {
		return false
	}
	if class == "" {
		class = InfraAlertUnspecified
	}
	if component == "" {
		component = InfraAlertComponent
	}
	now := ae.clock().Now()
	if !ae.infraAlertDue(ctx, project, class, now) {
		log.Printf("INFRA-ALERT: suppressed repeat %s/%s (window %v)", project, class, ae.infraAlertWindowOrDefault())
		return false
	}
	payload := make(map[string]any, len(details)+2)
	for k, v := range details {
		payload[k] = v
	}
	payload["project"] = project
	payload["finding_class"] = string(class)
	ae.events.Emit(ctx, severity, component, message, payload)
	ae.infraThrottle.note(infraAlertKey(project, class), now)
	return true
}
