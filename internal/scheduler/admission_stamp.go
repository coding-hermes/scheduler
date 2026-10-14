package scheduler

// SCHED-GAP-1695 requirement 3 (STAMP): record HOW EARLY an admitted tick ran,
// and never let an early admission hide behind admit_reason='ok'.
//
// 'ok' means "admitted on schedule" (the packer saw the cooldown elapse). An
// admission that happened AHEAD of the cooldown — a board wake, an operator
// manual spawn, any residual path — must name its own reason. The families
// already carry precise names (resume:board_wake, resume:manual,
// flip:board_empty); AdmissionReasonEarly is the residual. With
// cooldown_remaining_s recorded alongside, the law's invariant becomes a
// single query: no row may have cooldown_remaining_s > 0 AND admit_reason='ok'.

import (
	"database/sql"
	"time"
)

// AdmissionReasonEarly names a tick admitted ahead of its cooldown that no more
// specific family (wake / manual / flip) claims.
const AdmissionReasonEarly = "early"

// admissionCooldownRemainingS reports the lane's cooldown left at THIS moment.
// 0 means the cooldown has already elapsed — a tick admitted now ran on
// schedule. It reads the same two things the packer's cooldown gate uses, the
// lane's cooldown_s (or its pin when one is armed) and its last completion, so
// the recorded number is the gate's own arithmetic rather than a parallel
// invention. Best-effort: an unreadable row, a missing/zero cooldown or an
// unparseable timestamp yields 0 (the honest "not measurable / not early" —
// never a fabricated countdown).
func admissionCooldownRemainingS(db *sql.DB, name string, now time.Time) float64 {
	if db == nil {
		return 0
	}
	var cd, pin sql.NullInt64
	var last sql.NullString
	if err := db.QueryRow(
		`SELECT cooldown_s, cooldown_pin_s, last_tick_completed FROM projects WHERE name = ?`,
		name).Scan(&cd, &pin, &last); err != nil {
		return 0
	}
	if !last.Valid || last.String == "" {
		return 0
	}
	dur := cd.Int64
	if pin.Valid && pin.Int64 > 0 {
		dur = pin.Int64
	}
	if dur <= 0 {
		return 0
	}
	t, err := time.Parse(time.RFC3339, last.String)
	if err != nil {
		return 0
	}
	rem := float64(dur) - now.Sub(t).Seconds()
	if rem <= 0 {
		return 0
	}
	return rem
}
