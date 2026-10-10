package scheduler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-1654: the gateway availability path.
//
// Before this row the scheduler could not answer "how many ticks did the
// gateway cost us, and in what way?" — a 503 drain, a 429 rate limit and a
// refused dial all landed in the event log as the same generic failure (or,
// for the refused dial, as a deferral that is not a failure at all), which is
// exactly what made the 2026-09-27 03:00 incident (SCHED-GAP-1644)
// unattributable. These tests pin:
//
//  1. the classification itself (ClassifyGatewayError, and its use through a
//     REAL HTTP exchange against a mock gateway),
//  2. the spawn path's per-tick event — one per tick, carrying the class, the
//     lane and a timestamp,
//  3. the negative half: failures that are NOT gateway availability (5xx that
//     is not 503, auth rejections) emit NO availability event, so the count
//     cannot be inflated.

// schedGap1654Event is the shape the spawn path is expected to have written.
type schedGap1654Event struct {
	count int
	class string
	ts    string
	msg   string
}

// schedGap1654ReadEvent reads the single availability event written for one
// (project, tick) pair. count=0 means none was written.
func schedGap1654ReadEvent(t *testing.T, db *sql.DB, project, tickID string) schedGap1654Event {
	t.Helper()
	var e schedGap1654Event
	err := db.QueryRow(`
SELECT COUNT(*),
       COALESCE(MAX(json_extract(details, '$.error_class')), ''),
       COALESCE(MAX(json_extract(details, '$.timestamp')), ''),
       COALESCE(MAX(message), '')
FROM events
WHERE component = ?
  AND json_extract(details, '$.project') = ?
  AND json_extract(details, '$.tick_id') = ?`,
		GatewayAvailabilityEventComponent, project, tickID).Scan(&e.count, &e.class, &e.ts, &e.msg)
	if err != nil {
		t.Fatalf("read gateway availability event: %v", err)
	}
	return e
}

// schedGap1654Spawner builds a spawner wired to gatewayURL with the standard
// test posture (exec fallback disabled, event logger installed so the
// availability event can be read back from the DB) and, deliberately, ZERO
// transient retries so each case costs exactly one POST — the retry loop is
// covered by its own case below.
func schedGap1654Spawner(t *testing.T, db *sql.DB, gatewayURL string) *Spawner {
	t.Helper()
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(gatewayURL, "***", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.SetGatewayTransientRetries(0)
	spawner.SetEventLogger(NewEventLogger(db))
	return spawner
}

// TestSCHEDGAP1654_SpawnPathClassifiesAndEmits — the core acceptance: each of
// the three availability classes, driven by a REAL HTTP response (mock
// gateway 503 / 429, and a refused dial against a dead port), must
// (a) reach the class the endpoint counts and (b) write exactly ONE event
// carrying the class, the lane and a timestamp.
func TestSCHEDGAP1654_SpawnPathClassifiesAndEmits(t *testing.T) {
	cases := []struct {
		name      string
		status    int // HTTP status; 0 = no server (refused dial)
		wantClass string
		// wantDropped: true = the tick is dropped (Spawn returns an error),
		// false = the tick is DEFERRED (Spawn returns a tick whose Wait()
		// yields TickDeferred) — the refused-dial shape is a transient blip
		// and keeps its SCHED-GAP-203 deferral semantics.
		wantDropped bool
	}{
		{name: "503_service_unavailable", status: http.StatusServiceUnavailable, wantClass: GatewayErrClassUnavailable503, wantDropped: true},
		{name: "429_rate_limited", status: http.StatusTooManyRequests, wantClass: GatewayErrClassRateLimited429, wantDropped: true},
		{name: "connection_refused", status: 0, wantClass: GatewayErrClassConnRefused, wantDropped: false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			projectName := fmt.Sprintf("gap1654-%s", tc.name)
			tickID := fmt.Sprintf("gap1654-%s-%d", tc.name, i)
			mustCreateProjectINFRA012(t, db, projectName)
			insertRunningTick(t, db, tickID, projectName, 0)

			gatewayURL := "http://127.0.0.1:1" // nothing listens on port 1
			if tc.status != 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]string{
							"type":    "gateway_error",
							"message": fmt.Sprintf("mock gateway refusal HTTP %d", tc.status),
						},
					})
				}))
				t.Cleanup(srv.Close)
				gatewayURL = srv.URL
			}

			buf := schedGap080CaptureLog(t)
			spawner := schedGap1654Spawner(t, db, gatewayURL)

			tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
			switch {
			case tc.wantDropped && err == nil:
				t.Fatalf("Spawn err = nil, want a dropped tick (class %s)", tc.wantClass)
			case !tc.wantDropped && err != nil:
				t.Fatalf("Spawn err = %v, want a deferred tick (class %s)", err, tc.wantClass)
			}
			if !tc.wantDropped {
				if tick == nil {
					t.Fatal("Spawn returned nil tick — the refused-dial shape must defer, not drop")
				}
				if got := tick.Wait().Status; got != TickDeferred {
					t.Errorf("Wait() status = %s, want %s (a refused dial is a SCHED-GAP-203 blip)", got, TickDeferred)
				}
			}

			ev := schedGap1654ReadEvent(t, db, projectName, tickID)
			if ev.count != 1 {
				t.Fatalf("gateway_availability events for %s/%s = %d, want exactly 1 (class %s)",
					projectName, tickID, ev.count, tc.wantClass)
			}
			if ev.class != tc.wantClass {
				t.Errorf("event error_class = %q, want %q", ev.class, tc.wantClass)
			}
			if ev.ts == "" {
				t.Error("event details carry no timestamp — the acceptance asks for WHEN, not just what")
			}
			if _, perr := time.Parse(time.RFC3339, ev.ts); perr != nil {
				t.Errorf("event timestamp %q is not RFC3339: %v", ev.ts, perr)
			}
			// The event must name its class in a machine-readable message too,
			// so a grep of the events stream is as useful as the endpoint.
			if !strings.Contains(ev.msg, tc.wantClass) {
				t.Errorf("event message = %q, want it to name the class %q", ev.msg, tc.wantClass)
			}
			// The class classification must also be the one the client derives
			// from the same error (single authority: no second derivation).
			logs := buf.String()
			if !strings.Contains(logs, "GATEWAY-UNAVAILABLE: "+projectName+" tick="+tickID+" class="+tc.wantClass) {
				t.Errorf("logs missing the classified availability line for class %s:\n%s", tc.wantClass, logs)
			}
		})
	}
}

// TestSCHEDGAP1654_OneEventPerTickAcrossRetries — a 503 IS retryable
// (SCHED-GAP-080), so a persistently-draining gateway sees 1+N POSTs before
// the tick is dropped. The availability event must be written ONCE for the
// tick, not once per attempt: the endpoint counts TICKS lost, and a
// per-attempt counter would multiply one outage by the retry budget.
func TestSCHEDGAP1654_OneEventPerTickAcrossRetries(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap1654-retries"
		tickID      = "gap1654-retries-2026-10-10-03-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "gateway_draining", "message": "Gateway is draining"},
		})
	}))
	t.Cleanup(srv.Close)

	schedGap080CaptureLog(t)
	spawner := schedGap1654Spawner(t, db, srv.URL)
	spawner.SetGatewayTransientRetries(1) // 1 initial + 1 retry

	if _, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID); err == nil {
		t.Fatal("Spawn err = nil, want a drop once the 503 exhausted its retries")
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("gateway POSTs = %d, want 2 (1 initial + 1 configured retry)", got)
	}
	ev := schedGap1654ReadEvent(t, db, projectName, tickID)
	if ev.count != 1 {
		t.Fatalf("gateway_availability events = %d across %d POSTs, want exactly 1 per lost tick", ev.count, requests)
	}
	if ev.class != GatewayErrClassUnavailable503 {
		t.Errorf("event error_class = %q, want %q", ev.class, GatewayErrClassUnavailable503)
	}
}

// TestSCHEDGAP1654_ConcurrentTickLossesAggregateExactly — the availability
// counter is the events table, and ticks are lost CONCURRENTLY in production
// (one goroutine per project tick, all hitting the same draining gateway).
// This drives G concurrent Spawn calls through the shared single-connection
// SQLite pool (the same database.InitDB concurrency model the daemon runs)
// and requires the write path to be exactly-once per tick: no lost event, no
// duplicate, and the class aggregate equal to G afterwards. A lost write
// would understate the incident; a duplicate would overstate it — both are
// corruption of the exactness the endpoint promises. Run under -race it also
// pins the absence of data races in the Emit path.
func TestSCHEDGAP1654_ConcurrentTickLossesAggregateExactly(t *testing.T) {
	const goroutines = 8
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "gateway_draining", "message": "Gateway is draining"},
		})
	}))
	t.Cleanup(srv.Close)

	schedGap080CaptureLog(t)
	spawner := schedGap1654Spawner(t, db, srv.URL)

	type job struct {
		project string
		tickID  string
		dir     string
	}
	jobs := make([]job, goroutines)
	for i := range jobs {
		jobs[i] = job{
			project: fmt.Sprintf("gap1654-conc-%02d", i),
			tickID:  fmt.Sprintf("gap1654-conc-%02d-tick", i),
			dir:     t.TempDir(),
		}
		mustCreateProjectINFRA012(t, db, jobs[i].project)
		insertRunningTick(t, db, jobs[i].tickID, jobs[i].project, 0)
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = spawner.Spawn(PackedProject{Name: jobs[i].project, Workdir: jobs[i].dir}, jobs[i].tickID)
		}(i)
	}
	wg.Wait()

	byClass := map[string]int64{}
	var total int64
	for i, j := range jobs {
		// Every spawn must have FAILED (the gateway is draining) — a nil
		// error would mean the tick escaped classification entirely.
		if errs[i] == nil {
			t.Errorf("Spawn(%s/%s) err = nil, want the drop", j.project, j.tickID)
		}
		ev := schedGap1654ReadEvent(t, db, j.project, j.tickID)
		if ev.count != 1 {
			t.Errorf("%s/%s: gateway_availability events = %d, want exactly 1 (concurrent-write exactness)",
				j.project, j.tickID, ev.count)
			continue
		}
		if ev.class != GatewayErrClassUnavailable503 {
			t.Errorf("%s: event class = %q, want %q", j.project, ev.class, GatewayErrClassUnavailable503)
		}
		byClass[ev.class]++
		total++
	}
	if total != goroutines {
		t.Errorf("aggregate total = %d, want %d — an event write was lost under concurrency", total, goroutines)
	}
	if byClass[GatewayErrClassUnavailable503] != goroutines {
		t.Errorf("by_class[unavailable_503] = %d, want %d", byClass[GatewayErrClassUnavailable503], goroutines)
	}
}

// TestSCHEDGAP1654_NonAvailabilityFailuresEmitNothing is the negative half:
// a 500 (a gateway-side corruption, which has its own GAP-080/SCHED-GAP-143
// classification) and a 401 (terminal auth rejection, GAP-035) must NOT be
// counted as gateway unavailability — otherwise the endpoint's numbers would
// be inflated by failures whose operator response is entirely different.
func TestSCHEDGAP1654_NonAvailabilityFailuresEmitNothing(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{name: "500_internal", status: http.StatusInternalServerError},
		{name: "401_auth_rejected", status: http.StatusUnauthorized},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			projectName := "gap1654-neg-" + tc.name
			tickID := fmt.Sprintf("gap1654-neg-%d", i)
			mustCreateProjectINFRA012(t, db, projectName)
			insertRunningTick(t, db, tickID, projectName, 0)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"type":    "server_error",
						"message": "Internal server error: database disk image is malformed",
					},
				})
			}))
			t.Cleanup(srv.Close)

			schedGap080CaptureLog(t)
			spawner := schedGap1654Spawner(t, db, srv.URL)

			_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
			if err == nil {
				t.Fatalf("Spawn err = nil on HTTP %d, want a failure", tc.status)
			}
			if ev := schedGap1654ReadEvent(t, db, projectName, tickID); ev.count != 0 {
				t.Errorf("gateway_availability events = %d for HTTP %d, want 0 (not an availability class)",
					ev.count, tc.status)
			}
		})
	}
}
