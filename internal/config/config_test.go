package config

import (
	"testing"
	"time"
)

func TestActiveMultiplierWeekdaysOnly(t *testing.T) {
	wins := []BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0, WeekdaysOnly: true},
	}
	// Wed 2026-08-19 02:00 UTC = Beijing Wed 10:00 (peak window, weekday)
	wed := time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC)
	if m, _ := ActiveMultiplier(wins, wed); m != 2.0 {
		t.Fatalf("weekday peak: got %v want 2.0", m)
	}
	// Sat 2026-08-22 02:00 UTC = Beijing Sat 10:00 (weekend — off-peak all day)
	sat := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	if m, _ := ActiveMultiplier(wins, sat); m != 1.0 {
		t.Fatalf("weekend: got %v want 1.0 (off-peak)", m)
	}
	// Fri 2026-08-21 18:00 UTC = Beijing Sat 02:00 (already weekend in Beijing)
	fri_evening := time.Date(2026, 8, 21, 18, 0, 0, 0, time.UTC)
	if m, _ := ActiveMultiplier(wins, fri_evening); m != 1.0 {
		t.Fatalf("fri-evening (bj sat): got %v want 1.0", m)
	}
}
