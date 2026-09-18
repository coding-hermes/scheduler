package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"

	"github.com/coding-hermes/scheduler/internal/database"
)

// EventSeverity follows the alert escalation matrix from OBS-006.
type EventSeverity string

const (
	SeverityCritical EventSeverity = "CRITICAL" // scheduler down, data loss
	SeverityHigh     EventSeverity = "HIGH"     // >3 projects failing
	SeverityMedium   EventSeverity = "MEDIUM"   // project starved
	SeverityLow      EventSeverity = "LOW"      // single failure
	SeverityInfo     EventSeverity = "INFO"     // normal operation
)

// EventLogger writes structured events to the events table.
type EventLogger struct {
	db *sql.DB
}

// NewEventLogger creates a logger backed by db.
func NewEventLogger(db *sql.DB) *EventLogger {
	return &EventLogger{db: db}
}

// Emit writes an event row through the database package's single write path
// (database.LogEvent), which also publishes the committed row to the live
// event subscribers (CTL-002) — so the scheduler loop's events reach
// GET /api/v1/events/stream exactly like the API-, MCP- and sync-written ones.
// Non-blocking — errors are logged but not returned, so event logging never
// breaks the hot path.
func (el *EventLogger) Emit(ctx context.Context, severity EventSeverity, component, message string, details map[string]any) {
	detailsJSON := "{}"
	if details != nil {
		b, err := json.Marshal(details)
		if err == nil {
			detailsJSON = string(b)
		}
	}

	// Same columns and the same RFC3339 UTC timestamp as the previous direct
	// INSERT; LogEvent fills created_at in when it is empty.
	ev := database.Event{
		Severity:  database.EventSeverity(severity),
		Component: component,
		Message:   message,
		Details:   detailsJSON,
	}
	if err := database.LogEvent(ctx, el.db, &ev); err != nil {
		log.Printf("EVENT: failed to write event [%s] %s: %v", severity, message, err)
	}
}
