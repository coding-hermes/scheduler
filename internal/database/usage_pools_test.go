package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestUsagePoolMembershipAcrossNamespaces(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "host:shared", Kind: "host", ActiveLimit: 8, Enabled: true}}, map[string][]string{"lane-a": {"host:shared"}, "lane-b": {"host:shared"}}); err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"lane-a", "lane-b"} {
		got, err := ReserveAll(ctx, db, "lease-"+lane, "tick-"+lane, lane, []string{"host:shared"})
		if err != nil || got.Denied {
			t.Fatalf("ReserveAll(%s) = %+v, %v", lane, got, err)
		}
	}
	stats, err := UsagePoolStats(ctx, db)
	if err != nil || len(stats) != 1 || stats[0].Active != 2 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestReserveAllAtomicNoPartialLease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "free", Kind: "project", ActiveLimit: 1, Enabled: true}, {ID: "full", Kind: "project", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := ReserveAll(ctx, db, "busy", "tick-busy", "other", []string{"full"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReserveAll(ctx, db, "new", "tick-new", "lane", []string{"free", "full"})
	if err != nil || !got.Denied || got.PoolID != "full" {
		t.Fatalf("deny=%+v err=%v", got, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases WHERE lease_id='new'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial lease count=%d err=%v", n, err)
	}
	if err := ReleaseUsagePoolLease(ctx, db, "busy"); err != nil {
		t.Fatal(err)
	}
	got, err = ReserveAll(ctx, db, "new", "tick-new", "lane", []string{"free", "full"})
	if err != nil || got.Denied {
		t.Fatalf("retry=%+v err=%v", got, err)
	}
}

func TestUsagePoolLeaseReleaseIdempotentAndMissingFailsClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := ReserveAll(ctx, db, "x", "t", "lane", []string{"absent"}); !errors.Is(err, ErrUsagePoolMissing) {
		t.Fatalf("missing err=%v", err)
	}
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p", Kind: "project", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveAll(ctx, db, "x", "t", "lane", []string{"p"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ReleaseUsagePoolLease(ctx, db, "x"); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := UsagePoolStats(ctx, db)
	if err != nil || stats[0].Active != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func TestUsagePoolHostLimitRangeAndConcurrentConnections(t *testing.T) {
	path := t.TempDir() + "/concurrent.db"
	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	peer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	peer.SetMaxOpenConns(1)
	if _, err := peer.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	ctx := context.Background()
	for _, n := range []int{7, 13} {
		if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "host:h", Kind: "host", ActiveLimit: n, Enabled: true}}, nil); !errors.Is(err, ErrUsagePoolInvalid) {
			t.Fatalf("limit %d error=%v", n, err)
		}
	}
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "host:h", Kind: "host", ActiveLimit: 8, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	// Concurrent callers share the same authoritative SQLite database.
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	var failures []error
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			connection := db
			if i%2 != 0 {
				connection = peer
			}
			got, reserveErr := ReserveAll(ctx, connection, "lease-"+id, "tick-"+id, "lane", []string{"host:h"})
			mu.Lock()
			defer mu.Unlock()
			if reserveErr != nil {
				failures = append(failures, reserveErr)
			} else if !got.Denied {
				admitted++
			}
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent reservations failed: %v", failures)
	}
	if admitted != 8 {
		t.Fatalf("admitted %d, want 8", admitted)
	}
}

func TestUsagePoolRecoveryReleasesTerminalTickLease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p", Kind: "project", ActiveLimit: 2, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	// Recovery tolerates tick rows absent from this fixture; create a real project/tick.
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('rec','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ticks(id,project_name,status,created_at) VALUES('rec-t','rec','completed','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveAll(ctx, db, "rec-lease", "rec-t", "rec", []string{"p"}); err != nil {
		t.Fatal(err)
	}
	n, err := RecoverUsagePoolLeases(ctx, db)
	if err != nil || n != 1 {
		t.Fatalf("recovered=%d err=%v", n, err)
	}
}

func TestUsagePoolRecoveryRetainsNonterminalAndQuarantinesOwnerlessLease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p", Kind: "project", ActiveLimit: 2, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('rec-live','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live-tick", "ownerless-tick"} {
		if _, err := db.Exec(`INSERT INTO ticks(id,project_name,status,created_at) VALUES(?,'rec-live','queued','now')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := ReserveAll(ctx, db, id, id, "rec-live", []string{"p"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE usage_pool_leases SET owner_id='',expires_at='2000-01-01T00:00:00Z' WHERE lease_id='ownerless-tick'`); err != nil {
		t.Fatal(err)
	}
	if released, err := RecoverUsagePoolLeases(ctx, db); err != nil || released != 0 {
		t.Fatalf("recovered=%d err=%v, nonterminal leases must be retained", released, err)
	}
	var active, recoveryEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_pool_leases WHERE state='active'`).Scan(&active); err != nil || active != 2 {
		t.Fatalf("active leases=%d err=%v", active, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE message='usage_pool_recovery' AND details LIKE '%quarantined_retained%'`).Scan(&recoveryEvents); err != nil || recoveryEvents != 1 {
		t.Fatalf("ownerless recovery events=%d err=%v", recoveryEvents, err)
	}
	var owner, expiry string
	if err := db.QueryRow(`SELECT owner_id,expires_at FROM usage_pool_leases WHERE lease_id='live-tick'`).Scan(&owner, &expiry); err != nil || owner == "" || expiry == "" {
		t.Fatalf("lease owner/expiry=%q/%q err=%v", owner, expiry, err)
	}
}

func TestUsagePoolRecoveryExpiresDeadOwnerAndDispatchedWork(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p", Kind: "project", ActiveLimit: 2, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('crash-recovery','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unhanded", "handed"} {
		if _, err := db.Exec(`INSERT INTO ticks(id,project_name,status,created_at) VALUES(?,'crash-recovery','queued','now')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := ReserveAll(ctx, db, id, id, "crash-recovery", []string{"p"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE usage_pool_leases SET owner_id='pid:2147483647',expires_at='2000-01-01T00:00:00Z' WHERE lease_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tick_dispatch(tick_id,lane,agent,corr_id,state,updated_at) VALUES('handed','crash-recovery','agent','corr','dispatched','now')`); err != nil {
		t.Fatal(err)
	}
	var dispatchState string
	if err := db.QueryRow(`SELECT state FROM tick_dispatch WHERE tick_id='handed'`).Scan(&dispatchState); err != nil || dispatchState != "dispatched" {
		t.Fatalf("fixture dispatch state=%q err=%v", dispatchState, err)
	}
	if recovered, err := RecoverUsagePoolLeases(ctx, db); err != nil || recovered != 2 {
		t.Fatalf("recovered=%d err=%v, want both expired dead-owner leases released", recovered, err)
	}
	stats, err := UsagePoolStats(ctx, db)
	if err != nil || len(stats) != 1 || stats[0].Active != 0 {
		t.Fatalf("stats=%+v err=%v, expired dead-owner leases must not strand capacity", stats, err)
	}
	for _, tickID := range []string{"unhanded", "handed"} {
		var status, state string
		if err := db.QueryRow(`SELECT status FROM ticks WHERE id=?`, tickID).Scan(&status); err != nil || status != "failed" {
			t.Fatalf("tick %s status=%q err=%v, want failed", tickID, status, err)
		}
		if err := db.QueryRow(`SELECT state FROM usage_pool_leases WHERE lease_id=?`, tickID).Scan(&state); err != nil || state != "released" {
			t.Fatalf("lease %s state=%q err=%v, want released", tickID, state, err)
		}
	}
	if err := db.QueryRow(`SELECT state FROM tick_dispatch WHERE tick_id='handed'`).Scan(&dispatchState); err != nil || dispatchState != "expired" {
		t.Fatalf("dispatch state=%q err=%v, want expired", dispatchState, err)
	}
	var actions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE message='usage_pool_recovery' AND details LIKE '%failed_expired_dead_owner%'`).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("recovery events=%d err=%v, want two deterministic expiry actions", actions, err)
	}
	for _, id := range []string{"next-a", "next-b"} {
		if lease, err := ReserveAll(ctx, db, id, id, "crash-recovery", []string{"p"}); err != nil || lease.Denied {
			t.Fatalf("post-recovery admission %s = %+v, %v", id, lease, err)
		}
	}
	if decision, err := ReserveAll(ctx, db, "over-limit", "over-limit", "crash-recovery", []string{"p"}); err != nil || !decision.Denied {
		t.Fatalf("post-recovery limit check = %+v, %v", decision, err)
	}
}

func TestUsagePoolDeferredCountTracksQueuedDeferrals(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p", Kind: "project", ActiveLimit: 1, Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects(name,repo_url,workdir,created_at,updated_at) VALUES('deferred-lane','url','/tmp','now','now')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"occupier", "waiting"} {
		if _, err := db.Exec(`INSERT INTO ticks(id,project_name,status,created_at) VALUES(?,'deferred-lane','queued','now')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReserveAll(ctx, db, "occupier", "occupier", "deferred-lane", []string{"p"}); err != nil {
		t.Fatal(err)
	}
	decision, err := ReserveAll(ctx, db, "waiting", "waiting", "deferred-lane", []string{"p"})
	if err != nil || !decision.Denied {
		t.Fatalf("denial=%+v err=%v", decision, err)
	}
	stats, err := UsagePoolStats(ctx, db)
	if err != nil || len(stats) != 1 || stats[0].Deferred != 1 {
		t.Fatalf("stats=%+v err=%v, want one queued deferral", stats, err)
	}
}

// Ensure the migration version test catches the new schema on a normal DB connection.
func TestUsagePoolTablesMigrated(t *testing.T) {
	db := newTestDB(t)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('usage_pools','usage_pool_memberships','usage_pool_leases','usage_pool_lease_members')`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("tables=%d err=%v", n, err)
	}
}
