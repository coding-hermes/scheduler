package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// DuckBrainSync pushes fleet state to DuckBrain as a read replica
// via its HTTP REST API. Writes that fail are spooled to SQLite and
// replayed once DuckBrain is reachable — a write is never dropped
// silently (Bane 2026-08-01: DuckBrain was stdio-only, :3000 dead,
// every write was failing with no fallback).
type DuckBrainSync struct {
	db         *sql.DB
	namespace  string
	baseURL    string
	httpClient *http.Client
	interval   time.Duration

	mu             sync.Mutex
	reachable      bool   // last cycle reached DuckBrain
	consecutiveErr int    // consecutive failed post cycles
	lastErr        string // last error text
	lastOKAt       string // RFC3339 of last successful post
	spooled        int    // pending spooled writes (cached from count)
	alertedDown    bool   // HIGH event already emitted for current outage

	// pendingSpool buffers failed writes during a sync cycle. They are
	// flushed to sync_spool AFTER all syncs complete, because sync
	// functions iterate query rows while holding the single DB connection
	// (SetMaxOpenConns(1)) — writing to the same DB mid-iteration would
	// deadlock. In-memory buffer keeps the fallback safe (Bane 2026-08-01).
	pendingSpool []spoolItem
	// pendingEvents buffers one-shot HIGH/recovery events for the cycle.
	pendingEvents []pendingSyncEvent
	// lastPayloads holds the canonical hash (synced_at stripped) of the last
	// successfully posted payload per key. Unchanged payloads are SKIPPED on
	// later cycles — the fleet sync unconditionally re-posts every project,
	// namespace, event and tick every 5 minutes, which combined with
	// DuckBrain's per-write auto-commits produced ~45k git commits/day and
	// 490GB of loose objects in the coding-hermes namespace (2026-08-06).
	lastPayloads map[string]string
}

// spoolItem is a buffered failed write awaiting DB persistence.
type spoolItem struct {
	key     string
	domain  string
	content string
}

// pendingSyncEvent is a deferred event-log write (HIGH alert or INFO
// recovery) flushed after the sync cycle's row iterations finish.
type pendingSyncEvent struct {
	severity database.EventSeverity
	message  string
	details  string
}

// HealthSnapshot is a thread-safe view of DuckBrain sync health for
// the status API and dashboard.
type HealthSnapshot struct {
	Reachable      bool   `json:"reachable"`
	ConsecutiveErr int    `json:"consecutive_failures"`
	LastError      string `json:"last_error,omitempty"`
	LastOKAt       string `json:"last_ok_at,omitempty"`
	Spooled        int    `json:"spooled_pending"`
	BaseURL        string `json:"base_url"`
	Interval       string `json:"interval"`
}

// NewDuckBrainSync creates a DuckBrain syncer.
// baseURL is the DuckBrain HTTP server URL (e.g., http://localhost:3000).
func NewDuckBrainSync(db *sql.DB, namespace, baseURL string) *DuckBrainSync {
	return &DuckBrainSync{
		db:           db,
		namespace:    namespace,
		baseURL:      baseURL,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		interval:     5 * time.Minute,
		lastPayloads: make(map[string]string),
	}
}

// Health returns a snapshot of sync health, safe for concurrent reads.
func (d *DuckBrainSync) Health() HealthSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return HealthSnapshot{
		Reachable:      d.reachable,
		ConsecutiveErr: d.consecutiveErr,
		LastError:      d.lastErr,
		LastOKAt:       d.lastOKAt,
		Spooled:        d.spooled,
		BaseURL:        d.baseURL,
		Interval:       d.interval.String(),
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

// syncOnce runs one sync cycle: replay spooled writes first (fallback
// recovery), then fleet summary + per-project statuses + namespaces
// + SDLC events + tick lifecycle.
func (d *DuckBrainSync) syncOnce(ctx context.Context) {
	log.Println("SYNC: running sync cycle")

	// Phase 0: replay anything spooled from previous failures. This is the
	// fallback path — spooled writes are re-attempted before fresh syncs so
	// nothing is lost when DuckBrain comes back online.
	replayed, replayErr := d.replaySpool(ctx)
	if replayErr != nil {
		log.Printf("SYNC: spool replay error: %v", replayErr)
	}
	if replayed > 0 {
		log.Printf("SYNC: replayed %d spooled write(s)", replayed)
	}

	if err := d.syncFleetSummary(ctx); err != nil {
		log.Printf("SYNC: fleet summary error: %v", err)
	}

	if err := d.syncProjectStatuses(ctx); err != nil {
		log.Printf("SYNC: project statuses error: %v", err)
	}

	if err := d.syncNamespaceSummary(ctx); err != nil {
		log.Printf("SYNC: namespace summary error: %v", err)
	}

	if err := d.syncNamespaceStatuses(ctx); err != nil {
		log.Printf("SYNC: namespace statuses error: %v", err)
	}

	if err := d.syncSDLC(ctx); err != nil {
		log.Printf("SYNC: sdlc events error: %v", err)
	}

	if err := d.syncTickLifecycle(ctx); err != nil {
		log.Printf("SYNC: tick lifecycle error: %v", err)
	}

	// Flush buffered fallback state NOW, after all row iterations finished
	// (the single DB connection is free again). Failed writes land in
	// sync_spool for replay; alert/recovery events land in the event log.
	// A write is never dropped silently.
	d.flushPending(ctx)

	// Refresh cached spool depth for the health snapshot.
	if n, err := database.CountSpooledMemories(ctx, d.db); err == nil {
		d.mu.Lock()
		d.spooled = n
		d.mu.Unlock()
	}
}

// flushPending persists buffered spool writes + the one-shot alert/recovery
// event. Must be called with no open query rows on d.db (single connection).
func (d *DuckBrainSync) flushPending(ctx context.Context) {
	d.mu.Lock()
	spool := d.pendingSpool
	d.pendingSpool = nil
	events := d.pendingEvents
	d.pendingEvents = nil
	d.mu.Unlock()

	for _, it := range spool {
		if _, err := database.SpoolMemory(ctx, d.db, it.key, it.domain, it.content); err != nil {
			log.Printf("SYNC: spool %s failed (data may be lost): %v", it.key, err)
		} else {
			log.Printf("SYNC: spooled %s for replay after failure", it.key)
		}
	}
	for _, ev := range events {
		_ = database.LogEvent(ctx, d.db, &database.Event{
			Severity:  ev.severity,
			Component: "duckbrain-sync",
			Message:   ev.message,
			Details:   ev.details,
		})
	}
}

// replaySpool attempts every spooled write, oldest first. Successful
// replays are deleted; failed ones keep their attempt count and error.
// Returns the number replayed successfully.
func (d *DuckBrainSync) replaySpool(ctx context.Context) (int, error) {
	entries, err := database.ListSpooledMemories(ctx, d.db, 500)
	if err != nil {
		return 0, err
	}
	replayed := 0
	for _, e := range entries {
		// Parse the original content JSON back into raw bytes for posting.
		contentJSON := []byte(e.Content)
		body := map[string]any{
			"key":        e.MemKey,
			"domain":     e.Domain,
			"content":    string(contentJSON),
			"attributes": map[string]any{},
		}
		postErr := d.postMemoryBody(ctx, body, e.MemKey)
		if postErr != nil {
			_ = database.RecordSpoolAttempt(ctx, d.db, e.ID, postErr.Error())
			log.Printf("SYNC: replay %s failed (attempt %d): %v", e.MemKey, e.Attempts+1, postErr)
			continue
		}
		if err := database.DeleteSpooledMemory(ctx, d.db, e.ID); err != nil {
			log.Printf("SYNC: delete spooled %s: %v", e.MemKey, err)
			continue
		}
		replayed++
	}
	if _, err := database.PruneSpooledMemories(ctx, d.db, 50); err != nil {
		log.Printf("SYNC: prune spool: %v", err)
	}
	return replayed, nil
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
	Name          string  `json:"name"`
	Weight        int     `json:"weight"`
	Priority      int     `json:"priority"`
	Enabled       bool    `json:"enabled"`
	CooldownS     int     `json:"cooldown_s"`
	DecayRate     float64 `json:"decay_rate"`
	Model         string  `json:"model"`
	Provider      string  `json:"provider"`
	LastTick      string  `json:"last_tick"`
	LastTickStart string  `json:"last_tick_start"`
	SyncedAt      string  `json:"synced_at"`
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

// postMemory POSTs a memory to the DuckBrain HTTP API. (rest of method unchanged)

// ---------------------------------------------------------------------------
// Namespace sync
// ---------------------------------------------------------------------------

// namespaceSummary is the payload sent for /fleet/namespaces.
type namespaceSummary struct {
	Count         int    `json:"count"`
	TotalWeight   int    `json:"total_weight"`
	TotalReserved int    `json:"total_reserved"`
	SyncedAt      string `json:"synced_at"`
}

// syncNamespaceSummary queries aggregate namespace stats and pushes to DuckBrain.
func (d *DuckBrainSync) syncNamespaceSummary(ctx context.Context) error {
	var count, totalWeight, totalReserved int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(weight), 0), COALESCE(SUM(reserved), 0) FROM namespaces`).
		Scan(&count, &totalWeight, &totalReserved); err != nil {
		return fmt.Errorf("query namespace summary: %w", err)
	}

	summary := namespaceSummary{
		Count:         count,
		TotalWeight:   totalWeight,
		TotalReserved: totalReserved,
		SyncedAt:      time.Now().Format(time.RFC3339),
	}

	return d.postMemory(ctx, "/fleet/namespaces", "config", summary)
}

// namespaceStatus is the per-namespace payload sent to DuckBrain.
type namespaceStatus struct {
	ID          string `json:"id"`
	Weight      int    `json:"weight"`
	Reserved    int    `json:"reserved"`
	HardCap     int    `json:"hard_cap"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
	SyncedAt    string `json:"synced_at"`
}

// syncNamespaceStatuses queries all namespaces and pushes one memory each to DuckBrain.
func (d *DuckBrainSync) syncNamespaceStatuses(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, weight, reserved, hard_cap, enabled, COALESCE(description, '') FROM namespaces ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query namespaces: %w", err)
	}
	defer rows.Close()

	syncedAt := time.Now().Format(time.RFC3339)
	for rows.Next() {
		var id, desc string
		var weight, reserved, hardCap int
		var enabledInt int
		if err := rows.Scan(&id, &weight, &reserved, &hardCap, &enabledInt, &desc); err != nil {
			log.Printf("SYNC: scan namespace row: %v", err)
			continue
		}

		status := namespaceStatus{
			ID:          id,
			Weight:      weight,
			Reserved:    reserved,
			HardCap:     hardCap,
			Enabled:     enabledInt != 0,
			Description: desc,
			SyncedAt:    syncedAt,
		}

		key := "/fleet/namespaces/" + id + "/status"
		if err := d.postMemory(ctx, key, "config", status); err != nil {
			log.Printf("SYNC: post namespace %s: %v", id, err)
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// SDLC event sync
// ---------------------------------------------------------------------------

// sdlcEventLimit caps how many recent SDLC events are pushed per cycle.
const sdlcEventLimit = 50

// sdlcEvent is the payload sent to DuckBrain for a single event log entry.
// Field names mirror the database.Event struct.
type sdlcEvent struct {
	ID        int64  `json:"id"`
	Severity  string `json:"severity"`
	Component string `json:"component"`
	Message   string `json:"message"`
	Details   string `json:"details"`
	CreatedAt string `json:"created_at"`
}

// syncSDLC queries the most recent SDLC events from the event log and pushes
// one memory each to DuckBrain under /fleet/events/<id>. Per-event post
// failures are logged and skipped; the cycle never aborts on a single error.
func (d *DuckBrainSync) syncSDLC(ctx context.Context) error {
	events, err := database.ListEvents(ctx, d.db, "", "", sdlcEventLimit, 0)
	if err != nil {
		return fmt.Errorf("list sdlc events: %w", err)
	}

	for _, e := range events {
		payload := sdlcEvent{
			ID:        e.ID,
			Severity:  string(e.Severity),
			Component: e.Component,
			Message:   e.Message,
			Details:   e.Details,
			CreatedAt: e.CreatedAt,
		}

		key := fmt.Sprintf("/fleet/events/%d", e.ID)
		if err := d.postMemory(ctx, key, "event", payload); err != nil {
			log.Printf("SYNC: post event %d: %v", e.ID, err)
			// Continue to next event even if one fails.
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tick lifecycle sync
// ---------------------------------------------------------------------------

// tickLifecycleLimit caps how many recent ticks are pushed per cycle.
const tickLifecycleLimit = 25

// tickLifecycle is the payload sent to DuckBrain for a single tick lifecycle
// record. Field names mirror the database.Tick struct.
type tickLifecycle struct {
	ID           string  `json:"id"`
	ProjectName  string  `json:"project_name"`
	Status       string  `json:"status"`
	Outcome      string  `json:"outcome"`
	SpawnedAt    string  `json:"spawned_at"`
	CompletedAt  string  `json:"completed_at"`
	ExitCode     int     `json:"exit_code"`
	Commits      int     `json:"commits"`
	FilesChanged int     `json:"files_changed"`
	CostUSD      float64 `json:"cost_usd"`
	Urgency      float64 `json:"urgency"`
	Error        string  `json:"error"`
}

// syncTickLifecycle queries the most recent ticks (newest-first) and pushes
// one memory each to DuckBrain under /fleet/projects/<name>/ticks/<id>.
// NULL columns are coalesced to zero values, matching syncProjectStatuses.
// Per-tick post failures are logged and skipped; the cycle never aborts on a
// single error.
func (d *DuckBrainSync) syncTickLifecycle(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, project_name, status,
			COALESCE(outcome, ''),
			COALESCE(spawned_at, ''),
			COALESCE(completed_at, ''),
			COALESCE(exit_code, 0),
			COALESCE(commits, 0),
			COALESCE(files_changed, 0),
			COALESCE(cost_usd, 0.0),
			COALESCE(urgency, 0.0),
			COALESCE(error, '')
		FROM ticks
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, tickLifecycleLimit)
	if err != nil {
		return fmt.Errorf("query ticks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var t tickLifecycle
		if err := rows.Scan(&t.ID, &t.ProjectName, &t.Status,
			&t.Outcome, &t.SpawnedAt, &t.CompletedAt, &t.ExitCode,
			&t.Commits, &t.FilesChanged, &t.CostUSD, &t.Urgency, &t.Error); err != nil {
			log.Printf("SYNC: scan tick row: %v", err)
			continue
		}

		key := "/fleet/projects/" + t.ProjectName + "/ticks/" + t.ID
		if err := d.postMemory(ctx, key, "event", t); err != nil {
			log.Printf("SYNC: post tick %s: %v", t.ID, err)
			// Continue to next tick even if one fails.
		}
	}
	return rows.Err()
}

// URL: {baseURL}/api/memories?namespace={namespace}
// Body: {"key": key, "domain": domain, "content": <JSON of content>, "attributes": {}}
// canonicalPayloadHash returns a stable hash of a payload with the volatile
// synced_at field stripped, so "nothing changed since last cycle" is detected
// despite the timestamp that changes on every marshal.
func canonicalPayloadHash(content any) (string, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	delete(m, "synced_at")
	canon, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func (d *DuckBrainSync) postMemory(ctx context.Context, key, domain string, content any) error {
	payload, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal content: %w", err)
	}

	// Change detection: skip the POST entirely when the payload (minus the
	// volatile synced_at) is unchanged since the last successful post. Only a
	// SUCCESSFUL post records the hash, so a failed write is always retried.
	hash, hashErr := canonicalPayloadHash(content)
	if hashErr == nil {
		d.mu.Lock()
		prev, seen := d.lastPayloads[key]
		d.mu.Unlock()
		if seen && prev == hash {
			return nil // unchanged — nothing to sync
		}
	}

	body := map[string]any{
		"key":        key,
		"domain":     domain,
		"content":    string(payload),
		"attributes": map[string]any{},
	}

	postErr := d.postMemoryBody(ctx, body, key)
	if postErr != nil {
		// FALLBACK: never drop a write silently. Buffer it for spooling —
		// it is persisted to sync_spool by flushPending at the end of the
		// cycle and replayed once DuckBrain is reachable again. This is
		// what was missing while DuckBrain ran stdio-only with no HTTP
		// listener — every tick's memory write failed and vanished
		// (Bane 2026-08-01).
		d.bufferSpool(key, domain, string(payload))
		d.recordFailure(postErr.Error())
		return postErr
	}
	d.recordSuccess()
	// Remember the successful payload so unchanged data is skipped next cycle.
	if hashErr == nil {
		d.mu.Lock()
		d.lastPayloads[key] = hash
		d.mu.Unlock()
	}
	return nil
}

// bufferSpool queues a failed write for persistence. In-memory only —
// the DB write happens in flushPending (single-connection deadlock guard).
func (d *DuckBrainSync) bufferSpool(key, domain, content string) {
	d.mu.Lock()
	d.pendingSpool = append(d.pendingSpool, spoolItem{key: key, domain: domain, content: content})
	d.mu.Unlock()
}

// postMemoryBody posts a raw memory envelope to DuckBrain and returns the
// transport/app error. It does NOT spool — callers decide fallback policy.
func (d *DuckBrainSync) postMemoryBody(ctx context.Context, body map[string]any, key string) error {
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
	// Optional DuckBrain API-key auth (DB-GAP-039): when the daemon runs
	// with --auth=apikey it requires the X-API-Key header. Unset/empty env
	// keeps the pre-auth behavior exactly — no header is sent.
	if tok := os.Getenv("DUCKBRAIN_API_KEY"); tok != "" {
		req.Header.Set("X-API-Key", tok)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		// Rate limited (DuckBrain default 100/min; fleet burst can exceed).
		// Retryable — stop the burst; remaining writes spool for next cycle.
		return fmt.Errorf("duckbrain rate limited (429)")
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("duckbrain api returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// recordSuccess updates health after a successful write and queues a
// recovery event if this ends a previous outage (flushed end-of-cycle).
func (d *DuckBrainSync) recordSuccess() {
	d.mu.Lock()
	wasDown := d.consecutiveErr > 0
	d.reachable = true
	d.consecutiveErr = 0
	d.lastErr = ""
	d.lastOKAt = time.Now().Format(time.RFC3339)
	d.alertedDown = false
	if wasDown {
		// Recovery event — queued (flushed end-of-cycle with any HIGH).
		d.pendingEvents = append(d.pendingEvents, pendingSyncEvent{
			severity: database.SeverityInfo,
			message:  "DuckBrain reachable again — sync recovered",
			details:  `{"recovered_at": "` + time.Now().Format(time.RFC3339) + `"}`,
		})
	}
	d.mu.Unlock()

	if wasDown {
		log.Printf("SYNC: DuckBrain reachable again (recovery)")
	}
}

// recordFailure updates health after a failed write and queues a HIGH
// alert event on the first failure of a new outage (flushed end-of-cycle,
// not on every retry).
func (d *DuckBrainSync) recordFailure(errText string) {
	d.mu.Lock()
	d.reachable = false
	d.consecutiveErr++
	d.lastErr = errText
	first := !d.alertedDown
	d.alertedDown = true
	if first {
		// First failure of a new outage — queued (flushed end-of-cycle).
		d.pendingEvents = append(d.pendingEvents, pendingSyncEvent{
			severity: database.SeverityHigh,
			message:  "DuckBrain unreachable — writes spooled for replay",
			details:  `{"error": "` + errText + `", "base_url": "` + d.baseURL + `"}`,
		})
	}
	d.mu.Unlock()

	if first {
		log.Printf("SYNC: DuckBrain unreachable: %s", errText)
	}
}
