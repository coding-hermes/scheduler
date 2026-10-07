package main

import (
	"testing"
	"time"
)

// SCHED-GAP-1629: the bulk-sim context window derives from --sim-count
// (ceil(count/8) beats of 500ms + 10s headroom) instead of a flat 30s that
// FATALed at ~480 ticks.
func TestBulkSimWindow(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{1, 10*time.Second + BulkSimBeat},
		{8, 10*time.Second + BulkSimBeat},        // exactly one full beat
		{9, 10*time.Second + 2*BulkSimBeat},      // ceil rounds up
		{2000, 250*BulkSimBeat + 10*time.Second}, // 2000/8 = 250 beats
	}
	for _, c := range cases {
		if got := BulkSimWindow(c.count); got != c.want {
			t.Errorf("BulkSimWindow(%d) = %v, want %v", c.count, got, c.want)
		}
	}
	// Old behavior regression: the flat 30s window is the floor for large
	// counts, and 2000 ticks must get far more than the old 30s (~480-tick
	// ceiling that produced "context deadline exceeded").
	if w := BulkSimWindow(2000); w <= 30*time.Second {
		t.Errorf("BulkSimWindow(2000) = %v, must exceed the old flat 30s window", w)
	}
}
