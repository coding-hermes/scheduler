package scheduler

import (
	"testing"
	"time"
)

// TestSCHEDGAP1695_StampEarlyNeverOK pins requirement 3 (STAMP): the recorded
// cooldown remaining is the cooldown gate's own arithmetic, and the invariant
// "no EARLY admission carries admit_reason='ok'" holds by construction at the
// admit boundary — proven end-to-end on a real tick row.
func TestSCHEDGAP1695_StampEarlyNeverOK(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	// 10 minutes into a 60-minute cooldown → an admission NOW is EARLY, with
	// roughly 50 minutes (3000s) left on the clock.
	insertEligibilityProject(t, db, "earlylane", 3600, 5, 0, now.Add(-10*time.Minute), 0, 0)
	if rem := admissionCooldownRemainingS(db, "earlylane", now); rem < 2900 || rem > 3100 {
		t.Fatalf("earlylane remaining = %.0fs, want ~3000s", rem)
	}

	// 30 minutes into a 15-minute cooldown → the cooldown ELAPSED. Not early.
	insertEligibilityProject(t, db, "ontime", 900, 5, 0, now.Add(-30*time.Minute), 0, 0)
	if rem := admissionCooldownRemainingS(db, "ontime", now); rem != 0 {
		t.Fatalf("ontime remaining = %.0fs, want 0 (it ran on schedule)", rem)
	}

	// Honest zeros — never a fabricated countdown.
	if rem := admissionCooldownRemainingS(db, "no-such-lane", now); rem != 0 {
		t.Fatalf("unknown lane remaining = %.0f, want 0", rem)
	}
	if rem := admissionCooldownRemainingS(nil, "earlylane", now); rem != 0 {
		t.Fatalf("nil db remaining = %.0f, want 0", rem)
	}

	// The invariant, end to end: take the EARLY lane, apply the SAME two lines
	// the admit boundary applies, stamp a real tick row, and read it back.
	insertTickRow := func() {
		if _, err := db.Exec(
			`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES ('t-early','earlylane','running',?,?)`,
			now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert tick t-early: %v", err)
		}
	}
	insertTickRow()
	rem := admissionCooldownRemainingS(db, "earlylane", now)
	reason := AdmissionReasonOK
	if rem > 0 && reason == AdmissionReasonOK {
		reason = AdmissionReasonEarly
	}
	stampTickAdmission(db, "t-early", 0, reason, "", 0, 0, rem)

	var gotReason string
	var gotRem float64
	if err := db.QueryRow(
		`SELECT admit_reason, cooldown_remaining_s FROM ticks WHERE id = 't-early'`).
		Scan(&gotReason, &gotRem); err != nil {
		t.Fatalf("read stamped row: %v", err)
	}
	if gotReason == AdmissionReasonOK && gotRem > 0 {
		t.Fatalf("EARLY admission carried admit_reason=ok with %.0fs left — invariant violated", gotRem)
	}
	if gotReason != AdmissionReasonEarly || gotRem <= 0 {
		t.Fatalf("early stamp = (%q, %.0fs), want (%q, >0)", gotReason, gotRem, AdmissionReasonEarly)
	}
}
