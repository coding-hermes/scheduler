package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// DuckBrainSync pushes fleet state to DuckBrain as a read replica
// via its HTTP REST API.
type DuckBrainSync struct {
	db         *sql.DB
	namespace  string
	baseURL    string
	httpClient *http.Client
	interval   time.Duration
}

// NewDuckBrainSync creates a DuckBrain syncer.
// baseURL is the DuckBrain HTTP server URL (e.g., http://localhost:3000).
func NewDuckBrainSync(db *sql.DB, namespace, baseURL string) *DuckBrainSync {
	return &DuckBrainSync{
		db:         db,
		namespace:  namespace,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		interval:   5 * time.Minute,
	}
}

// Run starts the periodic sync loop. Blocks until ctx is cancelled.
func (d *DuckBrainSync) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	log.Printf("SYNC: DuckBrain sync started (namespace=%s, baseURL=%s, every %s)",
		d.namespace, d.baseURL, d.interval)

	// Sync immediately on start.
	d.syncOnce(ctx)

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

// syncOnce runs one sync cycle: fleet summary + per-project statuses.
func (d *DuckBrainSync) syncOnce(ctx context.Context) {
	log.Println("SYNC: running sync cycle")

	if err := d.syncFleetSummary(ctx); err != nil {
		log.Printf("SYNC: fleet summary error: %v", err)
	}

	if err := d.syncProjectStatuses(ctx); err != nil {
		log.Printf("SYNC: project statuses error: %v", err)
	}
}

// fleetSummary is the payload sent to DuckBrain for /fleet/summary.
type fleetSummary struct {
	TotalProjects int    `json:"total_projects"`
	Enabled       int    `json:"enabled"`
	ActiveTicks   int    `json:"active_ticks"`
	SyncedAt      string `json:"synced_at"`
}

// syncFleetSummary queries aggregate fleet stats and pushes to DuckBrain.
func (d *DuckBrainSync) syncFleetSummary(ctx context.Context) error {
	var total, enabled, activeTicks int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&total); err != nil {
		return fmt.Errorf("count projects: %w", err)
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE enabled=1`).Scan(&enabled); err != nil {
		return fmt.Errorf("count enabled: %w", err)
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&activeTicks); err != nil {
		return fmt.Errorf("count active ticks: %w", err)
	}

	summary := fleetSummary{
		TotalProjects: total,
		Enabled:       enabled,
		ActiveTicks:   activeTicks,
		SyncedAt:      time.Now().Format(time.RFC3339),
	}

	return d.postMemory(ctx, "/fleet/summary", "config", summary)
}

// projectStatus is the per-project payload sent to DuckBrain.
type projectStatus struct {
	Name            string  `json:"name"`
	Weight          int     `json:"weight"`
	Priority        int     `json:"priority"`
	Enabled         bool    `json:"enabled"`
	CooldownS       int     `json:"cooldown_s"`
	DecayRate       float64 `json:"decay_rate"`
	Model           string  `json:"model"`
	Provider        string  `json:"provider"`
	LastTick        string  `json:"last_tick"`
	LastTickStart   string  `json:"last_tick_start"`
	SyncedAt        string  `json:"synced_at"`
}

// syncProjectStatuses queries all projects and pushes one memory each to DuckBrain.
func (d *DuckBrainSync) syncProjectStatuses(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT name, weight, priority, enabled, cooldown_s, decay_rate,
			model, provider,
			COALESCE(last_tick_completed, ''),
			COALESCE(last_tick_started, '')
		FROM projects ORDER BY name
	`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	syncedAt := time.Now().Format(time.RFC3339)
	for rows.Next() {
		var name, lastCompleted, lastStarted, model, provider string
		var weight, priority, cooldownS int
		var decayRate float64
		var enabled bool
		if err := rows.Scan(&name, &weight, &priority, &enabled, &cooldownS, &decayRate,
			&model, &provider, &lastCompleted, &lastStarted); err != nil {
			log.Printf("SYNC: scan project row: %v", err)
			continue
		}

		status := projectStatus{
			Name:          name,
			Weight:        weight,
			Priority:      priority,
			Enabled:       enabled,
			CooldownS:     cooldownS,
			DecayRate:     decayRate,
			Model:         model,
			Provider:      provider,
			LastTick:      lastCompleted,
			LastTickStart: lastStarted,
			SyncedAt:      syncedAt,
		}

		key := "/fleet/projects/" + name + "/status"
		if err := d.postMemory(ctx, key, "config", status); err != nil {
			log.Printf("SYNC: post project %s: %v", name, err)
			// Continue to next project even if one fails.
		}
	}
	return rows.Err()
}

// postMemory POSTs a memory to the DuckBrain HTTP API.
// URL: {baseURL}/api/memories?namespace={namespace}
// Body: {"key": key, "domain": domain, "content": <JSON of content>, "attributes": {}}
func (d *DuckBrainSync) postMemory(ctx context.Context, key, domain string, content any) error {
	payload, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal content: %w", err)
	}

	body := map[string]any{
		"key":        key,
		"domain":     domain,
		"content":    string(payload),
		"attributes": map[string]any{},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	url := fmt.Sprintf("%s/api/memories?namespace=%s", d.baseURL, d.namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("duckbrain api returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
