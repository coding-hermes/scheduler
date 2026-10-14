package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// UsagePoolPolicy controls the opt-in DB-backed usage-pool admission layer.
type UsagePoolPolicy struct {
	Enabled     bool
	ObserveOnly bool
	LocalPoolID string
}

func (p *SlotPool) reserveUsagePools(ctx context.Context, db *sql.DB, lane, tickID string, target DispatchTarget, remote bool) (bool, error) {
	p.mu.Lock()
	policy := p.usagePolicy
	events := p.events
	p.mu.Unlock()
	if policy == nil || !policy.Enabled {
		return false, nil
	}
	if db == nil {
		return false, fmt.Errorf("usage-pool authority unavailable: nil scheduler database")
	}
	poolIDs, err := database.UsagePoolIDsForLane(ctx, db, lane)
	if err != nil {
		return false, err
	}
	if remote {
		hostID := strings.TrimSpace(target.HostID)
		if hostID == "" || strings.ContainsAny(hostID, ":/\\ 	\r\n") {
			return false, fmt.Errorf("%w: lane=%s has missing or invalid stable host_id", database.ErrUsagePoolMissing, lane)
		}
		poolIDs = append(poolIDs, "host:"+hostID)
	} else if policy.LocalPoolID != "" {
		poolIDs = append(poolIDs, policy.LocalPoolID)
	}
	if len(poolIDs) == 0 {
		return false, fmt.Errorf("%w: lane=%s has no implicit or explicit pool", database.ErrUsagePoolMissing, lane)
	}
	if policy.ObserveOnly {
		decision, checkErr := database.CheckUsagePools(ctx, db, poolIDs)
		if checkErr != nil {
			return false, checkErr
		}
		if decision.Denied {
			log.Printf("USAGE_POOL: observe-only defer lane=%s pool=%s active=%d limit=%d", lane, decision.PoolID, decision.Active, decision.Limit)
		}
		return false, nil
	}
	if recovered, recoveryErr := database.RecoverUsagePoolLeases(ctx, db); recoveryErr != nil {
		return false, fmt.Errorf("usage-pool authority recovery: %w", recoveryErr)
	} else if recovered > 0 {
		log.Printf("USAGE_POOL: recovered %d leases on admission", recovered)
	}
	decision, err := database.ReserveAll(ctx, db, tickID, tickID, lane, poolIDs)
	if err != nil {
		return false, err
	}
	if decision.Denied {
		if events != nil {
			events.Emit(context.Background(), SeverityMedium, "usage_pool", "usage pool capacity reached", map[string]any{
				"reason": "usage_pool_capacity", "tick_id": tickID, "lane": lane, "pool_id": decision.PoolID,
				"pool_kind": decision.Kind, "active": decision.Active, "limit": decision.Limit, "waited_ms": 0, "retryable": true,
			})
		}
		return false, nil
	}
	return true, nil
}

func isUsagePoolMisconfiguration(err error) bool {
	return errors.Is(err, database.ErrUsagePoolMissing) || errors.Is(err, database.ErrUsagePoolInvalid)
}

func (p *SlotPool) emitUsagePoolRefusal(proj PackedProject, tickID, reason string, err error, target DispatchTarget) {
	p.mu.Lock()
	events := p.events
	p.mu.Unlock()
	log.Printf("USAGE_POOL: lane=%s tick=%s reason=%s error=%v", proj.Name, tickID, reason, err)
	if events != nil {
		details := map[string]any{
			"reason": reason, "lane": proj.Name, "tick_id": tickID,
			"error_class": fmt.Sprintf("%T", err), "detail": err.Error(),
			"configuration_source": "schedulerd.toml and dispatch-targets.jsonl",
			"retryable":            reason == "usage_pool_authority_unavailable",
		}
		poolIDs := []string{}
		var poolErr error
		if p.lifecycle != nil && p.lifecycle.db != nil {
			poolIDs, poolErr = database.UsagePoolIDsForLane(context.Background(), p.lifecycle.db, proj.Name)
		}
		if poolErr == nil {
			if target.HostID != "" {
				poolIDs = append(poolIDs, "host:"+target.HostID)
			}
		}
		details["pool_ids"] = poolIDs
		if target.HostID != "" {
			details["host_id"] = target.HostID
		}
		if id := strings.TrimPrefix(err.Error(), "usage pool missing or disabled: "); id != err.Error() {
			details["pool_id"] = id
		} else if id := strings.TrimPrefix(err.Error(), "usage pool invalid: "); id != err.Error() {
			details["pool_id"] = id
		}
		events.Emit(context.Background(), SeverityHigh, "usage_pool", "usage-pool admission refused", details)
	}
}
