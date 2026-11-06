package scheduler

// SCHED-GAP-16941: a tick killed by the 3h wall must name the phase it was in
// and how long it spent there. The measured sample (2 of 15 failures) could
// only say "tick-timeout wall 3h0m0s", so every capacity decision about the
// wall was guesswork.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSCHEDGAP16941_WallNamesGatewayPhase drives the gateway-POST wall booking
// and asserts the phase, its elapsed time, and the related row are both in the
// returned reason and on the persisted tick row.
func TestSCHEDGAP16941_WallNamesGatewayPhase(t *testing.T) {
	db := newTestDB(t)
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name: "phase-lane", RepoURL: "https://example.com/phase-lane", Workdir: t.TempDir(),
		Weight: 10, Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	const tickID = "phase-lane-tick-1"
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES (?, 'phase-lane', 'running', ?, ?)`,
		tickID, now.Add(-3*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	sp := NewSpawner(db, 1)
	start := time.Now().Add(-3 * time.Hour)
	st := sp.tickDeadlineTimeout(
		PackedProject{Name: "phase-lane", Workdir: t.TempDir()},
		tickID,
		errors.New("tick deadline exceeded"),
		start,
		3*time.Hour,
		"test-model",
		"test-provider",
		routerRate{},
		nil,
	)

	for _, want := range []string{
		"tick-timeout wall 3h0m0s", // pre-existing wall text still greppable
		"phase=gateway_call",
		"phase_elapsed=3h0m0s",
		"related=SCHED-GAP-16939",
	} {
		if !strings.Contains(st.gwFailErr, want) {
			t.Fatalf("gwFailErr = %q, want it to contain %q", st.gwFailErr, want)
		}
	}

	outcome := st.Wait()
	if outcome.Status != TickTimeout {
		t.Fatalf("Wait() status = %q, want timeout", outcome.Status)
	}
	if err := NewLifecycleTracker(db).Complete(outcome); err != nil {
		t.Fatalf("Complete(): %v", err)
	}

	rowError := tickErrorOf(t, db, tickID)
	if !strings.Contains(rowError, "phase=gateway_call") {
		t.Fatalf("ticks.error = %q, want explicit phase=gateway_call", rowError)
	}
	if !strings.Contains(rowError, "phase_elapsed=3h0m0s") {
		t.Fatalf("ticks.error = %q, want phase_elapsed=3h0m0s", rowError)
	}
	if !strings.Contains(rowError, "related=SCHED-GAP-16939") {
		t.Fatalf("ticks.error = %q, want related issue SCHED-GAP-16939", rowError)
	}
}

// TestSCHEDGAP16941_DispatchLegNamesDispatchWait pins the remote dispatch leg's
// wall the same way: a wall spent waiting on the hand-out reply is a distinct
// phase from the gateway POST, and must not be reported as gateway_call.
func TestSCHEDGAP16941_DispatchLegNamesDispatchWait(t *testing.T) {
	src := "dispatch_leg.go" // go test runs with the package dir as cwd
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if !strings.Contains(string(body), "phase=dispatch_wait") {
		t.Fatalf("%s has no phase=dispatch_wait annotation for its wall booking", src)
	}
}
