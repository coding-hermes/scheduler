package dashboard_test

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

func TestResolveProjectDetailNameUsesExactLaneBeforeForemanFallback(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"coding-hermes-scheduler-foreman", "existing-foreman", "existing"} {
		if err := database.CreateProject(context.Background(), db, &database.Project{
			Name: name, Workdir: "/tmp/" + name, Weight: 1, Priority: 1, CooldownS: 900, DecayRate: 1,
		}); err != nil {
			t.Fatalf("create project %q: %v", name, err)
		}
	}
	g := dashboard.NewGenerator(db, nil)
	cases := []struct {
		input        string
		want         string
		wantRedirect bool
		wantNotFound bool
	}{
		{input: "coding-hermes-scheduler", want: "coding-hermes-scheduler-foreman", wantRedirect: true},
		{input: "existing", want: "existing", wantRedirect: false},
		{input: "existing-foreman", want: "existing-foreman", wantRedirect: false},
		{input: "missing", wantNotFound: true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, redirect, err := g.ResolveProjectDetailName(tc.input)
			if err != nil {
				t.Fatalf("ResolveProjectDetailName: %v", err)
			}
			if tc.wantNotFound {
				if got != "" || redirect {
					t.Fatalf("got (%q, redirect=%t), want not found", got, redirect)
				}
				return
			}
			if got != tc.want || redirect != tc.wantRedirect {
				t.Fatalf("got (%q, redirect=%t), want (%q, redirect=%t)", got, redirect, tc.want, tc.wantRedirect)
			}
		})
	}
}
