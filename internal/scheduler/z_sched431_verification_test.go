package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/lanehook"
)

func TestVerificationSched431SessionResolutionAndNullSafety(t *testing.T) {
	cases := []struct {
		name, rowSession, traceSession, want string
	}{
		{"durable real session wins", "real-session", "trace-session", "real-session"},
		{"trace fallback when row is NULL", "", "trace-session", "trace-session"},
		{"trace fallback when row holds tick placeholder", "<tick>", "trace-session", "trace-session"},
		{"NULL row and missing trace is empty", "", "", ""},
		{"placeholder row and missing trace is empty", "<tick>", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.InitDB(filepath.Join(t.TempDir(), "verify.db"))
			if err != nil {
				t.Fatalf("open test db: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := database.CreateProject(context.Background(), db, &database.Project{Name: "verify", RepoURL: "local:/tmp/verify", Workdir: "/tmp/verify", Weight: 1, Priority: 1, CooldownS: 1, DecayRate: 1, Model: "test", Provider: "test", Enabled: true}); err != nil {
				t.Fatalf("insert project: %v", err)
			}
			tickID := "tick-" + tc.name
			trace := "{}"
			if tc.traceSession != "" {
				trace = `{"session_id":"` + tc.traceSession + `"}`
			}
			if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at, session_id, gateway_trace) VALUES (?, 'verify', 'completed', ?, NULLIF(?, ''), ?)`, tickID, time.Now().UTC().Format(time.RFC3339), tc.rowSession, trace); err != nil {
				t.Fatalf("insert tick: %v", err)
			}
			p := &SlotPool{lifecycle: &LifecycleTracker{db: db}}
			gotSession := p.hookSessionID(tickID)
			if tc.rowSession == "<tick>" {
				// The production placeholder is the tick ID itself.
				_, _ = db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, tickID, tickID)
				gotSession = p.hookSessionID(tickID)
			}
			if gotSession != tc.want {
				t.Fatalf("hookSessionID = %q, want %q", gotSession, tc.want)
			}
			got := lanehook.Expand([]string{"{session}", "CODING_HERMES_SESSION={session}"}, lanehook.Env{SessionID: gotSession})
			if tc.want == "" && (got[0] == "null" || got[0] == "NULL" || got[1] == "CODING_HERMES_SESSION=null" || got[1] == "CODING_HERMES_SESSION=NULL") {
				t.Fatalf("NULL session rendered as a literal null: %#v", got)
			}
		})
	}
}
