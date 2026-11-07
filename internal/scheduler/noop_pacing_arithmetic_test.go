package scheduler

import "testing"

// TestTasksNoopCooldown_GrowthAndCap pins the pacing arithmetic: the first
// no-op costs the full floor, each further consecutive no-op doubles it, and
// the pause is bounded at 8x the floor (ADV-R10's derived ceiling shape) so a
// parked lane still retries and is never abandoned.
func TestTasksNoopCooldown_GrowthAndCap(t *testing.T) {
	const floor = 21600 // 6h — the measured lane's cooldown_floor_s

	cases := []struct {
		streak int
		want   int // seconds
	}{
		{streak: 0, want: 0},      // productive lane: no no-op pause at all
		{streak: 1, want: 21600},  // 6h  — cannot be respawned 2 minutes later
		{streak: 2, want: 43200},  // 12h
		{streak: 3, want: 86400},  // 24h
		{streak: 4, want: 172800}, // 48h = 8x floor
		{streak: 9, want: 172800}, // capped: still retries, never abandoned
	}
	for _, tc := range cases {
		got := int(tasksNoopCooldown(tc.streak, floor).Seconds())
		if got != tc.want {
			t.Fatalf("tasksNoopCooldown(streak=%d, floor=%d) = %ds, want %ds", tc.streak, floor, got, tc.want)
		}
	}

	// A lane with no configured floor has no no-op pause to claim: the
	// SCHED-GAP-136 pacing floor is the only spacing it can have, and inventing
	// one would be an operator-visible policy change on a lane nobody measured.
	if got := tasksNoopCooldown(3, 0); got != 0 {
		t.Fatalf("floor=0 must yield no no-op pause, got %v", got)
	}
}
