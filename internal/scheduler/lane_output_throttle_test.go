package scheduler

import (
	"testing"
)

// TestLaneOutput_HighEventEmittedOncePerCrossing pins the throttle: the HIGH
// lane-output event fires at the threshold crossing, not on every subsequent
// zero-output tick — a lane stuck at streak 20 must not have 13 events. A
// re-crossing after a reset emits again (once per crossing).
func TestLaneOutput_HighEventEmittedOncePerCrossing(t *testing.T) {
	db := newTestDB(t)
	insertLaneOutputProject(t, db, "lane-pm", "pm")

	// 11 consecutive zero-output ticks — 3 past the threshold of 8.
	for i := 1; i <= 11; i++ {
		id := "t-x-" + string(rune('a'+i))
		insertLaneOutputTick(t, db, id, "lane-pm", "completed", 0, 0, 0)
		recordLaneFamilyOutput(db, "lane-pm", id)
	}
	if n := countLaneOutputHighEvents(t, db, "lane-pm"); n != 1 {
		t.Fatalf("HIGH lane-output events after 11 zero ticks = %d, want exactly 1 (once per crossing, not per tick)", n)
	}

	// An output tick resets the streak...
	insertLaneOutputTick(t, db, "t-x-ok", "lane-pm", "completed", 1, 0, 1)
	recordLaneFamilyOutput(db, "lane-pm", "t-x-ok")

	// ...so a fresh 8-tick crossing emits exactly one more event (2 total).
	for i := 1; i <= 8; i++ {
		id := "t-y-" + string(rune('a'+i))
		insertLaneOutputTick(t, db, id, "lane-pm", "completed", 0, 0, 0)
		recordLaneFamilyOutput(db, "lane-pm", id)
	}
	if n := countLaneOutputHighEvents(t, db, "lane-pm"); n != 2 {
		t.Fatalf("HIGH lane-output events after re-crossing = %d, want exactly 2 (one per crossing)", n)
	}
}
