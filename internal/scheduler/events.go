package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"
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

// Emit writes an event row to the events table. Non-blocking — errors are
// logged but not returned, so event logging never breaks the hot path.
func (el *EventLogger) Emit(ctx context.Context, severity EventSeverity, component, message string, details map[string]any) {
	detailsJSON := "{}"
	if details != nil {
		b, err := json.Marshal(details)
		if err == nil {
			detailsJSON = string(b)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := el.db.ExecContext(ctx, `
		INSERT INTO events (severity, component, message, details, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, string(severity), component, message, detailsJSON, now)

	if err != nil {
		log.Printf("EVENT: failed to write event [%s] %s: %v", severity, message, err)
	}
}
