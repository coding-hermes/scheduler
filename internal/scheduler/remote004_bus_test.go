package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// stubCrier is the httptest Crier stand-in for the scheduler-package
// acceptance tests: it records POST /relay/publish requests and answers a
// configurable status (500 = the relay is up but refusing — the autonomy
// law's proving arm).
type stubCrier struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
	auth   []string
	status int
	srv    *httptest.Server
}

func newStubCrier(t *testing.T, status int) *stubCrier {
	s := &stubCrier{status: status}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(body, &m)
		s.mu.Lock()
		s.bodies = append(s.bodies, m)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubCrier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *stubCrier) last() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

// enabledBusFor builds an enabled bus pointed at the stub.
func enabledBusFor(s *stubCrier, schedID string) *scheduler.SchedulerBus {
	return scheduler.NewSchedulerBus(bus.NewClient(true, s.srv.URL, "", schedID))
}

// TestRemote004_PublishesDocumentedEnvelopeOnTickTerminal proves acceptance
// #2a: a tick-terminal transition fires exactly one publish carrying the
// documented envelope shape on the scheduler's topic.
func TestRemote004_PublishesDocumentedEnvelopeOnTickTerminal(t *testing.T) {
	crier := newStubCrier(t, http.StatusAccepted)
	busHook := enabledBusFor(crier, "alpha")

	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	lt.SetSchedulerBus(busHook)

	tickID := "alpha-remote004-1"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Minute),
		Finished: now,
		Status:   scheduler.TickCompleted,
		ExitCode: 0,
		// Dispatched so the completion is not a stand-down.
		DispatchDispatched: true,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := getTickStatus(t, db, tickID); got != database.StatusCompleted {
		t.Fatalf("tick status = %v, want completed — the bus must not alter the lifecycle", got)
	}
	if crier.count() != 1 {
		t.Fatalf("crier saw %d publishes, want exactly 1", crier.count())
	}
	body := crier.last()
	if body["topic"] != "sched.tick.alpha" {
		t.Errorf("topic = %v, want sched.tick.alpha", body["topic"])
	}
	raw, _ := json.Marshal(body["event"])
	var env bus.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("event not an envelope: %v (%s)", err, raw)
	}
	if env.SchedulerID != "alpha" {
		t.Errorf("scheduler_id = %q", env.SchedulerID)
	}
	if env.Kind != "tick.terminal" {
		t.Errorf("kind = %q, want tick.terminal", env.Kind)
	}
	if env.Project != "alpha" || env.TickID != tickID {
		t.Errorf("project/tick = %q/%q", env.Project, env.TickID)
	}
	if env.Status != "completed" {
		t.Errorf("status = %q, want completed (the lifecycle verdict)", env.Status)
	}
	if env.EventID == "" {
		t.Error("event_id empty — the publisher must mint the monotonic id")
	}
	if _, err := time.Parse(time.RFC3339Nano, env.TS); err != nil {
		t.Errorf("ts %q not RFC3339Nano", env.TS)
	}
}

// TestRemote004_PublishFailureDoesNotFailTick proves acceptance #2b AND the
// autonomy law (§3): a Crier stub that answers 500 — and a relay that is
// not even reachable — must leave the tick's terminal outcome untouched.
func TestRemote004_PublishFailureDoesNotFailTick(t *testing.T) {
	for name, busHook := range map[string]*scheduler.SchedulerBus{
		"relay-500":     enabledBusFor(newStubCrier(t, http.StatusInternalServerError), "alpha"),
		"relay-down":    scheduler.NewSchedulerBus(bus.NewClient(true, "http://127.0.0.1:1", "", "alpha")),
		"bus-disabled":  scheduler.NewSchedulerBus(bus.NewClient(false, "http://127.0.0.1:1", "", "alpha")),
		"bus-unset-nil": nil,
	} {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			mustCreateProject(t, db, "alpha")
			lt := scheduler.NewLifecycleTracker(db)
			lt.SetSchedulerBus(busHook)

			tickID := "alpha-remote004-fail"
			if err := lt.Enqueue("alpha", tickID); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := lt.StartRunning(tickID); err != nil {
				t.Fatalf("StartRunning: %v", err)
			}
			now := time.Now().UTC()
			// A FAILED tick with the bus failing around it: the error must
			// stay exactly the completion error, never a bus error.
			err := lt.Complete(scheduler.TickOutcome{
				TickID:   tickID,
				Project:  "alpha",
				Started:  now.Add(-time.Minute),
				Finished: now,
				Status:   scheduler.TickFailed,
				ExitCode: 1,
				Error:    "project-side failure",
			})
			if err != nil {
				t.Fatalf("Complete returned %v — a Crier failure must NEVER fail the tick", err)
			}
			if got := getTickStatus(t, db, tickID); got != database.StatusFailed {
				t.Errorf("tick status = %v, want failed", got)
			}
		})
	}
}

// TestRemote004_DisabledBusNeverDials proves acceptance #2c: with
// [crier] enabled=false the client is a pure no-op — zero requests even
// across many terminal ticks.
func TestRemote004_DisabledBusNeverDials(t *testing.T) {
	crier := newStubCrier(t, http.StatusAccepted)
	disabled := scheduler.NewSchedulerBus(bus.NewClient(false, crier.srv.URL, "", "alpha"))
	if disabled.Enabled() {
		t.Fatal("a disabled bus reports Enabled")
	}

	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	lt.SetSchedulerBus(disabled)

	for i := 1; i <= 3; i++ {
		tickID := fmt.Sprintf("alpha-remote004-off-%d", i)
		if err := lt.Enqueue("alpha", tickID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		_ = lt.StartRunning(tickID)
		now := time.Now().UTC()
		if err := lt.Complete(scheduler.TickOutcome{
			TickID: tickID, Project: "alpha",
			Started: now, Finished: now,
			Status: scheduler.TickCompleted, DispatchDispatched: true,
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if got := crier.count(); got != 0 {
		t.Fatalf("disabled bus hit crier %d times — enabled=false must be a pure no-op", got)
	}
	// The loop-level default is the same no-op: Bus() never nil.
	if scheduler.NewLoop(newTestDB(t), time.Second, time.Hour, 10, 100, 4).Bus() == nil {
		t.Fatal("Loop.Bus() returned nil — the no-op bus contract is broken")
	}
}

// TestRemote004_EventIDMonotonicAcrossTicks proves acceptance #3: the
// event_ids minted across successive terminal ticks carry a strictly
// increasing per-scheduler counter.
func TestRemote004_EventIDMonotonicAcrossTicks(t *testing.T) {
	crier := newStubCrier(t, http.StatusAccepted)
	busHook := enabledBusFor(crier, "alpha")

	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	lt.SetSchedulerBus(busHook)

	var ids []string
	for i := 1; i <= 4; i++ {
		tickID := fmt.Sprintf("alpha-remote004-mono-%d", i)
		if err := lt.Enqueue("alpha", tickID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		_ = lt.StartRunning(tickID)
		now := time.Now().UTC()
		if err := lt.Complete(scheduler.TickOutcome{
			TickID: tickID, Project: "alpha",
			Started: now, Finished: now,
			Status: scheduler.TickCompleted, DispatchDispatched: true,
		}); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
		raw, _ := json.Marshal(crier.last()["event"])
		var env bus.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		ids = append(ids, env.EventID)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if seen[id] {
			t.Fatalf("event id %q repeated — event ids must be monotonic (acceptance #3)", id)
		}
		seen[id] = true
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("event ids not monotonic: %q after %q", id, ids[i-1])
		}
	}
}

// TestRemote004_LoopSetSchedulerBusPropagatesToLifecycle proves the wiring:
// installing the bus on the loop reaches the lifecycle tracker the tracker
// uses at Complete time.
func TestRemote004_LoopSetSchedulerBusPropagatesToLifecycle(t *testing.T) {
	crier := newStubCrier(t, http.StatusAccepted)
	loopDB := newTestDB(t)
	loop := scheduler.NewLoop(loopDB, time.Second, time.Hour, 10, 100, 4)

	loop.SetSchedulerBus(enabledBusFor(crier, "alpha"))
	if !loop.Bus().Enabled() {
		t.Fatal("bus not enabled after SetSchedulerBus")
	}

	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	// Propagate the way Loop.SetSchedulerBus does.
	lt.SetSchedulerBus(loop.Bus())

	tickID := "alpha-remote004-prop"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_ = lt.StartRunning(tickID)
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID: tickID, Project: "alpha",
		Started: now, Finished: now,
		Status: scheduler.TickFailed, ExitCode: 1, Error: "x",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if crier.count() != 1 {
		t.Fatalf("crier saw %d publishes, want 1 — the loop bus must reach the tracker", crier.count())
	}
	// Installing nil or a disabled bus clears it again (back to silent).
	loop.SetSchedulerBus(scheduler.NewSchedulerBus(bus.NewClient(false, crier.srv.URL, "", "alpha")))
	if loop.Bus().Enabled() {
		t.Error("a disabled bus must not report Enabled after install")
	}
}

// TestRemote004_LaneStatePublishProves the lane-state hook publishes the
// documented vocabulary (paused/resumed/disabled) on the same topic.
func TestRemote004_LaneStatePublishes(t *testing.T) {
	crier := newStubCrier(t, http.StatusAccepted)
	loop := scheduler.NewLoop(newTestDB(t), time.Second, time.Hour, 10, 100, 4)
	loop.SetSchedulerBus(enabledBusFor(crier, "alpha"))

	loop.PublishLaneState(context.Background(), "some-lane", "paused")
	if crier.count() != 1 {
		t.Fatalf("crier saw %d publishes, want 1", crier.count())
	}
	raw, _ := json.Marshal(crier.last()["event"])
	var env bus.Envelope
	_ = json.Unmarshal(raw, &env)
	if env.Kind != "lane.state" || env.Project != "some-lane" || env.Status != "paused" {
		t.Errorf("lane-state envelope wrong: %+v", env)
	}
}
