package dashboard_test

// SCHED-GAP-1594 — the /health panel's Tick Push card renders the
// live-vs-pushed state: "pushed" with the SCHED-GAP-1694 note when the
// spawner pushes at tick exit, "live only" with the web-primary note when
// --disable-tick-PUSH is armed. The card reads the same package state the
// spawner's push decision uses, so it can never disagree with what Wait()
// does.

import (
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func renderTickPushHealth(t *testing.T) string {
	t.Helper()
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := gen.GenerateHealth(&buf); err != nil {
		t.Fatalf("GenerateHealth: %v", err)
	}
	return buf.String()
}

func TestSCHEDGAP1594_HealthRendersPushedState(t *testing.T) {
	scheduler.SetTickPushDisabledDefaultForTest(false)
	t.Cleanup(func() { scheduler.SetTickPushDisabledDefaultForTest(false) })

	out := renderTickPushHealth(t)
	for _, want := range []string{"Tick Push", ">pushed<", "pushed at tick exit (SCHED-GAP-1694)"} {
		if !strings.Contains(out, want) {
			t.Errorf("health page missing %q in the push state", want)
		}
	}
	if strings.Contains(out, "web-primary") {
		t.Errorf("health page shows the web-primary note while pushes are ON")
	}
}

func TestSCHEDGAP1594_HealthRendersLocalOnlyState(t *testing.T) {
	scheduler.SetTickPushDisabledDefaultForTest(true)
	t.Cleanup(func() { scheduler.SetTickPushDisabledDefaultForTest(false) })

	out := renderTickPushHealth(t)
	for _, want := range []string{"Tick Push", ">live only<", "web-primary (--disable-tick-push)", "commits stay local until pushed"} {
		if !strings.Contains(out, want) {
			t.Errorf("health page missing %q in the web-primary state", want)
		}
	}
	if strings.Contains(out, ">pushed<") {
		t.Errorf("health page shows \"pushed\" while --disable-tick-push is armed")
	}
}
