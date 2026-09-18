package scheduler

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// CTL-002 — the scheduler's own event path must feed the live event stream.
// EventLogger.Emit used to INSERT into the events table directly, which meant
// every loop/alert event bypassed the fan-out and never reached a connected SSE
// client; it now writes through database.LogEvent, the single write path that
// publishes committed rows.

func newEmitterTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestEventLoggerEmit_PublishesToSubscribers(t *testing.T) {
	db := newEmitterTestDB(t)
	el := NewEventLogger(db)

	events, unsubscribe := database.SubscribeEvents(db, 8)
	defer unsubscribe()

	el.Emit(context.Background(), SeverityHigh, "loop", "escalation", map[string]any{"project": "alpha"})

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("subscriber channel closed")
		}
		if ev.Severity != database.SeverityHigh || ev.Component != "loop" || ev.Message != "escalation" {
			t.Errorf("published event = %+v, want HIGH/loop/escalation", ev)
		}
		if ev.ID == 0 {
			t.Error("published event has no committed ID")
		}
		if ev.Details != `{"project":"alpha"}` {
			t.Errorf("published details = %q, want the marshalled payload", ev.Details)
		}
		if ev.CreatedAt == "" {
			t.Error("published event has no created_at")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EventLogger.Emit did not publish to subscribers")
	}
}

// TestEventLoggerEmit_RowShapeUnchanged: routing Emit through LogEvent must not
// change what lands in the table.
func TestEventLoggerEmit_RowShapeUnchanged(t *testing.T) {
	db := newEmitterTestDB(t)
	el := NewEventLogger(db)

	el.Emit(context.Background(), SeverityInfo, "loop", "evaluation started", nil)

	var severity, component, message, details, createdAt string
	if err := db.QueryRow(
		`SELECT severity, component, message, details, created_at FROM events ORDER BY id DESC LIMIT 1`,
	).Scan(&severity, &component, &message, &details, &createdAt); err != nil {
		t.Fatalf("read back event row: %v", err)
	}
	if severity != "INFO" || component != "loop" || message != "evaluation started" {
		t.Errorf("row = %s/%s/%s, want INFO/loop/evaluation started", severity, component, message)
	}
	if details != "{}" {
		t.Errorf("details = %q, want {} for a nil details map", details)
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Errorf("created_at = %q, want RFC3339: %v", createdAt, err)
	}
}
