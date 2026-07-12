package scheduler

import (
	"log"
	"math"
	"time"
)

// UrgencyCalculator computes tick intervals and urgency scores.
type UrgencyCalculator struct {
	minInterval time.Duration
	maxInterval time.Duration
	numLevels   int
	ratio       float64
}

// NewUrgencyCalculator creates a calculator with the given range.
func NewUrgencyCalculator(minI, maxI time.Duration, numLevels int) *UrgencyCalculator {
	if numLevels < 2 {
		log.Printf("WARNING: numLevels=%d < 2, setting to 2", numLevels)
		numLevels = 2
	}
	minSec := minI.Seconds()
	maxSec := maxI.Seconds()
	ratio := 1.0
	if minSec > 0 && maxSec > minSec {
		ratio = maxSec / minSec
	} else {
		log.Printf("WARNING: invalid interval range (min=%.0fs, max=%.0fs), using ratio=1.0", minSec, maxSec)
	}
	return &UrgencyCalculator{
		minInterval: minI,
		maxInterval: maxI,
		numLevels:   numLevels,
		ratio:       ratio,
	}
}

// ComputeInterval maps priority to a geometric tick interval.
// Formula: interval = minInterval * ratio ^ ((priority - 1) / (numLevels - 1))
func (u *UrgencyCalculator) ComputeInterval(priority float64) time.Duration {
	p := priority
	if p < 1 {
		p = 1
	} else if p > float64(u.numLevels) {
		p = float64(u.numLevels)
	}
	position := (p - 1) / float64(u.numLevels-1)
	multiplier := math.Pow(u.ratio, position)
	seconds := u.minInterval.Seconds() * multiplier
	return time.Duration(seconds * float64(time.Second))
}

// ComputeUrgency returns the urgency score for a project.
// Formula: urgency = priority * (1 + elapsed / interval) ^ decayRate
func (u *UrgencyCalculator) ComputeUrgency(priority, decayRate float64, now time.Time, lastCompleted *time.Time, createdAt time.Time) float64 {
	d := decayRate
	if d < 0 {
		d = 0
	}
	var elapsed float64
	if lastCompleted != nil {
		elapsed = now.Sub(*lastCompleted).Seconds()
	} else {
		elapsed = now.Sub(createdAt).Seconds()
	}
	if elapsed < 0 {
		elapsed = 0
	}
	interval := u.ComputeInterval(priority)
	intervalSeconds := interval.Seconds()
	base := 1.0 + (elapsed / intervalSeconds)
	if base < 1.0 {
		base = 1.0
	}
	return priority * math.Pow(base, d)
}

// SetRange updates the min/max interval range at runtime.
func (u *UrgencyCalculator) SetRange(minI, maxI time.Duration) {
	minSec := minI.Seconds()
	maxSec := maxI.Seconds()
	if minSec <= 0 || maxSec <= minSec {
		log.Printf("WARNING: SetRange ignored: invalid range (min=%.0fs, max=%.0fs)", minSec, maxSec)
		return
	}
	u.minInterval = minI
	u.maxInterval = maxI
	u.ratio = maxSec / minSec
}
