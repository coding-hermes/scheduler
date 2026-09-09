package scheduler_test

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// syncBuf is a mutex-guarded buffer safe for concurrent log writes.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestResume_RedundantResumeDoesNotWedgeLoop_GAP101 locks the SCHED-GAP-101
// contract (re-proven by DOGFOOD-020 on 2026-09-09): a redundant Resume()
// on a loop that is NOT paused must be an idempotent no-op.
//
// Root cause: Run()'s outer select treated ANY value on pauseCh as "park
// now" — a resume sent while running was consumed as a pause (log line
// "LOOP: paused"), and the loop then sat in a nested select that stopped
// draining slotFreedCh / reaper / health tickers. Production deployers hit
// this on restart-when-drained resume steps: evaluation event triggers died,
// and /status never exposed a paused flag to debug it.
func TestResume_RedundantResumeDoesNotWedgeLoop_GAP101(t *testing.T) {
	db := newTestDB(t)
	fixture := scheduler.NewSimFixture(db)
	if err := fixture.Setup(fixture.TestProjects()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 100, 8)
	loop.SetSimulation(0.85)

	// Capture the global logger BEFORE Run starts so the wedge log line
	// ("LOOP: paused") is observable.
	captured := &syncBuf{}
	orig := log.Writer()
	log.SetOutput(captured)
	defer log.SetOutput(orig)

	done := make(chan struct{})
	go func() {
		loop.Run()
		close(done)
	}()

	// ── Phase A: the kill shot ────────────────────────────────────
	// Resume a loop that was never paused (the production deploy step).
	loop.Resume()
	time.Sleep(250 * time.Millisecond)

	if s := captured.String(); strings.Contains(s, "LOOP: paused") {
		t.Errorf("GAP-101: redundant Resume() wedged the loop — logged %q", "LOOP: paused")
	}

	// Sanity: evaluation must still work after the kill shot.
	loop.ForceEvaluate()
	spawnDeadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
			t.Fatalf("count ticks: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(spawnDeadline) {
			t.Fatal("GAP-101: loop spawned no ticks after redundant Resume() — eval loop wedged")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ── Phase B: real pause must actually pause evaluation ────────
	// Quiesce first: ForceEvaluate is async and an in-flight evaluate()
	// legitimately finishes its cycle even after Pause (pause stops NEW
	// evaluations; it does not abort one mid-run). Wait until the tick
	// count is stable so the baseline isn't a moving target.
	stable, prev := 0, -1
	for stable < 15 {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
			t.Fatalf("count ticks during quiesce: %v", err)
		}
		if n == prev {
			stable++
		} else {
			stable, prev = 0, n
		}
		time.Sleep(20 * time.Millisecond)
	}
	base := prev

	loop.Pause()
	time.Sleep(100 * time.Millisecond)
	loop.ForceEvaluate()
	time.Sleep(400 * time.Millisecond)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
		t.Fatalf("count ticks after pause: %v", err)
	}
	if n != base {
		t.Errorf("evaluation ran while paused: tick count %d → %d, want unchanged", base, n)
	}

	// ── Phase C: resume unblocks, Stop is clean ───────────────────
	loop.Resume()
	loop.ForceEvaluate()
	unblockDeadline := time.Now().Add(3 * time.Second)
	for {
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
			t.Fatalf("count ticks after resume: %v", err)
		}
		if n > base {
			break
		}
		if time.Now().After(unblockDeadline) {
			t.Fatal("evaluation did not spawn ticks after real Resume()")
		}
		time.Sleep(20 * time.Millisecond)
	}

	loop.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Pause/Resume/Stop")
	}
}
