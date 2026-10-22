package dashboard_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/dashboard"
)

// ch:trace row=SCHED-GAP-1642 spec=none:board-row-and-SCHED-GAP-1641 wave=.coding-hermes/waves/coding-hermes-scheduler-foreman-2026-10-22-21-23-23.json#SCHED-GAP-1642 test=internal/dashboard/*_test.go::Burst doc=none:implementation-docs-if-needed prompt=/tmp/sched-20261022212323-SCHED-GAP-1642-brief.txt evidence=commit witness=none:foreman-verifies-dashboard-suite verdict=none:pending-foreman-trace commit=none:pending-foreman memory=none:not-written
func TestSCHEDGAP1642_BurstActivityRendersRecentAccounting(t *testing.T) {
	now := time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC)
	tests := []struct {
		name string
		rows []struct {
			id     string
			age    time.Duration
			reason string
		}
		wantCount  int
		wantMarkup string
	}{
		{
			name: "recent instant turns only",
			rows: []struct {
				id     string
				age    time.Duration
				reason string
			}{
				{id: "recent-a", age: 2 * time.Hour, reason: "instant_turn"},
				{id: "recent-b", age: 23 * time.Hour, reason: "instant_turn"},
				{id: "old", age: 25 * time.Hour, reason: "instant_turn"},
				{id: "legacy", age: time.Hour, reason: ""},
			},
			wantCount:  2,
			wantMarkup: "One-shot activity (24h)",
		},
		{
			name: "legacy and other accounting stay absent",
			rows: []struct {
				id     string
				age    time.Duration
				reason string
			}{
				{id: "legacy", age: time.Hour, reason: ""},
				{id: "other", age: 2 * time.Hour, reason: "transport_error"},
			},
			wantCount:  0,
			wantMarkup: "One-shot activity (24h)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			mustCreateProject(t, db, "burst-project", 10, 5)
			for _, row := range tc.rows {
				mustCreateTick(t, db, row.id, "burst-project", now.Add(-row.age))
				if _, err := db.ExecContext(context.Background(), `UPDATE ticks SET failure_reason = ?, completed_at = ? WHERE id = ?`, row.reason, now.Add(-row.age).Add(time.Minute).Format(time.RFC3339), row.id); err != nil {
					t.Fatalf("stamp tick accounting %s: %v", row.id, err)
				}
			}

			g := dashboard.NewGenerator(db, nil)
			g.SetClock(clock.NewFixed(now))
			data := g.OverviewDataForTest(context.Background())
			if data.InstantTurnCount != tc.wantCount {
				t.Errorf("recent one-shot count = %d, want %d", data.InstantTurnCount, tc.wantCount)
			}

			var page strings.Builder
			if err := g.Generate(&page); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !strings.Contains(page.String(), tc.wantMarkup) {
				t.Errorf("rendered dashboard missing %q: %s", tc.wantMarkup, snippet(page.String(), "One-shot"))
			}
			wantValue := fmt.Sprintf(">%d</div>", tc.wantCount)
			if !strings.Contains(page.String(), wantValue) {
				t.Errorf("rendered dashboard missing one-shot count %q: %s", wantValue, snippet(page.String(), "One-shot"))
			}
		})
	}
}
