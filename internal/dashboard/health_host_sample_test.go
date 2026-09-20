package dashboard_test

// ADV-R13 — regression tests for the dashboard's persisted host-sample
// render: the latest host_samples row must appear on /health, and a
// database with no samples must render an honest "unavailable" instead of
// a fabricated 0.00.

import (
	"context"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

func TestADVR13_HealthRendersLatestPersistedSample(t *testing.T) {
	db := newTestDB(t)
	if err := database.InsertHostSample(context.Background(), db, &database.HostSample{
		SampledAt:         "2031-05-04T03:02:01Z",
		Load1:             1.25,
		Load5:             0.75,
		Load15:            0.5,
		MemTotalBytes:     16308280 << 10,
		MemAvailableBytes: 8154140 << 10,
		Source:            "proc",
	}); err != nil {
		t.Fatalf("InsertHostSample: %v", err)
	}
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateHealth(&buf); err != nil {
		t.Fatalf("GenerateHealth: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Host Load (1/5/15m)",
		"1.25 / 0.75 / 0.50",
		"Host Memory Available",
		"7963 / 15926 MB",
		"source=proc",
		"2031-05-04T03:02:01Z",
		"latest persisted host sample",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("health page missing %q", want)
		}
	}
	if strings.Contains(out, ">unavailable<") {
		t.Errorf("health page shows \"unavailable\" although a sample is persisted")
	}
}

func TestADVR13_HealthRendersUnavailableWithoutSamples(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateHealth(&buf); err != nil {
		t.Fatalf("GenerateHealth: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "unavailable") != 2 {
		t.Errorf("health page without samples should render exactly 2 \"unavailable\" cards, found %d", strings.Count(out, "unavailable"))
	}
	if strings.Contains(out, "0.00") || strings.Contains(out, "0 / 0 MB") {
		t.Errorf("health page fabricates a zero reading without samples:\n%s", out)
	}
}
