package scheduler

import (
	"database/sql"
	"testing"
	"time"
)

// SCHED-GAP-1685 covers the bounded restart handoff. A fresh gateway row is
// adopted as a durable claim only; the scheduler must not issue a second
// request when the old process's response stream cannot be transferred.

func TestSCHEDGAP1685_StartupAdoptionIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	const project = "schedgap1685-live"
	mustCreateProjectINFRA012(t, db, project)
	fresh := time.Now().UTC().Add(-time.Minute)
	insertGatewayTickState(t, db, "schedgap1685-live-tick", project, fresh, &fresh)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	if got := loop.adoptLiveGatewayTicksAtStartup(); got != 1 {
		t.Fatalf("first startup adoption = %d projects, want 1", got)
	}
	if !loop.slotPool.RunningSet()[project] {
		t.Fatal("fresh gateway tick was not added to the startup dedup/slot view")
	}
	if got := loop.slotPool.Running(); got != 1 {
		t.Fatalf("running slots after adoption = %d, want 1", got)
	}

	// A repeated startup reconciliation must not acquire another slot or
	// create another execution opportunity for the same durable tick.
	if got := loop.adoptLiveGatewayTicksAtStartup(); got != 0 {
		t.Fatalf("repeated startup adoption = %d projects, want 0", got)
	}
	if got := loop.slotPool.Running(); got != 1 {
		t.Fatalf("running slots after repeated adoption = %d, want 1", got)
	}
	if loop.slotPool.tryReserve(project) {
		t.Fatal("adopted project became reservable: duplicate execution was not blocked")
	}

	// The first post-boot evaluation must also respect the durable running
	// row; adoption is not allowed to create a replay row or a second request.
	loop.evaluate()

	var ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, project).Scan(&ticks); err != nil {
		t.Fatalf("count durable ticks: %v", err)
	}
	if ticks != 1 {
		t.Fatalf("durable tick count = %d, want 1; adoption must not enqueue a replay", ticks)
	}
}

func TestSCHEDGAP1685_DeadOwnerReapReleasesAdoptedClaim(t *testing.T) {
	db := newTestDB(t)
	const project = "schedgap1685-dead"
	mustCreateProjectINFRA012(t, db, project)
	fresh := time.Now().UTC().Add(-time.Minute)
	insertGatewayTickState(t, db, "schedgap1685-dead-tick", project, fresh, &fresh)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	if got := loop.adoptLiveGatewayTicksAtStartup(); got != 1 {
		t.Fatalf("startup adoption = %d projects, want 1", got)
	}

	// The heartbeat stopping is the production dead-owner signal for a
	// gateway tick. The existing startup cleanup must still reap it and
	// release the claim that adoption installed.
	stale := time.Now().UTC().Add(-gatewayZombieMaxAge - time.Minute)
	if _, err := db.Exec(`UPDATE ticks SET heartbeat_at = ? WHERE id = ?`,
		stale.Format(time.RFC3339), "schedgap1685-dead-tick"); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	loop.cleanDanglingOnStartup()

	if got := tickStatusOf(t, db, "schedgap1685-dead-tick"); got != "timeout" {
		t.Fatalf("dead gateway tick status = %q, want timeout", got)
	}
	if loop.slotPool.RunningSet()[project] {
		t.Fatal("startup reaper left the adopted claim after the owner became stale")
	}
	var reason sql.NullString
	if err := db.QueryRow(`SELECT orphan_reason FROM ticks WHERE id = ?`, "schedgap1685-dead-tick").Scan(&reason); err != nil {
		t.Fatalf("read orphan reason: %v", err)
	}
	if !reason.Valid || reason.String != OrphanReasonStartupReap {
		t.Fatalf("orphan reason = %q (valid=%v), want %q", reason.String, reason.Valid, OrphanReasonStartupReap)
	}
}
