package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// DuckBrainSync pushes fleet state to DuckBrain as a read replica.
type DuckBrainSync struct {
	db        *sql.DB
	namespace string
	interval  time.Duration
}

// NewDuckBrainSync creates a DuckBrain syncer.
func NewDuckBrainSync(db *sql.DB, namespace string) *DuckBrainSync {
	return &DuckBrainSync{
		db:        db,
		namespace: namespace,
		interval:  5 * time.Minute,
	}
}

// Run starts the periodic sync loop. Blocks until ctx is cancelled.
func (d *DuckBrainSync) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	log.Printf("SYNC: DuckBrain sync started (namespace=%s, every %s)", d.namespace, d.interval)

	for {
		select {
		case <-ctx.Done():
			log.Println("SYNC: stopping")
			return
		case <-ticker.C:
			d.syncOnce(ctx)
		}
	}
}

func (d *DuckBrainSync) syncOnce(ctx context.Context) {
	// Fleet summary.
	if err := d.syncFleetSummary(ctx); err != nil {
		log.Printf("SYNC: fleet summary error: %v", err)
	}
	// Per-project status.
	if err := d.syncProjectStatuses(ctx); err != nil {
		log.Printf("SYNC: project statuses error: %v", err)
	}
}

func (d *DuckBrainSync) syncFleetSummary(ctx context.Context) error {
	var total, enabled, activeTicks int
	d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&total)
	d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE enabled=1`).Scan(&enabled)
	d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&activeTicks)

	summary := map[string]interface{}{
		"total_projects": total,
		"enabled":        enabled,
		"active_ticks":   activeTicks,
		"synced_at":      time.Now().Format(time.RFC3339),
	}

	data, _ := json.Marshal(summary)
	return d.duckbrainRemember("/fleet/summary", "config", data)
}

func (d *DuckBrainSync) syncProjectStatuses(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT name, weight, priority, enabled, cooldown_s, decay_rate,
			model, provider,
			COALESCE(last_tick_completed, ''),
			COALESCE(last_tick_started, '')
		FROM projects ORDER BY name
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name, lastCompleted, lastStarted, model, provider string
		var weight, priority, cooldownS int
		var decayRate float64
		var enabled bool
		if err := rows.Scan(&name, &weight, &priority, &enabled, &cooldownS, &decayRate, &model, &provider, &lastCompleted, &lastStarted); err != nil {
			log.Printf("SYNC: scan project row: %v", err)
			continue
		}
		status := map[string]interface{}{
			"name":            name,
			"weight":          weight,
			"priority":        priority,
			"enabled":         enabled,
			"cooldown_s":      cooldownS,
			"decay_rate":      decayRate,
			"model":           model,
			"provider":        provider,
			"last_tick":       lastCompleted,
			"last_tick_start": lastStarted,
			"synced_at":       time.Now().Format(time.RFC3339),
		}
		statusData, _ := json.Marshal(status)
		_ = d.duckbrainRemember("/fleet/projects/"+name+"/status", "config", statusData)
	}
	return rows.Err()
}

func (d *DuckBrainSync) duckbrainRemember(key, domain string, data []byte) error {
	// Use the DuckBrain MCP tool via hermes CLI.
	// duckbrain remember <key> <domain> <attributes_json> <embedding_text>
	cmd := exec.Command("hermes", "mcp", "duckbrain", "remember",
		"--key", key,
		"--domain", domain,
		"--attributes", string(data),
		"--embedding-text", string(data),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("duckbrain remember %s: %w (output: %s)", key, err, string(output))
	}
	return nil
}
