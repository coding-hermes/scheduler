package database

import (
	"context"
	"database/sql"
	"encoding/json"

	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// UsagePool is a configured concurrency resource, independent of namespaces.
type UsagePool struct {
	ID          string
	Kind        string
	ActiveLimit int
	Enabled     bool
}

// UsagePoolLease reports an all-or-none reservation and the deterministic blocker.
type UsagePoolLease struct {
	LeaseID   string `json:"lease_id,omitempty"`
	PoolID    string `json:"pool_id"`
	Kind      string `json:"pool_kind,omitempty"`
	Active    int    `json:"active"`
	Limit     int    `json:"limit"`
	Available int    `json:"available"`
	Deferred  int    `json:"deferred"`
	Denied    bool   `json:"denied,omitempty"`
}

// CheckUsagePools validates pools and reports the lexicographically first blocker without reserving.
func CheckUsagePools(ctx context.Context, db *sql.DB, poolIDs []string) (UsagePoolLease, error) {
	if len(poolIDs) == 0 {
		return UsagePoolLease{}, fmt.Errorf("%w: no pools", ErrUsagePoolInvalid)
	}
	ids := compactStrings(append([]string(nil), poolIDs...))
	sort.Strings(ids)
	ids = compactStrings(ids)
	for _, id := range ids {
		var limit, active int
		var enabled int
		var kind string
		err := db.QueryRowContext(ctx, `SELECT kind,active_limit,enabled,(SELECT COUNT(*) FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=p.pool_id AND l.state='active') FROM usage_pools p WHERE pool_id=?`, id).Scan(&kind, &limit, &enabled, &active)
		if err == sql.ErrNoRows {
			return UsagePoolLease{}, fmt.Errorf("%w: %s", ErrUsagePoolMissing, id)
		}
		if err != nil {
			return UsagePoolLease{}, err
		}
		if enabled == 0 || limit <= 0 {
			return UsagePoolLease{}, fmt.Errorf("%w: %s", ErrUsagePoolInvalid, id)
		}
		if active >= limit {
			return UsagePoolLease{PoolID: id, Kind: kind, Active: active, Limit: limit, Available: 0, Denied: true}, nil
		}
	}
	return UsagePoolLease{}, nil
}

var ErrUsagePoolInvalid = errors.New("usage pool invalid")
var ErrUsagePoolMissing = errors.New("usage pool missing or disabled")

type usagePoolTx struct {
	conn *sql.Conn
}

func beginUsagePoolTx(ctx context.Context, db *sql.DB) (*usagePoolTx, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &usagePoolTx{conn: conn}, nil
}

func (tx *usagePoolTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *usagePoolTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx *usagePoolTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (tx *usagePoolTx) Commit() error {
	_, err := tx.conn.ExecContext(context.Background(), `COMMIT`)
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (tx *usagePoolTx) Rollback() error {
	_, err := tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func rollbackUsagePoolTx(tx *usagePoolTx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		log.Printf("USAGE_POOL: transaction rollback failed: %v", err)
	}
}

// ConfigureUsagePools atomically upserts definitions and lane memberships.
// Existing active leases prevent deletion, disabling, or lowering a limit below occupancy.
func ConfigureUsagePools(ctx context.Context, db *sql.DB, pools []UsagePool, memberships map[string][]string) error {
	seen := make(map[string]bool, len(pools))
	for _, p := range pools {
		if strings.TrimSpace(p.ID) != p.ID || p.ID == "" || p.ActiveLimit <= 0 || (p.Kind != "local" && p.Kind != "host" && p.Kind != "project") || seen[p.ID] {
			return fmt.Errorf("%w: pool %q has invalid or duplicate definition", ErrUsagePoolInvalid, p.ID)
		}
		if p.Kind == "host" && (!strings.HasPrefix(p.ID, "host:") || len(strings.TrimPrefix(p.ID, "host:")) == 0 || p.ActiveLimit < 8 || p.ActiveLimit > 12) {
			return fmt.Errorf("%w: host pool %q requires host:<stable-id> and limit 8 through 12", ErrUsagePoolInvalid, p.ID)
		}
		seen[p.ID] = true
	}
	tx, err := beginUsagePoolTx(ctx, db)
	if err != nil {
		return fmt.Errorf("begin usage-pool config: %w", err)
	}
	defer rollbackUsagePoolTx(tx)
	// Acquire SQLite's writer lock before validation so the active-count check and writes serialize across connections.
	if _, err = tx.ExecContext(ctx, `UPDATE usage_pools SET updated_at=updated_at WHERE pool_id=(SELECT pool_id FROM usage_pools LIMIT 1)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_pool_memberships`); err != nil {
		return err
	}
	if len(seen) == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE usage_pools SET enabled=0 WHERE NOT EXISTS (SELECT 1 FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=usage_pools.pool_id AND l.state='active')`); err != nil {
			return err
		}
	} else {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(seen)), ",")
		args := make([]any, 0, len(seen))
		for id := range seen {
			args = append(args, id)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage_pools SET enabled=0 WHERE pool_id NOT IN (`+placeholders+`) AND NOT EXISTS (SELECT 1 FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=usage_pools.pool_id AND l.state='active')`, args...); err != nil {
			return err
		}
	}
	for _, p := range pools {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=? AND l.state='active'`, p.ID).Scan(&active); err != nil {
			return err
		}
		if active > 0 && !p.Enabled {
			return fmt.Errorf("%w: pool %s has %d active leases", ErrUsagePoolInvalid, p.ID, active)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_pools(pool_id,kind,active_limit,enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(pool_id) DO UPDATE SET kind=excluded.kind,active_limit=excluded.active_limit,enabled=excluded.enabled,updated_at=excluded.updated_at`, p.ID, p.Kind, p.ActiveLimit, p.Enabled, clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	for lane, ids := range memberships {
		if strings.TrimSpace(lane) == "" {
			return fmt.Errorf("%w: blank lane membership", ErrUsagePoolInvalid)
		}
		unique := map[string]bool{}
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_pool_memberships WHERE lane=?`, lane); err != nil {
			return err
		}
		for _, id := range ids {
			if !seen[id] {
				return fmt.Errorf("%w: lane %s references %s", ErrUsagePoolMissing, lane, id)
			}
			if unique[id] {
				continue
			}
			unique[id] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO usage_pool_memberships(lane,pool_id) VALUES(?,?)`, lane, id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage-pool config: %w", err)
	}
	return nil
}

// UsagePoolIDsForLane returns the configured explicit memberships in stable order.
func UsagePoolIDsForLane(ctx context.Context, db *sql.DB, lane string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT pool_id FROM usage_pool_memberships WHERE lane=? ORDER BY pool_id`, lane)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReserveAll atomically charges every named pool or none. A repeated lease ID is idempotent only for the same tick/lane and membership set.
func ReserveAll(ctx context.Context, db *sql.DB, leaseID, tickID, lane string, poolIDs []string) (UsagePoolLease, error) {
	if leaseID == "" || tickID == "" || lane == "" || len(poolIDs) == 0 {
		return UsagePoolLease{}, fmt.Errorf("%w: lease, tick, lane and pools are required", ErrUsagePoolInvalid)
	}
	ids := append([]string(nil), poolIDs...)
	sort.Strings(ids)
	ids = compactStrings(ids)
	tx, err := beginUsagePoolTx(ctx, db)
	if err != nil {
		return UsagePoolLease{}, err
	}
	defer rollbackUsagePoolTx(tx)
	// No-op write obtains SQLite's cross-connection writer serialization before checking counts.
	if _, err := tx.ExecContext(ctx, `UPDATE usage_pools SET updated_at=updated_at WHERE pool_id=?`, ids[0]); err != nil {
		return UsagePoolLease{}, err
	}
	var state, oldTick, oldLane string
	err = tx.QueryRowContext(ctx, `SELECT state,tick_id,lane FROM usage_pool_leases WHERE lease_id=?`, leaseID).Scan(&state, &oldTick, &oldLane)
	if err == nil {
		if state != "active" || oldTick != tickID || oldLane != lane {
			return UsagePoolLease{}, fmt.Errorf("%w: lease ID collision", ErrUsagePoolInvalid)
		}
		rows, e := tx.QueryContext(ctx, `SELECT pool_id FROM usage_pool_lease_members WHERE lease_id=? ORDER BY pool_id`, leaseID)
		if e != nil {
			return UsagePoolLease{}, e
		}
		defer rows.Close()
		var existing []string
		for rows.Next() {
			var id string
			if e := rows.Scan(&id); e != nil {
				return UsagePoolLease{}, e
			}
			existing = append(existing, id)
		}
		if e := rows.Err(); e != nil {
			return UsagePoolLease{}, e
		}
		if strings.Join(existing, "\x00") != strings.Join(ids, "\x00") {
			return UsagePoolLease{}, fmt.Errorf("%w: retry membership differs", ErrUsagePoolInvalid)
		}
		if e := tx.Commit(); e != nil {
			return UsagePoolLease{}, e
		}
		return UsagePoolLease{LeaseID: leaseID}, nil
	} else if err != sql.ErrNoRows {
		return UsagePoolLease{}, err
	}
	type count struct {
		id, kind      string
		active, limit int
		enabled       bool
	}
	counts := make([]count, 0, len(ids))
	for _, id := range ids {
		var c count
		c.id = id
		var enabled int
		e := tx.QueryRowContext(ctx, `SELECT kind,active_limit,enabled,(SELECT COUNT(*) FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=p.pool_id AND l.state='active') FROM usage_pools p WHERE pool_id=?`, id).Scan(&c.kind, &c.limit, &enabled, &c.active)
		if e == sql.ErrNoRows {
			return UsagePoolLease{}, fmt.Errorf("%w: %s", ErrUsagePoolMissing, id)
		}
		if e != nil {
			return UsagePoolLease{}, e
		}
		c.enabled = enabled != 0
		if !c.enabled || c.limit <= 0 {
			return UsagePoolLease{}, fmt.Errorf("%w: %s", ErrUsagePoolInvalid, id)
		}
		counts = append(counts, c)
	}
	for _, c := range counts {
		if c.active >= c.limit {
			if _, err := tx.ExecContext(ctx, `INSERT INTO usage_pool_deferrals(pool_id,tick_id,created_at) VALUES(?,?,?) ON CONFLICT(pool_id,tick_id) DO UPDATE SET created_at=excluded.created_at`, c.id, tickID, clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return UsagePoolLease{}, err
			}
			if err := tx.Commit(); err != nil {
				return UsagePoolLease{}, err
			}
			return UsagePoolLease{PoolID: c.id, Kind: c.kind, Active: c.active, Limit: c.limit, Available: 0, Denied: true}, nil
		}
	}
	now := clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano)
	ownerID := fmt.Sprintf("pid:%d", os.Getpid())
	expiresAt := clock.FromContext(ctx).Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_pool_leases(lease_id,tick_id,lane,state,created_at,updated_at,owner_id,expires_at) VALUES(?,?,?,'active',?,?,?,?)`, leaseID, tickID, lane, now, now, ownerID, expiresAt); err != nil {
		return UsagePoolLease{}, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_pool_lease_members(lease_id,pool_id) VALUES(?,?)`, leaseID, id); err != nil {
			return UsagePoolLease{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return UsagePoolLease{}, err
	}
	return UsagePoolLease{LeaseID: leaseID}, nil
}

// ReleaseUsagePoolLease idempotently releases all memberships for one lease.
func ReleaseUsagePoolLease(ctx context.Context, db *sql.DB, leaseID string) error {
	tx, err := beginUsagePoolTx(ctx, db)
	if err != nil {
		return err
	}
	defer rollbackUsagePoolTx(tx)
	if _, err = tx.ExecContext(ctx, `UPDATE usage_pools SET updated_at=updated_at WHERE pool_id=(SELECT pool_id FROM usage_pool_lease_members WHERE lease_id=? LIMIT 1)`, leaseID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_pool_leases SET state='released',updated_at=? WHERE lease_id=? AND state='active'`, clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano), leaseID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseTerminalUsagePoolLeases releases active leases for named lanes whose
// ticks have already been made terminal by a scheduler reaper.
func ReleaseTerminalUsagePoolLeases(ctx context.Context, db *sql.DB, lanes []string) error {
	if len(lanes) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(lanes)), ",")
	args := make([]any, 0, len(lanes)+1)
	args = append(args, clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano))
	for _, lane := range lanes {
		args = append(args, lane)
	}
	_, err := db.ExecContext(ctx, `UPDATE usage_pool_leases SET state='released',updated_at=? WHERE state='active' AND lane IN (`+placeholders+`) AND tick_id IN (SELECT id FROM ticks WHERE status IN ('completed','failed','timeout','deferred'))`, args...)
	return err
}

// RecoverUsagePoolLeases releases terminal leases and retains leases for live
// owners. An expired lease with a dead, identifiable owner is failed and
// released atomically; ownerless and missing-tick leases remain quarantined.
func RecoverUsagePoolLeases(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := beginUsagePoolTx(ctx, db)
	if err != nil {
		return 0, err
	}
	defer rollbackUsagePoolTx(tx)
	rows, err := tx.QueryContext(ctx, `SELECT l.lease_id,l.tick_id,l.lane,l.owner_id,l.expires_at,l.state,COALESCE(t.status,'missing'),COALESCE((SELECT d.state FROM tick_dispatch d WHERE d.tick_id=l.tick_id ORDER BY d.updated_at DESC LIMIT 1),'') FROM usage_pool_leases l LEFT JOIN ticks t ON t.id=l.tick_id WHERE l.state='active' ORDER BY l.lease_id`)
	if err != nil {
		return 0, err
	}
	type recovery struct{ leaseID, tickID, lane, owner, expires, state, status, dispatchState string }
	var candidates []recovery
	for rows.Next() {
		var item recovery
		if err := rows.Scan(&item.leaseID, &item.tickID, &item.lane, &item.owner, &item.expires, &item.state, &item.status, &item.dispatchState); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	now := clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano)
	var released int64
	for _, item := range candidates {
		action := "retained_live_owner"
		if item.status == "completed" || item.status == "failed" || item.status == "timeout" || item.status == "deferred" {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_pool_leases SET state='released',updated_at=? WHERE lease_id=? AND state='active'`, now, item.leaseID); err != nil {
				return 0, err
			}
			released++
			action = "released_terminal"
		} else if item.owner == "" || item.status == "missing" {
			action = "quarantined_retained"
		} else if expires, err := time.Parse(time.RFC3339Nano, item.expires); !usagePoolOwnerAlive(item.owner) && err == nil && !expires.After(clock.FromContext(ctx).Now()) {
			// Once the recorded owner is dead and its durable lease deadline
			// has elapsed, the attempt is no longer authorized to occupy
			// capacity. Terminalize any still-live tick and expire an
			// outstanding hand-out in this same serialized transaction.
			if _, err := tx.ExecContext(ctx, `UPDATE ticks SET status='failed',outcome='failed',error='usage-pool lease expired after owner exit',completed_at=? WHERE id=? AND status IN ('queued','running')`, now, item.tickID); err != nil {
				return 0, err
			}
			if item.dispatchState == "dispatched" {
				if _, err := tx.ExecContext(ctx, `UPDATE tick_dispatch SET state='expired',error='usage-pool lease expired after owner exit',updated_at=? WHERE tick_id=? AND state='dispatched'`, now, item.tickID); err != nil {
					return 0, err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE usage_pool_leases SET state='released',updated_at=? WHERE lease_id=? AND state='active'`, now, item.leaseID); err != nil {
				return 0, err
			}
			released++
			action = "failed_expired_dead_owner"
		} else if !usagePoolOwnerAlive(item.owner) {
			action = "quarantined_retained"
		} else if expires, err := time.Parse(time.RFC3339Nano, item.expires); err != nil || !expires.After(clock.FromContext(ctx).Now()) {
			action = "retained_expired_owner_active_tick"
		}
		poolRows, err := tx.QueryContext(ctx, `SELECT pool_id FROM usage_pool_lease_members WHERE lease_id=? ORDER BY pool_id`, item.leaseID)
		if err != nil {
			return 0, err
		}
		var poolIDs []string
		for poolRows.Next() {
			var id string
			if err := poolRows.Scan(&id); err != nil {
				poolRows.Close()
				return 0, err
			}
			poolIDs = append(poolIDs, id)
		}
		if err := poolRows.Err(); err != nil {
			poolRows.Close()
			return 0, err
		}
		poolRows.Close()
		if action != "retained_live_owner" {
			detailBytes, err := json.Marshal(map[string]any{"lease_id": item.leaseID, "tick_id": item.tickID, "lane": item.lane, "pool_ids": poolIDs, "owner_id": item.owner, "expires_at": item.expires, "prior_status": item.status, "action": action})
			if err != nil {
				return 0, err
			}
			detail := string(detailBytes)
			if _, err := tx.ExecContext(ctx, `INSERT INTO events(severity,component,message,details,created_at,scheduler_id) VALUES(?,?,?,?,?,?)`, `MEDIUM`, "usage_pool", "usage_pool_recovery", detail, now, stampSchedulerID()); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return released, nil
}

func usagePoolOwnerAlive(ownerID string) bool {
	pidText, ok := strings.CutPrefix(ownerID, "pid:")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// UsagePoolStats returns active and configured limits for operator surfaces.
func UsagePoolStats(ctx context.Context, db *sql.DB) ([]UsagePoolLease, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.pool_id,p.kind,p.active_limit,(SELECT COUNT(*) FROM usage_pool_lease_members m JOIN usage_pool_leases l USING(lease_id) WHERE m.pool_id=p.pool_id AND l.state='active'),(SELECT COUNT(*) FROM usage_pool_deferrals d JOIN ticks t ON t.id=d.tick_id WHERE d.pool_id=p.pool_id AND t.status='queued') FROM usage_pools p WHERE p.enabled=1 ORDER BY p.pool_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UsagePoolLease, 0)
	for rows.Next() {
		var s UsagePoolLease
		if err := rows.Scan(&s.PoolID, &s.Kind, &s.Limit, &s.Active, &s.Deferred); err != nil {
			return nil, err
		}
		s.Available = s.Limit - s.Active
		if s.Available < 0 {
			s.Available = 0
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func compactStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
