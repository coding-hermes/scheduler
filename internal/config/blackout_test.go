package config

import (
	"testing"
	"time"
)

func TestActiveMultiplier_NoWindows(t *testing.T) {
	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(nil, now)
	if mult != 1.0 || inBlackout {
		t.Errorf("no windows: got (%v,%v), want (1.0,false)", mult, inBlackout)
	}
}

func TestActiveMultiplier_InsideWindow(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("inside window: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_OutsideWindow(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 7, 30, 5, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 1.0 || inBlackout {
		t.Errorf("outside window: got (%v,%v), want (1.0,false)", mult, inBlackout)
	}
}

func TestActiveMultiplier_SkipMode(t *testing.T) {
	// Multiplier <= 0 means skip entirely
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 0},
	}
	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 0 || !inBlackout {
		t.Errorf("skip mode: got (%v,%v), want (0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_BoundaryStart(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("boundary start: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_BoundaryEnd(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 1.0 || inBlackout {
		t.Errorf("boundary end (exclusive): got (%v,%v), want (1.0,false)", mult, inBlackout)
	}
}

func TestActiveMultiplier_MultipleWindows(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
		{Start: "06:00", End: "10:00", Multiplier: 2.0},
	}
	// Inside first window
	now := time.Date(2026, 7, 30, 2, 30, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("first window: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
	// Gap between windows
	now = time.Date(2026, 7, 30, 5, 0, 0, 0, time.UTC)
	mult, inBlackout = ActiveMultiplier(windows, now)
	if mult != 1.0 || inBlackout {
		t.Errorf("gap: got (%v,%v), want (1.0,false)", mult, inBlackout)
	}
	// Inside second window
	now = time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	mult, inBlackout = ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("second window: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_DifferentMultipliers(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 3.0}, // triple slowdown
		{Start: "06:00", End: "10:00", Multiplier: 1.5}, // 50% slowdown
	}
	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 3.0 || !inBlackout {
		t.Errorf("triple: got (%v,%v), want (3.0,true)", mult, inBlackout)
	}
	now = time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	mult, inBlackout = ActiveMultiplier(windows, now)
	if mult != 1.5 || !inBlackout {
		t.Errorf("1.5x: got (%v,%v), want (1.5,true)", mult, inBlackout)
	}
}

// INT-CI-169 regression tests: overnight blackout windows must match on
// BOTH their late-night side (before midnight) and their early-morning
// side (after midnight, on the next UTC day). All times are FIXED — the
// suite must stay wall-clock independent. Note that 2026-09-30 00:30 UTC
// is a Wednesday, so WeekdaysOnly windows stay active here.
func TestActiveMultiplier_OvernightEarlyMorningSide(t *testing.T) {
	// 00:30 UTC sits on the early-morning side of the 23:00-02:00 window.
	windows := []BlackoutWindow{
		{Start: "23:00", End: "02:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 9, 30, 0, 30, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("overnight early-morning side: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_OvernightEarlyMorningSkipMode(t *testing.T) {
	windows := []BlackoutWindow{
		{Start: "23:00", End: "02:00", Multiplier: 0},
	}
	now := time.Date(2026, 9, 30, 0, 30, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 0 || !inBlackout {
		t.Errorf("overnight early-morning skip mode: got (%v,%v), want (0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_OvernightLateNightSide(t *testing.T) {
	// 23:30 UTC is the late-night side of the same window — regression guard.
	windows := []BlackoutWindow{
		{Start: "23:00", End: "02:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 9, 29, 23, 30, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("overnight late-night side: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestActiveMultiplier_OvernightAfterWindowEnd(t *testing.T) {
	// 03:00 UTC is past the window's end (02:00) — must NOT match.
	windows := []BlackoutWindow{
		{Start: "23:00", End: "02:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 1.0 || inBlackout {
		t.Errorf("after overnight window end: got (%v,%v), want (1.0,false)", mult, inBlackout)
	}
}

func TestActiveMultiplier_PreviousDayAnchorNoFalsePositives(t *testing.T) {
	// At 12:00 UTC, a 01:00-04:00 window must not match via the previous-day
	// anchor (same-day sanity); 11:00-13:00 must still match normally.
	windows := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
		{Start: "11:00", End: "13:00", Multiplier: 2.0},
	}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	mult, inBlackout := ActiveMultiplier(windows, now)
	if mult != 2.0 || !inBlackout {
		t.Errorf("same-day window: got (%v,%v), want (2.0,true)", mult, inBlackout)
	}
}

func TestParseHM(t *testing.T) {
	tests := []struct {
		input string
		wantH int
		wantM int
	}{
		{"01:00", 1, 0},
		{"23:59", 23, 59},
		{"0:0", 0, 0},
		{"bad", 0, 0},
	}
	for _, tt := range tests {
		h, m := parseHM(tt.input)
		if h != tt.wantH || m != tt.wantM {
			t.Errorf("parseHM(%q) = (%d,%d), want (%d,%d)", tt.input, h, m, tt.wantH, tt.wantM)
		}
	}
}
