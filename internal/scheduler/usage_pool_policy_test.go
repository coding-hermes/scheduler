package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
