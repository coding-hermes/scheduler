package dashboard_test

// SCHED-GAP-1594 — the health panel's "Tick Updates" indicator must state
// HOW completed ticks reach the operator: "pushed" = per-tick git push at
// tick exit (SCHED-GAP-1694, the default); "live" = --disable-tick-push is
// set and the web dashboard is the primary update surface (no per-tick
// push; the fleet-strand-push cron is the only remaining pusher). The
// indicator must never be absent — an operator who cannot tell whether
// pushes are happening cannot trust the fleet page.

import (
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
)

func renderHealth(t *testing.T, gen *dashboard.Generator) string {
	t.Helper()
	var buf strings.Builder
	if err := gen.GenerateHealth(&buf); err != nil {
		t.Fatalf("GenerateHealth: %v", err)
	}
	return buf.String()
}

// TestSCHEDGAP1594_HealthShowsPushedState — the default (no flag): the card
// reads "pushed" and names the per-tick push behavior.
func TestSCHEDGAP1594_HealthShowsPushedState(t *testing.T) {
	gen := dashboard.NewGenerator(newTestDB(t), nil)
	out := renderHealth(t, gen)
	if !strings.Contains(out, ">pushed<") {
		t.Fatal("health page must render the 'pushed' tick-update state by default; got:\n" + out[:min(len(out), 2000)])
	}
	if !strings.Contains(out, "per-tick git push on") {
		t.Fatal("health page must explain the pushed state (per-tick git push on)")
	}
}

// TestSCHEDGAP1594_HealthShowsLiveState — with SetTickPushDisabled(true)
// (main.go passes the resolved --disable-tick-push value), the card reads
// "live" and names the web-primary behavior.
func TestSCHEDGAP1594_HealthShowsLiveState(t *testing.T) {
	gen := dashboard.NewGenerator(newTestDB(t), nil)
	gen.SetTickPushDisabled(true)
	out := renderHealth(t, gen)
	if !strings.Contains(out, ">live<") {
		t.Fatal("health page must render the 'live' tick-update state when tick push is disabled")
	}
	if !strings.Contains(out, "web-primary") {
		t.Fatal("health page must explain the live state (web-primary: dashboard updates live)")
	}
	if strings.Contains(out, ">pushed<") {
		t.Fatal("live state must not also render 'pushed' — the card states exactly one mode")
	}
}
