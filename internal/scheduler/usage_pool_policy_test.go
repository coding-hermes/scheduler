package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestRemoteTickUsesHostPoolNotLocalPool(t *testing.T) {
	db, err := database.InitDB(t.TempDir() + "/usage-pools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	pools := []database.UsagePool{
		{ID: "local:control", Kind: "local", ActiveLimit: 12, Enabled: true},
		{ID: "host:build-01", Kind: "host", ActiveLimit: 8, Enabled: true},
		{ID: "project:helix", Kind: "project", ActiveLimit: 2, Enabled: true},
	}
	if err := database.ConfigureUsagePools(ctx, db, pools, map[string][]string{"helix": {"project:helix"}}); err != nil {
		t.Fatal(err)
	}
	pool := NewSlotPool(1, nil, nil)
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:control"})
	reserved, err := pool.reserveUsagePools(ctx, db, "helix", "tick-1", DispatchTarget{HostID: "build-01"}, true)
	if err != nil || !reserved {
		t.Fatalf("remote reserve = %t, %v", reserved, err)
	}
	stats, err := database.UsagePoolStats(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]int{}
	for _, stat := range stats {
		active[stat.PoolID] = stat.Active
	}
	if active["host:build-01"] != 1 || active["project:helix"] != 1 || active["local:control"] != 0 {
		t.Fatalf("remote pool occupancy = %#v", active)
	}
	if err := database.ReleaseUsagePoolLease(ctx, db, "tick-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteMissingHostPoolFailsClosed(t *testing.T) {
	db, err := database.InitDB(t.TempDir() + "/usage-pools.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := database.ConfigureUsagePools(ctx, db, []database.UsagePool{{ID: "local:control", Kind: "local", ActiveLimit: 12, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	pool := NewSlotPool(1, nil, nil)
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:control"})
	if _, err := pool.reserveUsagePools(ctx, db, "remote-lane", "tick-missing", DispatchTarget{HostID: "missing"}, true); !errors.Is(err, database.ErrUsagePoolMissing) {
		t.Fatalf("missing host-pool error = %v", err)
	}
	var leases int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("lease count = %d, error = %v", leases, err)
	}
}

func TestUsagePoolMissingLocalConfigLeavesDurableQueuedTick(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at) VALUES ('pool-queued-test', 'https://example.invalid/repo', '/tmp/pool-queued-test', 10, 5, 900, 1.0, 'm', 'p', 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDispatchTargets, filepath.Join(t.TempDir(), "missing-targets.jsonl"))
	pool := NewSlotPool(1, NewSpawner(db, 1), NewLifecycleTracker(db))
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:missing"})
	tickID := pool.Spawn(PackedProject{Name: "pool-queued-test", Workdir: "/tmp/pool-queued-test"}, time.Unix(1_800_000_000, 0), true, db)

	deadline := time.After(5 * time.Second)
	for {
		var status string
		err := db.QueryRow(`SELECT status FROM ticks WHERE id=?`, tickID).Scan(&status)
		if err == nil {
			if status != "queued" {
				t.Fatalf("tick status after missing-pool refusal = %q, want queued", status)
			}
			break
		}
		if err != sql.ErrNoRows {
			t.Fatalf("read queued tick: %v", err)
		}
		select {
		case <-deadline:
			t.Fatalf("tick %s was not durably enqueued before pool refusal", tickID)
		case <-time.After(time.Millisecond):
		}
	}
	var leases, dispatches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases WHERE tick_id=?`, tickID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tick_dispatch WHERE tick_id=?`, tickID).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if leases != 0 || dispatches != 0 || pool.Running() != 0 {
		t.Fatalf("refused tick leaked lease/dispatch/local slot: leases=%d dispatches=%d slots=%d", leases, dispatches, pool.Running())
	}
}

func TestRemoteMissingHostPoolFailsClosedNoFallback(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects (name,repo_url,workdir,created_at,updated_at) VALUES ('remote-missing-pool','https://example.invalid/repo','/tmp/remote-missing-pool','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{{ID: "local:control", Kind: "local", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	targets := writeDispatchTargets(t, `{"lane":"remote-missing-pool","agent":"agent-1","host_id":"bunker-1","workdir":"/remote"}`+"\n")
	t.Setenv(EnvDispatchTargets, targets)
	resetDispatchTargetsCache()
	pool := NewSlotPool(1, NewSpawner(db, 1), NewLifecycleTracker(db))
	pool.SetEventLogger(NewEventLogger(db))
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:control"})
	tickID := pool.Spawn(PackedProject{Name: "remote-missing-pool", Workdir: "/tmp/remote-missing-pool"}, time.Unix(1_800_000_000, 0), true, db)
	deadline := time.After(5 * time.Second)
	for {
		var reason string
		err := db.QueryRow(`SELECT message FROM events WHERE component='usage_pool' AND message='usage-pool admission refused' ORDER BY id DESC LIMIT 1`).Scan(&reason)
		if err == nil {
			if reason != "usage-pool admission refused" {
				t.Fatalf("refusal event=%q", reason)
			}
			break
		}
		if err != sql.ErrNoRows {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("missing remote host pool did not emit refusal event")
		case <-time.After(time.Millisecond):
		}
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM ticks WHERE id=?`, tickID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("tick status=%q err=%v, want durable queued row", status, err)
	}
	var leases, dispatches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases WHERE tick_id=?`, tickID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tick_dispatch WHERE tick_id=?`, tickID).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if leases != 0 || dispatches != 0 || pool.Running() != 0 {
		t.Fatalf("fallback/handout occurred: leases=%d dispatches=%d local_slots=%d", leases, dispatches, pool.Running())
	}
	var detail string
	if err := db.QueryRow(`SELECT details FROM events WHERE component='usage_pool' AND message='usage-pool admission refused' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"reason":"usage_pool_missing"`, `"host_id":"bunker-1"`, `"pool_id":"host:bunker-1"`, `"retryable":false`} {
		if !strings.Contains(detail, field) {
			t.Errorf("refusal detail %q missing %s", detail, field)
		}
	}
}

func TestUsagePoolLeaseReleasedAfterRemoteDispatchRefusal(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('remote-refused','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{{ID: "host:bunker", Kind: "host", ActiveLimit: 8, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDispatchTargets, writeDispatchTargets(t, `{"lane":"remote-refused","agent":"agent-1","host_id":"bunker","workdir":"/remote"}`+"\n"))
	resetDispatchTargetsCache()
	pool := NewSlotPool(1, NewSpawner(db, 1), NewLifecycleTracker(db))
	pool.SetEventLogger(NewEventLogger(db))
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true})
	tickID := pool.Spawn(PackedProject{Name: "remote-refused", Workdir: "/tmp"}, time.Now(), true, db)
	if status, ok := waitForTickTerminal(t, db, tickID, 15*time.Second); !ok || status != "failed" {
		t.Fatalf("remote dispatch refusal status=%q terminal=%t, want failed", status, ok)
	}
	assertNoActiveUsagePoolLeases(t, db)
	if pool.Running() != 0 {
		t.Fatalf("remote refusal consumed local slot: running=%d", pool.Running())
	}
}

func TestUsagePoolLeaseReleaseAcrossRemoteCompletionCancelAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holdReply  bool
		cancel     bool
		timeout    time.Duration
		wantStatus string
	}{
		{name: "completion", wantStatus: "completed"},
		{name: "cancel", holdReply: true, cancel: true, wantStatus: "timeout"},
		{name: "timeout", holdReply: true, timeout: 50 * time.Millisecond, wantStatus: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay := newFakeRelay(t, "usage-pool-scheduler", "usage-pool-agent", "completed")
			relay.holdReply = tc.holdReply
			_, pool, _, _, _ := newDispatchTestRig(t, relay, true)
			db := pool.spawner.db
			if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{{ID: "host:bunker", Kind: "host", ActiveLimit: 8, Enabled: true}}, nil); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvDispatchTargets, writeDispatchTargets(t, `{"lane":"helix","agent":"usage-pool-agent","host_id":"bunker","workdir":"/agent/helix"}`+"\n"))
			resetDispatchTargetsCache()
			pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true})
			if tc.timeout > 0 {
				pool.spawner.timeout = tc.timeout
			}
			tickID := pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)
			if tc.cancel {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var state string
					err := db.QueryRow(`SELECT state FROM tick_dispatch WHERE tick_id=?`, tickID).Scan(&state)
					if err == nil && state == database.DispatchStateDispatched {
						if !pool.spawner.CancelTickSession(tickID) {
							t.Fatal("cancel registry refused the live remote tick")
						}
						break
					}
					if err != nil && err != sql.ErrNoRows {
						t.Fatal(err)
					}
					if time.Now().Add(time.Millisecond).After(deadline) {
						t.Fatalf("tick %s was not handed off before cancellation", tickID)
					}
					time.Sleep(time.Millisecond)
				}
			}
			if status, ok := waitForTickTerminal(t, db, tickID, 15*time.Second); !ok || status != tc.wantStatus {
				t.Fatalf("tick status=%q terminal=%t, want %q", status, ok, tc.wantStatus)
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				stats, err := database.UsagePoolStats(context.Background(), db)
				if err != nil {
					t.Fatal(err)
				}
				if len(stats) == 1 && stats[0].Active == 0 {
					return
				}
				time.Sleep(time.Millisecond)
			}
			assertNoActiveUsagePoolLeases(t, db)
		})
	}
}

func TestUsagePoolLeaseReleasedAfterLocalSlotTimeout(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('local-timeout','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{{ID: "local:control", Kind: "local", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDispatchTargets, writeDispatchTargets(t, ""))
	resetDispatchTargetsCache()
	pool := NewSlotPool(1, NewSpawner(db, 1), NewLifecycleTracker(db))
	pool.SetEventLogger(NewEventLogger(db))
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:control"})
	pool.SetPatience(time.Millisecond)
	if !pool.Acquire(context.Background(), "busy-other") {
		t.Fatal("failed to occupy local slot for timeout fixture")
	}
	tickID := pool.Spawn(PackedProject{Name: "local-timeout", Workdir: "/tmp"}, time.Now(), true, db)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var event string
		err := db.QueryRow(`SELECT message FROM events WHERE component='slot_pool' AND message LIKE 'slot wait expired%' LIMIT 1`).Scan(&event)
		if err == nil {
			pool.Release("busy-other")
			assertNoActiveUsagePoolLeases(t, db)
			return
		}
		if err != sql.ErrNoRows {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	pool.Release("busy-other")
	t.Fatalf("slot timeout did not release usage lease for tick %s", tickID)
}

func TestUsagePoolLeaseReleasedWhenStartFails(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('start-fails','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{
		{ID: "local:control", Kind: "local", ActiveLimit: 1, Enabled: true},
		{ID: "project:start-fails", Kind: "project", ActiveLimit: 1, Enabled: true},
	}, map[string][]string{"start-fails": {"project:start-fails"}}); err != nil {
		t.Fatal(err)
	}
	// Make StartRunning refuse after the lease has been reserved. Ignoring the
	// queued→running update deterministically exercises post-admission cleanup.
	if _, err := db.Exec(`CREATE TRIGGER fail_start_after_usage_lease BEFORE UPDATE OF status ON ticks WHEN OLD.status='queued' AND NEW.status='running' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDispatchTargets, writeDispatchTargets(t, ""))
	resetDispatchTargetsCache()
	pool := NewSlotPool(1, NewSpawner(db, 1), NewLifecycleTracker(db))
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:control"})
	tickID := pool.Spawn(PackedProject{Name: "start-fails", Workdir: "/tmp"}, time.Now(), true, db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Wait(ctx); err != nil {
		t.Fatalf("spawn did not exit after start refusal: %v", err)
	}
	assertNoActiveUsagePoolLeases(t, db)
	var activeMemberships int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_lease_members m JOIN usage_pool_leases l ON l.lease_id=m.lease_id WHERE l.tick_id=? AND l.state='active'`, tickID).Scan(&activeMemberships); err != nil || activeMemberships != 0 {
		t.Fatalf("active lease memberships after start refusal=%d err=%v", activeMemberships, err)
	}
}

func TestUsagePoolLeaseReleasedWhenSpawnPanics(t *testing.T) {
	if path := os.Getenv("USAGE_POOL_PANIC_CHILD_DB"); path != "" {
		t.Setenv(EnvDispatchTargets, os.Getenv("USAGE_POOL_PANIC_TARGETS"))
		resetDispatchTargetsCache()
		db, err := database.InitDB(path)
		if err != nil {
			t.Fatal(err)
		}
		pool := NewSlotPool(1, nil, NewLifecycleTracker(db))
		pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, LocalPoolID: "local:panic"})
		pool.Spawn(PackedProject{Name: "panic-lane", Workdir: "/tmp"}, time.Now(), true, db)
		time.Sleep(time.Second)
		t.Fatal("nil spawner did not panic in production spawn lifecycle")
	}

	dbPath := filepath.Join(t.TempDir(), "panic.db")
	targetPath := filepath.Join(t.TempDir(), "dispatch-targets.jsonl")
	if err := os.WriteFile(targetPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{{ID: "local:panic", Kind: "local", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('panic-lane','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsagePoolLeaseReleasedWhenSpawnPanics$")
	cmd.Env = append(os.Environ(), "USAGE_POOL_PANIC_CHILD_DB="+dbPath, "USAGE_POOL_PANIC_TARGETS="+targetPath)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "nil pointer dereference") {
		t.Fatalf("child did not panic through SlotPool.spawn: err=%v output=%s", err, output)
	}
	db, err = database.InitDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertNoActiveUsagePoolLeases(t, db)
	var state string
	if err := db.QueryRow(`SELECT state FROM usage_pool_leases WHERE lane='panic-lane' ORDER BY created_at DESC LIMIT 1`).Scan(&state); err != nil || state != "released" {
		t.Fatalf("production panic cleanup state=%q err=%v, want released", state, err)
	}
	if err := database.ReleaseUsagePoolLease(context.Background(), db, mustQueryLeaseID(t, db, "panic-lane")); err != nil {
		t.Fatalf("repeat release after panic: %v", err)
	}
}

func TestUsagePoolLeaseReleasedByReaper(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('reaper-lane','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureUsagePools(context.Background(), db, []database.UsagePool{
		{ID: "local:reaper", Kind: "local", ActiveLimit: 1, Enabled: true},
		{ID: "project:reaper", Kind: "project", ActiveLimit: 1, Enabled: true},
	}, map[string][]string{"reaper-lane": {"project:reaper"}}); err != nil {
		t.Fatal(err)
	}
	lifecycle := NewLifecycleTracker(db)
	if err := lifecycle.Enqueue("reaper-lane", "reaper-tick"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE ticks SET status='timeout' WHERE id='reaper-tick'`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReserveAll(context.Background(), db, "reaper-tick", "reaper-tick", "reaper-lane", []string{"local:reaper", "project:reaper"}); err != nil {
		t.Fatal(err)
	}
	pool := NewSlotPool(1, NewSpawner(db, 1), lifecycle)
	pool.releaseReaped([]string{"reaper-lane"})
	pool.releaseReaped([]string{"reaper-lane"})
	assertNoActiveUsagePoolLeases(t, db)
	var state string
	if err := db.QueryRow(`SELECT state FROM usage_pool_leases WHERE lease_id='reaper-tick'`).Scan(&state); err != nil || state != "released" {
		t.Fatalf("reaper lease state=%q err=%v, want released", state, err)
	}
}

func assertNoActiveUsagePoolLeases(t *testing.T, db *sql.DB) {
	t.Helper()
	stats, err := database.UsagePoolStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, stat := range stats {
		if stat.Active != 0 {
			t.Fatalf("pool %s retained %d active leases", stat.PoolID, stat.Active)
		}
	}
}

func mustQueryLeaseID(t *testing.T, db *sql.DB, lane string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT lease_id FROM usage_pool_leases WHERE lane=? ORDER BY created_at DESC LIMIT 1`, lane).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUsagePoolsDisabledCompatibility(t *testing.T) {
	db := newTestDB(t)
	pool := NewSlotPool(1, nil, nil)
	reserved, err := pool.reserveUsagePools(context.Background(), db, "local-lane", "compat-tick", DispatchTarget{}, false)
	if err != nil || reserved {
		t.Fatalf("disabled policy decision=%t err=%v", reserved, err)
	}
	var leases int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("disabled policy changed lease state: count=%d err=%v", leases, err)
	}
}

func TestUsagePoolsObserveOnlyNoMutation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.ConfigureUsagePools(ctx, db, []database.UsagePool{{ID: "host:bunker", Kind: "host", ActiveLimit: 8, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	pool := NewSlotPool(1, nil, nil)
	pool.SetUsagePoolPolicy(UsagePoolPolicy{Enabled: true, ObserveOnly: true})
	reserved, err := pool.reserveUsagePools(ctx, db, "remote-lane", "observe-tick", DispatchTarget{HostID: "bunker"}, true)
	if err != nil || reserved {
		t.Fatalf("observe-only decision=%t err=%v", reserved, err)
	}
	var leases int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases`).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("observe-only created %d leases, err=%v", leases, err)
	}
}
