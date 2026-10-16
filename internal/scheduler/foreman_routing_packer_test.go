package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestHarness001ProductionPackers(t *testing.T) {
	db := newTestDB(t)
	// Disabled ownership evidence must still classify a parented owner as foreman.
	for _, row := range []struct {
		name, parent string
		enabled      bool
	}{
		{"root", "", true}, {"owner", "root", true}, {"owner-qa", "owner", false},
		{"root-review", "root", true}, {"helper", "root", true},
	} {
		_, err := db.Exec(`INSERT INTO projects(name,parent,enabled,weight,priority,cooldown_s,decay_rate,repo_url,workdir,created_at,updated_at) VALUES(?,?,?,1,5,0,1,'',?,?,CURRENT_TIMESTAMP)`, row.name, row.parent, row.enabled, t.TempDir(), time.Now().Add(-24*time.Hour).Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
	}
	projects, err := database.ListProjects(context.Background(), db, false)
	if err != nil {
		t.Fatal(err)
	}
	calc := NewUrgencyCalculator(time.Second, time.Hour, 5)
	want := map[string]string{"root": LaneClassForeman, "owner": LaneClassForeman, "root-review": LaneClassSatellite, "helper": LaneClassSatellite}
	check := func(t *testing.T, got []PackedProject) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("packed=%v want %d lanes", got, len(want))
		}
		for _, p := range got {
			if p.LaneClass != want[p.Name] {
				t.Errorf("%s class=%q want %q", p.Name, p.LaneClass, want[p.Name])
			}
		}
	}
	t.Run("db-flat", func(t *testing.T) {
		got, err := NewPacker(db, calc, 100, 10, nil).Pick(time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, got)
	})
	t.Run("list-enabled", func(t *testing.T) {
		got, err := NewPacker(db, calc, 100, 10, nil).ListEnabled(nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, got)
	})
	t.Run("multipool-flat", func(t *testing.T) {
		got := NewMultiPoolPacker(100, 10, nil).Pack(projects, nil, calc, nil, nil, time.Now())
		check(t, got.Projects)
	})
	t.Run("multipool-namespace-and-unassigned", func(t *testing.T) {
		ns := "test-ns"
		for i := range projects {
			if projects[i].Name != "owner" {
				projects[i].NamespaceID = &ns
			}
		}
		got := NewMultiPoolPacker(100, 10, nil).Pack(projects, []database.Namespace{{ID: ns, Weight: 1, Reserved: 1, HardCap: 100, Enabled: true}}, calc, nil, nil, time.Now())
		check(t, got.Projects)
	})
	t.Run("manual-resume-conversion", func(t *testing.T) {
		l := &Loop{db: db}
		for _, p := range projects {
			if p.Enabled {
				checkP, err := l.packedProjectFrom(p)
				if err != nil {
					t.Fatal(err)
				}
				if checkP.LaneClass != want[p.Name] {
					t.Errorf("%s class=%q", p.Name, checkP.LaneClass)
				}
			}
		}
	})
}
