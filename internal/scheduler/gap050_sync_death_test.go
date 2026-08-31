package scheduler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// ── GAP-050 (P1): silent fleet-wide sync death on gateway state.db corruption ──
//
// When the gateway's state.db corrupts, every spawn fails with a gateway error
// and — with --no-exec-fallback (the default) — the tick is DROPPED. Before
// this fix the drop was silent: a log line + consecutive_failures bump, no
// HIGH event, no alert, so fleet-wide sync death went unnoticed. These tests
// pin the GAP-050 contract:
//
//  1. every gateway-failed spawn drop emits EXACTLY one HIGH spawn event with
//     details {project, tick_id, error} (query pattern from
//     board_closure_test.go / schedgap079_test.go);
//  2. a per-project consecutive-drop counter increments on each drop, resets
//     to 0 on the next successful spawn, and fires a distinct alert HIGH
//     event at exactly 2 consecutive drops (never re-firing beyond the
//     threshold until a reset);
//  3. transient gateway failures (ErrGatewayTransient, HTTP 5xx,
//     network/timeout/read errors) count as drops;
//  4. auth rejections (ErrGatewayKeyRejected / 401/403) keep the GAP-035
//     behavior and NEVER touch the counter.

// gap050CorruptResponse writes a 2xx response whose error envelope mimics the
// gateway state.db corruption shape. gateway_client.go classifies it as a
// plain gateway error (NOT auth, NOT transient) — so Spawn() drops the tick
// with NO retry: fast, deterministic drops for the counter tests. The error
// text carries the real-world corruption signature.
func gap050CorruptResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "server_error",
			"message": "Internal server error: database disk image is malformed",
		},
	})
}

// gap050AuthResponse writes a 401 with the gateway's auth_error envelope
// (GAP-035 terminal classification).
func gap050AuthResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "auth_error",
			"message": "Invalid gateway API key",
		},
	})
}

// gap050DropEventCount counts per-drop HIGH spawn events for a project —
// message 'gateway spawn dropped' only, so GAP-035 'gateway key rejected'
// events (which carry project+tick_id in details too) never pollute it.
func gap050DropEventCount(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='spawn' AND message='gateway spawn dropped' AND json_extract(details, '$.project') = ?`, project).Scan(&n); err != nil {
		t.Fatalf("count gap050 drop events: %v", err)
	}
	return n
}

// gap050AlertCount counts GAP-050 consecutive-drop alert events for a project
// (distinct message naming the project + drop count).
func gap050AlertCount(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='spawn' AND message LIKE 'gateway consecutive spawn drops%' AND json_extract(details, '$.project') = ?`, project).Scan(&n); err != nil {
		t.Fatalf("count gap050 alerts: %v", err)
	}
	return n
}

// gap050DropCount reads the spawner's in-memory per-project consecutive-drop
// counter (guarded by the spawner mutex, same as the production accessors).
func gap050DropCount(spawner *Spawner, project string) int {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()
	return spawner.consecutiveGatewayDrops[project]
}

// TestGap050_SingleDropEmitsOneHighEventCounterOneNoAlert — acceptance
// scenario (1): one gateway-failed drop emits exactly one HIGH spawn event
// whose details carry project + tick_id + the gateway error text (the AC1
// query pattern from board_closure_test.go), the counter sits at 1, and NO
// alert fires.
func TestGap050_SingleDropEmitsOneHighEventCounterOneNoAlert(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap050-single"
		tickID      = "gap050-single-2026-08-31-10-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := schedGap079Spawner(t, db, gap050CorruptResponse)
	spawner.SetEventLogger(NewEventLogger(db))

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — a corrupted gateway must drop the tick")
	}
	if !strings.Contains(err.Error(), "database disk image is malformed") {
		t.Errorf("error = %q, want the gateway corruption text verbatim", err.Error())
	}

	// AC1: exactly 1 HIGH spawn event for this project+tick_id.
	if n := schedGap079HighEventCount(t, db, projectName, tickID); n != 1 {
		t.Errorf("HIGH spawn events for %s/%s = %d, want exactly 1 per dropped tick", projectName, tickID, n)
	}
	if n := gap050DropEventCount(t, db, projectName); n != 1 {
		t.Errorf("'gateway spawn dropped' events = %d, want 1", n)
	}
	// Counter advanced to 1, no alert below the threshold.
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("consecutive drop counter = %d, want 1", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts = %d, want 0 — a single drop must not alert", n)
	}
}

// TestGap050_TwoConsecutiveDropsFireAlert — acceptance scenario (2): the
// second consecutive drop fires the alert (counter >= 2), the alert message
// names the project and the drop count, and it is the ONLY extra event — each
// dropped tick still has exactly 1 HIGH spawn event (the alert carries no
// tick_id, keeping the AC1 per-tick count at 1). A third drop must NOT
// re-fire the alert (no re-fire beyond the threshold until a reset).
func TestGap050_TwoConsecutiveDropsFireAlert(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap050-double"
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := schedGap079Spawner(t, db, gap050CorruptResponse)
	spawner.SetEventLogger(NewEventLogger(db))

	tick1 := projectName + "-2026-08-31-10-01-00"
	tick2 := projectName + "-2026-08-31-10-02-00"
	tick3 := projectName + "-2026-08-31-10-03-00"

	// Drop 1: counter 1, no alert.
	if _, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tick1); err == nil {
		t.Fatal("Spawn 1 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("counter after drop 1 = %d, want 1", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts after drop 1 = %d, want 0", n)
	}

	// Drop 2: counter 2, alert fires exactly once.
	if _, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tick2); err == nil {
		t.Fatal("Spawn 2 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 2 {
		t.Errorf("counter after drop 2 = %d, want 2", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 1 {
		t.Errorf("alerts after drop 2 = %d, want 1 (alert fires at >=2 consecutive drops)", n)
	}

	// The alert is a distinct message naming project + count.
	var alertMsg, alertDetails string
	if err := db.QueryRow(`SELECT message, details FROM events WHERE severity='HIGH' AND component='spawn' AND message LIKE 'gateway consecutive spawn drops%'`).Scan(&alertMsg, &alertDetails); err != nil {
		t.Fatalf("read alert event: %v", err)
	}
	if !strings.Contains(alertMsg, projectName) {
		t.Errorf("alert message = %q, want it to name the project", alertMsg)
	}
	if !strings.Contains(alertMsg, "2") {
		t.Errorf("alert message = %q, want it to name the drop count", alertMsg)
	}
	var alertDetailsObj map[string]any
	if err := json.Unmarshal([]byte(alertDetails), &alertDetailsObj); err != nil {
		t.Fatalf("alert details not JSON: %v", err)
	}
	if got := alertDetailsObj["count"]; got != float64(2) {
		t.Errorf("alert details count = %v, want 2", got)
	}

	// AC1 still holds on the alert tick: exactly 1 HIGH spawn event per
	// project+tick_id — the alert deliberately carries no tick_id.
	if n := schedGap079HighEventCount(t, db, projectName, tick1); n != 1 {
		t.Errorf("HIGH events for tick1 = %d, want 1", n)
	}
	if n := schedGap079HighEventCount(t, db, projectName, tick2); n != 1 {
		t.Errorf("HIGH events for tick2 = %d, want 1 — the alert must not double-count a dropped tick", n)
	}

	// Drop 3: counter 3, NO re-fire beyond the threshold until a reset.
	if _, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tick3); err == nil {
		t.Fatal("Spawn 3 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 3 {
		t.Errorf("counter after drop 3 = %d, want 3", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 1 {
		t.Errorf("alerts after drop 3 = %d, want still 1 — no re-fire beyond the threshold until a reset", n)
	}

	// Per-project isolation: an untouched project's counter stays 0.
	if got := gap050DropCount(spawner, "gap050-untouched"); got != 0 {
		t.Errorf("untouched project counter = %d, want 0 — the counter is per-project", got)
	}
}

// TestGap050_DropSuccessDropResetsCounterAlertOnlyAfterTwoConsecutive —
// acceptance scenario (3): a successful spawn between drops resets the
// counter to 0, so the alert fires only after TWO CONSECUTIVE drops following
// the reset (drop-success-drop leaves counter=1 with no alert; the next drop
// reaches 2 and alerts).
func TestGap050_DropSuccessDropResetsCounterAlertOnlyAfterTwoConsecutive(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap050-reset"
	mustCreateProjectINFRA012(t, db, projectName)

	// Request 1 = drop, request 2 = success, requests 3+ = drop again.
	var requests int32
	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 2 {
			schedGap080CompletedResponse(w)
			return
		}
		gap050CorruptResponse(w, r)
	})
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: projectName, Workdir: t.TempDir()}
	tick1 := projectName + "-2026-08-31-10-04-00"
	tick2 := projectName + "-2026-08-31-10-05-00"
	tick3 := projectName + "-2026-08-31-10-06-00"
	tick4 := projectName + "-2026-08-31-10-07-00"

	// Drop 1.
	if _, err := spawner.Spawn(project, tick1); err == nil {
		t.Fatal("Spawn 1 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("counter after drop 1 = %d, want 1", got)
	}

	// Success: spawn completes, counter resets to 0.
	tick, err := spawner.Spawn(project, tick2)
	if err != nil {
		t.Fatalf("Spawn 2 (success): %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn 2 returned nil tick")
	}
	if outcome := tick.Wait(); outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	if got := gap050DropCount(spawner, projectName); got != 0 {
		t.Errorf("counter after success = %d, want 0 — a successful spawn must reset the counter", got)
	}

	// Drop 3: counter back to 1, no alert (only ONE consecutive drop since
	// the reset).
	if _, err := spawner.Spawn(project, tick3); err == nil {
		t.Fatal("Spawn 3 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("counter after drop 3 = %d, want 1", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts = %d, want 0 — drop-success-drop must not alert (not consecutive)", n)
	}

	// Drop 4: second consecutive drop since the reset — alert fires.
	if _, err := spawner.Spawn(project, tick4); err == nil {
		t.Fatal("Spawn 4 returned nil error")
	}
	if got := gap050DropCount(spawner, projectName); got != 2 {
		t.Errorf("counter after drop 4 = %d, want 2", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 1 {
		t.Errorf("alerts = %d, want 1 — two consecutive drops after the reset must alert", n)
	}
}

// TestGap050_AuthRejectionDoesNotTouchCounter — acceptance scenario (4): a
// 401 keeps the GAP-035 terminal path (ErrGatewayKeyRejected, 'gateway key
// rejected' HIGH event) and NEVER increments the consecutive-drop counter nor
// emits a 'gateway spawn dropped' event.
func TestGap050_AuthRejectionDoesNotTouchCounter(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap050-auth"
		tickID      = "gap050-auth-2026-08-31-10-08-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := schedGap079Spawner(t, db, gap050AuthResponse)
	spawner.SetEventLogger(NewEventLogger(db))

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error on HTTP 401")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("errors.Is(err, ErrGatewayKeyRejected) = false, want true (GAP-035 terminal classification preserved)")
	}

	// Counter untouched by auth rejections.
	if got := gap050DropCount(spawner, projectName); got != 0 {
		t.Errorf("consecutive drop counter = %d, want 0 — auth rejections must never increment it", got)
	}
	// No generic drop events, no alerts — the GAP-035 event is the only one.
	if n := gap050DropEventCount(t, db, projectName); n != 0 {
		t.Errorf("'gateway spawn dropped' events = %d, want 0 for an auth rejection", n)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts = %d, want 0 for an auth rejection", n)
	}
	// The GAP-035 HIGH event still fires (existing behavior untouched).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='spawn' AND message='gateway key rejected' AND json_extract(details, '$.project') = ?`, projectName).Scan(&n); err != nil {
		t.Fatalf("count gateway key rejected events: %v", err)
	}
	if n != 1 {
		t.Errorf("'gateway key rejected' events = %d, want 1 (GAP-035 behavior intact)", n)
	}
}

// TestGap050_EventShapeMatchesBoardClosureQueryPattern — acceptance scenario
// (5): the per-drop event's shape (severity HIGH, component 'spawn', details
// with project + tick_id + error) is queryable with the exact query pattern
// from board_closure_test.go, and the error text is the gateway's own.
func TestGap050_EventShapeMatchesBoardClosureQueryPattern(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap050-shape"
		tickID      = "gap050-shape-2026-08-31-10-09-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := schedGap079Spawner(t, db, gap050CorruptResponse)
	spawner.SetEventLogger(NewEventLogger(db))

	if _, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID); err == nil {
		t.Fatal("Spawn returned nil error")
	}

	// board_closure_test.go:229 pattern, component='spawn': exactly 1 row.
	var severity, component, message, details string
	err := db.QueryRow(`SELECT severity, component, message, details FROM events WHERE severity='HIGH' AND component='spawn' AND json_extract(details, '$.project') = ? AND json_extract(details, '$.tick_id') = ?`, projectName, tickID).Scan(&severity, &component, &message, &details)
	if err != nil {
		t.Fatalf("query drop event: %v", err)
	}
	if severity != string(SeverityHigh) {
		t.Errorf("severity = %q, want %q", severity, SeverityHigh)
	}
	if component != "spawn" {
		t.Errorf("component = %q, want 'spawn'", component)
	}
	if message != "gateway spawn dropped" {
		t.Errorf("message = %q, want 'gateway spawn dropped'", message)
	}
	var det map[string]any
	if err := json.Unmarshal([]byte(details), &det); err != nil {
		t.Fatalf("details not JSON: %v", err)
	}
	if det["project"] != projectName {
		t.Errorf("details.project = %v, want %q", det["project"], projectName)
	}
	if det["tick_id"] != tickID {
		t.Errorf("details.tick_id = %v, want %q", det["tick_id"], tickID)
	}
	errText, _ := det["error"].(string)
	if !strings.Contains(errText, "database disk image is malformed") {
		t.Errorf("details.error = %q, want the gateway error text verbatim", errText)
	}
}

// TestGap050_TransientHTTP500CountsAsDrop — acceptance criterion (3): a
// persistent HTTP 5xx is a transient gateway failure — after the bounded
// SCHED-GAP-080 retries are exhausted the tick is dropped, the drop counter
// increments, and exactly one HIGH event carries the 5xx text.
func TestGap050_TransientHTTP500CountsAsDrop(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap050-5xx"
		tickID      = "gap050-5xx-2026-08-31-10-10-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		schedGap080ServerError(w)
	})
	spawner.SetEventLogger(NewEventLogger(db))

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — a persistently-5xx gateway must drop the tick")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to carry 'HTTP 500'", err.Error())
	}
	// All 4 POSTs (1 initial + 3 retries) were transient — the SCHED-GAP-080
	// counter reflects them AND the GAP-050 drop counter counts the drop.
	if got := spawner.GatewayErrorCount(); got != 1+gatewayRetryMaxAttempts {
		t.Errorf("GatewayErrorCount() = %d, want %d", got, 1+gatewayRetryMaxAttempts)
	}
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("consecutive drop counter = %d, want 1 — a 5xx drop counts", got)
	}
	if n := schedGap079HighEventCount(t, db, projectName, tickID); n != 1 {
		t.Errorf("HIGH spawn events = %d, want exactly 1 for the 5xx drop", n)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts = %d, want 0 for a single 5xx drop", n)
	}
}

// TestGap050_NilGatewayDropCountsTowardAlert — the GAP-048 nil-gateway path
// (no gateway client + exec fallback disabled) is a gateway-caused drop too:
// each tick emits its existing 'gateway unavailable' HIGH event (exactly one
// per dropped tick), and the consecutive-drop counter still alerts at 2.
func TestGap050_NilGatewayDropCountsTowardAlert(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap050-nilgw"
	mustCreateProjectINFRA012(t, db, projectName)

	spawner := NewSpawner(db, 4)
	spawner.SetNoExecFallback(true) // no SetGatewayClient — gateway stays nil
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: projectName, Workdir: t.TempDir()}
	tick1 := projectName + "-2026-08-31-10-11-00"
	tick2 := projectName + "-2026-08-31-10-12-00"

	if _, err := spawner.Spawn(project, tick1); err == nil {
		t.Fatal("Spawn 1 returned nil error with nil gateway + noExecFallback")
	}
	if got := gap050DropCount(spawner, projectName); got != 1 {
		t.Errorf("counter after nil-gateway drop 1 = %d, want 1", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 0 {
		t.Errorf("alerts = %d, want 0 after a single nil-gateway drop", n)
	}

	if _, err := spawner.Spawn(project, tick2); err == nil {
		t.Fatal("Spawn 2 returned nil error with nil gateway + noExecFallback")
	}
	if got := gap050DropCount(spawner, projectName); got != 2 {
		t.Errorf("counter after nil-gateway drop 2 = %d, want 2", got)
	}
	if n := gap050AlertCount(t, db, projectName); n != 1 {
		t.Errorf("alerts = %d, want 1 — two consecutive nil-gateway drops must alert", n)
	}
	// Exactly one HIGH event per dropped tick on this path too.
	if n := schedGap079HighEventCount(t, db, projectName, tick1); n != 1 {
		t.Errorf("HIGH events for tick1 = %d, want 1", n)
	}
	if n := schedGap079HighEventCount(t, db, projectName, tick2); n != 1 {
		t.Errorf("HIGH events for tick2 = %d, want 1", n)
	}
}
