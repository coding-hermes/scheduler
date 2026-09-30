package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// dailyCooldownResidueCheck is the loop's read-only residue pass
// (SCHED-GAP-1670): a daily event line naming any ENABLED lane whose live
// cooldown_s sits strictly BELOW its own stored operator pin, so the drift
// class the 2026-09-28 measurement found (11 lanes hotter than their pins)
// cannot accumulate silently again. It deliberately does NOT schedule
// fleet-cooldown-policy.py's mutating --apply path — detection here,
// reconciliation stays an operator action (a pin import or the API PUT,
// both of which snap the live value up).
//
// Wired into the loop's 30s health pass: the 24h cadence comes from the
// lastDailyResidueReport gate, so a restart re-reports only when the
// virtual/real day has actually turned (sim-clock friendly — every time
// source flows from the `nowTs` argument).
func (l *Loop) dailyCooldownResidueCheck(nowTs time.Time) {
	if l.db == nil {
		return
	}
	l.dailyMu.Lock()
	due := nowTs.Sub(l.lastDailyResidueReport) >= 24*time.Hour
	if due {
		l.lastDailyResidueReport = nowTs
	}
	l.dailyMu.Unlock()
	if !due {
		return
	}
	l.residueReportOnce(context.Background())
}

// residueReportOnce runs one read-only residue census and emits the event
// line. Split from dailyCooldownResidueCheck so tests can drive the census
// directly without staging a 24h clock advance; the emitter is shared with
// the daily gate. Best-effort by contract (mirrors the builder guard
// pass): a failed detection is logged and can never touch the caller's
// semantics.
func (l *Loop) residueReportOnce(ctx context.Context) {
	hasResidue, err := database.DetectCooldownResidue(ctx, l.db)
	if err != nil {
		log.Printf("RESIDUE: detection failed: %v", err)
		return
	}
	if !hasResidue {
		log.Printf("RESIDUE: cooldown-pin residue check clean (0 lanes live below their pin)")
		return
	}
	residue, err := database.ListCooldownResidue(ctx, l.db)
	if err != nil {
		log.Printf("RESIDUE: residue listing failed after detection: %v", err)
		return
	}
	names := make([]string, 0, len(residue))
	for _, r := range residue {
		names = append(names, fmt.Sprintf("%s(live=%ds<pin=%ds by %s)", r.Name, r.CooldownS, r.PinS, r.PinBy))
	}
	message := fmt.Sprintf("cooldown-pin residue: %d enabled lane(s) live BELOW their own pin — wake residue, not intent; reconcile via a pin import or the API PUT snap (cooldown_pin_s), never by editing cooldown_s directly", len(residue))
	details := map[string]any{
		"lanes":  names,
		"source": "daily residue check (SCHED-GAP-1670), read-only",
	}
	l.events.Emit(ctx, SeverityMedium, "cooldown-residue", message, details)
	log.Printf("RESIDUE: %s", message)
	for _, n := range names {
		log.Printf("RESIDUE: %s", n)
	}
}
