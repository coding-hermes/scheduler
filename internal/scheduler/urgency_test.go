package scheduler

import (
	"testing"
	"time"
)

// NewUrgencyCalculator tests the constructor's clamping and warning behavior.
func TestNewUrgencyCalculator_BasicRange(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)
	if u == nil {
		t.Fatal("NewUrgencyCalculator returned nil")
	}
	if u.minInterval != 60*time.Second {
		t.Errorf("minInterval = %v, want 60s", u.minInterval)
	}
	if u.maxInterval != 60*time.Minute {
		t.Errorf("maxInterval = %v, want 60m", u.maxInterval)
	}
	if u.numLevels != 10 {
		t.Errorf("numLevels = %d, want 10", u.numLevels)
	}
	// 60m / 60s = 60
	wantRatio := 60.0
	if u.ratio != wantRatio {
		t.Errorf("ratio = %f, want %f", u.ratio, wantRatio)
	}
}

// TestNewUrgencyCalculator_ClampsNumLevels verifies that < 2 is clamped to 2.
func TestNewUrgencyCalculator_ClampsNumLevels(t *testing.T) {
	u := NewUrgencyCalculator(time.Second, time.Minute, 1)
	if u.numLevels != 2 {
		t.Errorf("numLevels = %d, want 2 (clamped)", u.numLevels)
	}
	u = NewUrgencyCalculator(time.Second, time.Minute, 0)
	if u.numLevels != 2 {
		t.Errorf("numLevels = %d, want 2 (clamped from 0)", u.numLevels)
	}
}

// TestNewUrgencyCalculator_InvalidRange verifies the ratio falls back to 1.0.
func TestNewUrgencyCalculator_InvalidRange(t *testing.T) {
	// min > max — invalid.
	u := NewUrgencyCalculator(time.Minute, time.Second, 5)
	if u.ratio != 1.0 {
		t.Errorf("ratio = %f, want 1.0 (fallback for invalid range)", u.ratio)
	}
	// min == 0 — invalid.
	u = NewUrgencyCalculator(0, time.Minute, 5)
	if u.ratio != 1.0 {
		t.Errorf("ratio = %f, want 1.0 (fallback for min=0)", u.ratio)
	}
}

// TestComputeInterval_Boundaries checks the priority-to-interval mapping.
func TestComputeInterval_Boundaries(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)

	// Priority 10 → minInterval (fastest).
	got := u.ComputeInterval(10)
	if got != 60*time.Second {
		t.Errorf("ComputeInterval(10) = %v, want 60s", got)
	}

	// Priority 1 → maxInterval (slowest).
	got = u.ComputeInterval(1)
	if got < 59*time.Minute || got > 61*time.Minute {
		t.Errorf("ComputeInterval(1) = %v, want ~60m", got)
	}

	// Out-of-range priorities are clamped.
	got = u.ComputeInterval(0)
	if got < 59*time.Minute || got > 61*time.Minute {
		t.Errorf("ComputeInterval(0) = %v, want ~60m (clamped to priority=1)", got)
	}
	got = u.ComputeInterval(100)
	if got != 60*time.Second {
		t.Errorf("ComputeInterval(100) = %v, want 60s (clamped to priority=10)", got)
	}
}

// TestComputeInterval_MonotonicInPriority verifies higher priority → shorter interval.
func TestComputeInterval_MonotonicInPriority(t *testing.T) {
	u := NewUrgencyCalculator(time.Second, 100*time.Second, 10)

	prev := 100 * time.Second // start from max
	for p := 1.0; p <= 10; p++ {
		got := u.ComputeInterval(p)
		if got > prev {
			t.Errorf("ComputeInterval(%.0f) = %v not <= previous %v", p, got, prev)
		}
		prev = got
	}
}

// TestComputeUrgency_BasicSanity verifies the formula priority * (1 + elapsed/interval)^decay.
func TestComputeUrgency_BasicSanity(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-1 * time.Hour)

	// elapsed = 1h, interval(priority=5) is somewhere between 60s and 60m.
	// urgency should be at least priority (no decay multiplier).
	got := u.ComputeUrgency(5, 1.0, now, nil, created)
	if got < 5.0 {
		t.Errorf("ComputeUrgency = %f, want >= 5 (priority)", got)
	}
}

// TestComputeUrgency_NilLastCompleted checks that nil falls back to createdAt.
func TestComputeUrgency_NilLastCompleted(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * time.Minute)
	last := now.Add(-1 * time.Minute)

	// With nil lastCompleted, elapsed comes from createdAt (30 minutes).
	// With lastCompleted set, elapsed comes from lastCompleted (1 minute).
	urgNil := u.ComputeUrgency(5, 1.0, now, nil, created)
	urgSet := u.ComputeUrgency(5, 1.0, now, &last, created)
	if urgNil <= urgSet {
		t.Errorf("urgency(nil last) = %f should be > urgency(set last) = %f", urgNil, urgSet)
	}
}

// TestComputeUrgency_NegativeDecay verifies that negative decay is clamped to 0.
func TestComputeUrgency_NegativeDecay(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-1 * time.Hour)

	// With decay=0, urgency = priority * 1.0 = priority.
	got := u.ComputeUrgency(5, 0, now, nil, created)
	if got != 5.0 {
		t.Errorf("ComputeUrgency(decay=0) = %f, want 5.0", got)
	}

	// Negative decay is clamped, should behave like decay=0.
	got = u.ComputeUrgency(5, -3.0, now, nil, created)
	if got != 5.0 {
		t.Errorf("ComputeUrgency(decay=-3) = %f, want 5.0 (clamped)", got)
	}
}

// TestComputeUrgency_FutureLastCompleted verifies future timestamps don't produce negative elapsed.
func TestComputeUrgency_FutureLastCompleted(t *testing.T) {
	u := NewUrgencyCalculator(60*time.Second, 60*time.Minute, 10)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour) // lastCompleted is in the future

	// Elapsed would be -1h, but clamped to 0. Base = 1+0 = 1, urgency = priority*1.
	got := u.ComputeUrgency(5, 1.0, now, &future, created)
	if got != 5.0 {
		t.Errorf("ComputeUrgency(future last) = %f, want 5.0", got)
	}
}

// TestSetRange_UpdatesRatio verifies SetRange recomputes the ratio.
func TestSetRange_UpdatesRatio(t *testing.T) {
	u := NewUrgencyCalculator(time.Second, time.Minute, 5)
	if u.ratio != 60.0 {
		t.Errorf("initial ratio = %f, want 60", u.ratio)
	}

	u.SetRange(10*time.Second, 100*time.Second)
	if u.ratio != 10.0 {
		t.Errorf("after SetRange ratio = %f, want 10", u.ratio)
	}
	if u.minInterval != 10*time.Second || u.maxInterval != 100*time.Second {
		t.Errorf("SetRange did not update fields: min=%v max=%v", u.minInterval, u.maxInterval)
	}

	// New interval should reflect the new range: priority 10 = minInterval.
	got := u.ComputeInterval(10)
	if got != 10*time.Second {
		t.Errorf("ComputeInterval(10) after SetRange = %v, want 10s", got)
	}
}

// TestSetRange_InvalidIgnored verifies invalid ranges don't modify state.
func TestSetRange_InvalidIgnored(t *testing.T) {
	u := NewUrgencyCalculator(time.Second, time.Minute, 5)
	origRatio := u.ratio
	origMin := u.minInterval

	// min > max — invalid.
	u.SetRange(time.Minute, time.Second)
	if u.ratio != origRatio || u.minInterval != origMin {
		t.Errorf("SetRange(invalid) modified state: ratio=%f min=%v", u.ratio, u.minInterval)
	}

	// min == 0 — invalid.
	u.SetRange(0, time.Second)
	if u.ratio != origRatio || u.minInterval != origMin {
		t.Errorf("SetRange(min=0) modified state")
	}
}
