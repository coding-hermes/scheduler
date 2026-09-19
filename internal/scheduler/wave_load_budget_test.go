package scheduler

import (
	"strings"
	"testing"
)

// SCHED-GAP-170 — load-scaled WAVE_BUDGET (Bane 2026-09-19):
// "for each load average point below 12 we can launch 1 worker min 1 upto 12".

func setLoadForTest(t *testing.T, v float64, ok bool) {
	t.Helper()
	prev := currentLoad1m
	currentLoad1m = func() (float64, bool) { return v, ok }
	t.Cleanup(func() { currentLoad1m = prev })
}

func TestWaveLoadCapIdleBoxFullCap(t *testing.T) {
	setLoadForTest(t, 0.0, true)
	SetWaveLoadCeiling(12)
	t.Cleanup(func() { SetWaveLoadCeiling(0) })
	got, scaled := waveLoadCap(12)
	if !scaled || got != 12 {
		t.Fatalf("idle box: want full cap 12 scaled, got %d scaled=%v", got, scaled)
	}
}

func TestWaveLoadCapOnePerSparePoint(t *testing.T) {
	SetWaveLoadCeiling(12)
	t.Cleanup(func() { SetWaveLoadCeiling(0) })
	cases := []struct {
		load float64
		want int
	}{
		{8.0, 4},  // 12−8 = 4 workers
		{11.0, 1}, // 12−11 = 1
		{11.5, 1}, // floor(headroom)=0 → min-1 floor
		{13.0, 1}, // at/above ceiling → min-1 floor (the GATE defers the spawn itself)
	}
	for _, c := range cases {
		setLoadForTest(t, c.load, true)
		got, scaled := waveLoadCap(12)
		if !scaled || got != c.want {
			t.Fatalf("load %.1f: want %d scaled, got %d scaled=%v", c.load, c.want, got, scaled)
		}
	}
}

func TestWaveLoadCapNoTelemetryFailsOpen(t *testing.T) {
	SetWaveLoadCeiling(12)
	t.Cleanup(func() { SetWaveLoadCeiling(0) })
	setLoadForTest(t, 0, false) // no reading
	got, scaled := waveLoadCap(12)
	if scaled || got != 12 {
		t.Fatalf("no telemetry: want depth-only cap 12 unscaled, got %d scaled=%v", got, scaled)
	}
}

func TestWaveLoadCapDisabledGateSkipsScaling(t *testing.T) {
	SetWaveLoadCeiling(0) // gate disabled
	setLoadForTest(t, 11.9, true)
	got, scaled := waveLoadCap(12)
	if scaled || got != 12 {
		t.Fatalf("ceiling 0: want pre-170 behavior (12, unscaled), got %d scaled=%v", got, scaled)
	}
}

func TestWaveBudgetLineExactText(t *testing.T) {
	got := waveBudgetLine(7)
	want := "WAVE_BUDGET: 7 — max concurrent wave workers this tick (0 = serial tick, do not compose a wave)."
	if got != want {
		t.Fatalf("budget line drifted:\n got: %s\nwant: %s", got, want)
	}
	if !strings.Contains(waveBudgetReminder, "INDEPENDENT") {
		t.Fatalf("reminder must carry the independent-rows-only rule: %s", waveBudgetReminder)
	}
}
