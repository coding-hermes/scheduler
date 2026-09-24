package scheduler

// SCHED-GAP-1582 — weight-budget enforcement tests.
//
// Acceptance (from the row):
//   1. A namespace at/over its budget HOLDS work and the hold is recorded
//      with the reason — asserted through the REAL surfaces (deferrals
//      rows, events rows), not just return values.
//   2. A namespace under budget is unaffected — the selection set is
//      unchanged (the fair-share arithmetic is the allocator's pinned
//      contract; see scheduler_test's TestMultiPoolPacker_*).
//   3. A budget of 0 / unset behaves as the documented default (100), not
//      as "hold everything".
//
// FIXTURE SHAPE (why 7 members): without the floor-at-1, Σ floor(alloc·wᵢ/D)
// ≤ alloc always, so an oversubscribed namespace would still place everything
// and no hold could ever exist. Real holds come from the floor-at-1: members
// whose proportional share is < 1 each consume a full unit. ns-fat (alloc 20,
// demand 36) has one big member (effW 16) and six unit members (effW 1 each):
// 16+1+1+1+1 = 20 = alloc, so fat-t5/fat-t6 queue — the hold. This is exactly
// the live-fleet shape (many namespaces demand ~248 against a budget of ~100,
// so member shares floor to 1 and queues form every cycle).

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// holdCalc is the urgency calculator every fixture packs with.
func holdCalc() *UrgencyCalculator {
	return NewUrgencyCalculator(time.Minute, time.Hour, 10)
}

// holdNow is the pinned decision instant (same shape as fixedEvalNow).
func holdNow() time.Time {
	return time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC)
}

// budgetHoldsRecorder captures deferral writes without a full Loop.
type budgetHoldsRecorder struct {
	projects []string
	reasons  []string
	details  []string
}

func (r *budgetHoldsRecorder) recordDeferral(project, reason string, passID int64, detail string) {
	r.projects = append(r.projects, project)
	r.reasons = append(r.reasons, reason)
	r.details = append(r.details, detail)
}

// holdSeedNamespace / holdSeedProject are local seed helpers (create-or-fail).
func holdSeedNamespace(t *testing.T, db *sql.DB, id string, weight int) {
	t.Helper()
	ns := &database.Namespace{ID: id, Weight: weight, Reserved: 0, HardCap: 100, Enabled: true}
	if err := database.CreateNamespace(context.Background(), db, ns); err != nil {
		t.Fatalf("CreateNamespace %s: %v", id, err)
	}
}

func holdSeedProject(t *testing.T, db *sql.DB, name, nsID string, weight int) {
	t.Helper()
	p := &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    weight,
		Priority:  5,
		CooldownS: 3600, // 1h pin: deterministic eligibility (no dynamic interval)
		DecayRate: 1.0,
		Model:     "test-model",
		Provider:  "test-provider",
		Enabled:   true,
		// Ticked 90m ago: cooldown-eligible (90m > 1h pin), NOT overdue
		// (90m < 2h = 2x cooldown, so GAP-011 force-select never fires),
		// not starving. These tests exercise the BUDGET gate only.
		LastTickCompleted: holdNow().Add(-90 * time.Minute).UTC().Format(time.RFC3339),
	}
	id := nsID
	p.NamespaceID = &id
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// oversubFixture builds the standard hold fixture: budget 100 —
//   - ns-fat: weight 20 → alloc 20; members fat-big(w30) + fat-t1..t6(w1)
//     → demand 36, effective weights 16,1,1,1,1,1,1 → 20 placed, t5+t6 HELD.
//   - ns-fit: weight 80 → alloc 80; members fit-a(w4), fit-b(w6)
//     → demand 10, both placed.
func oversubFixture(t *testing.T) PackResult {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()

	holdSeedNamespace(t, db, "ns-fat", 20)
	holdSeedNamespace(t, db, "ns-fit", 80)
	holdSeedProject(t, db, "fat-big", "ns-fat", 30)
	for _, n := range []string{"fat-t1", "fat-t2", "fat-t3", "fat-t4", "fat-t5", "fat-t6"} {
		holdSeedProject(t, db, n, "ns-fat", 1)
	}
	holdSeedProject(t, db, "fit-a", "ns-fit", 4)
	holdSeedProject(t, db, "fit-b", "ns-fit", 6)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := NewMultiPoolPacker(100, 10, nil)
	return mp.Pack(projects, namespaces, holdCalc(), nil, nil, holdNow())
}

// TestBudgetHold_OversubscribedNamespaceHoldsWork — acceptance 1, packer
// surface: the over-budget namespace reports demand > allocation with the
// held lanes and the surplus; the under-budget namespace reports nothing.
func TestBudgetHold_OversubscribedNamespaceHoldsWork(t *testing.T) {
	result := oversubFixture(t)

	if len(result.BudgetHolds) != 1 {
		t.Fatalf("BudgetHolds = %d entries, want exactly 1 (ns-fat): %+v", len(result.BudgetHolds), result.BudgetHolds)
	}
	h := result.BudgetHolds[0]
	if h.nsID != "ns-fat" {
		t.Errorf("hold namespace = %q, want ns-fat", h.nsID)
	}
	if h.demand != 36 {
		t.Errorf("demand = %d, want 36 (the raw enabled weights: 30 + 6x1)", h.demand)
	}
	if h.alloc != 20 {
		t.Errorf("alloc = %d, want 20 (fat's proportional share of the 100 budget)", h.alloc)
	}
	if h.over() != 16 {
		t.Errorf("over = %d, want 16", h.over())
	}
	got := map[string]bool{}
	for _, pu := range h.held {
		got[pu.Project.Name] = true
	}
	if !got["fat-t5"] || !got["fat-t6"] || len(got) != 2 {
		t.Errorf("held = %v, want exactly [fat-t5 fat-t6]", got)
	}
	// The under-budget namespace must be absent from the holds.
	if h.nsID == "ns-fit" {
		t.Errorf("ns-fit (demand 10 <= alloc 80) must not be held: %+v", h)
	}

	// The namespace tick carries the demand/overcommitted columns.
	var fatTick, fitTick *NamespaceTickData
	for i := range result.NamespaceTicks {
		switch result.NamespaceTicks[i].NamespaceID {
		case "ns-fat":
			fatTick = &result.NamespaceTicks[i]
		case "ns-fit":
			fitTick = &result.NamespaceTicks[i]
		}
	}
	if fatTick == nil {
		t.Fatal("no NamespaceTickData for ns-fat")
	}
	if fatTick.Demand != 36 || fatTick.Overcommitted != 16 {
		t.Errorf("ns-fat tick demand/overcommitted = %d/%d, want 36/16", fatTick.Demand, fatTick.Overcommitted)
	}
	if fitTick == nil || fitTick.Demand != 10 || fitTick.Overcommitted != 0 {
		t.Errorf("ns-fit tick must read demand=10 overcommitted=0, got %+v", fitTick)
	}
}

// TestBudgetHold_UnderBudgetNamespaceUnaffected — acceptance 2: the placed
// SET is exactly what the pre-change packer produced for this input (all of
// ns-fit, plus ns-fat's five placeable members), and ns-fit is untouched by
// the hold machinery. Placed weights are effective weights (the existing
// fair-share contract — shares scale to fill the allocation), so the set is
// asserted, not the raw weights.
func TestBudgetHold_UnderBudgetNamespaceUnaffected(t *testing.T) {
	result := oversubFixture(t)

	selected := map[string]bool{}
	for _, p := range result.Projects {
		selected[p.Name] = true
	}
	for _, want := range []string{"fit-a", "fit-b", "fat-big", "fat-t1", "fat-t2", "fat-t3", "fat-t4"} {
		if !selected[want] {
			t.Errorf("%s missing from selection — selection changed: %v", want, selected)
		}
	}
	for _, unplanned := range []string{"fat-t5", "fat-t6"} {
		if selected[unplanned] {
			t.Errorf("%s placed — hold arithmetic broken: %v", unplanned, selected)
		}
	}
	if len(result.Projects) != 7 {
		t.Errorf("selected %d projects, want 7", len(result.Projects))
	}
}

// TestBudgetHold_DeferralsRecorded — acceptance 1, DB surface: each held
// lane gets a deferrals row through the SCHED-GAP-157 path with reason
// "budget" and a detail naming the namespace arithmetic. Asserted against
// the deferrals TABLE via the real Loop helper, not just the recorder.
func TestBudgetHold_DeferralsRecorded(t *testing.T) {
	db := newTestDB(t)
	rec := &budgetHoldsRecorder{}

	// No held lanes → nothing recorded.
	empty := newNsHold("ns-fat", 36, 20, nil)
	recordBudgetHoldDeferrals(rec, empty, 42)
	if len(rec.projects) != 0 {
		t.Fatalf("nil held list must record nothing, got %v", rec.projects)
	}

	h := newNsHold("ns-fat", 36, 20, []*ProjectUrgency{
		{Project: database.Project{Name: "fat-t5"}},
		{Project: database.Project{Name: "fat-t6"}},
	})
	recordBudgetHoldDeferrals(rec, h, 42)

	if len(rec.projects) != 2 || rec.projects[0] != "fat-t5" || rec.projects[1] != "fat-t6" {
		t.Fatalf("recorded projects = %v, want [fat-t5 fat-t6]", rec.projects)
	}
	for i, reason := range rec.reasons {
		if reason != AdmissionReasonBudget {
			t.Errorf("reason[%d] = %q, want %q (the frozen SCHED-GAP-155 vocabulary entry)", i, reason, AdmissionReasonBudget)
		}
	}
	if !strings.Contains(rec.details[0], "ns=ns-fat") || !strings.Contains(rec.details[0], "demand=36") || !strings.Contains(rec.details[0], "alloc=20") {
		t.Errorf("detail missing the hold arithmetic: %q", rec.details[0])
	}

	// The same write through the REAL Loop helper lands in the table.
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.recordDeferral("fat-t7", AdmissionReasonBudget, 7, "budget_hold ns=ns-fat demand=36 alloc=20 over=16")
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deferrals WHERE project_name='fat-t7' AND reason='budget'`).Scan(&count); err != nil {
		t.Fatalf("query deferrals: %v", err)
	}
	if count != 1 {
		t.Errorf("deferrals rows for fat-t7 = %d, want 1", count)
	}
}

// TestBudgetHold_EventEmitted — acceptance 1, events surface: the hold
// emits one HIGH event through the shared EventLogger, landing in the
// events TABLE (the same channel /api/v1/events/stream reads).
func TestBudgetHold_EventEmitted(t *testing.T) {
	db := newTestDB(t)
	el := NewEventLogger(db)

	h := newNsHold("ns-fat", 36, 20, []*ProjectUrgency{
		{Project: database.Project{Name: "fat-t5"}},
		{Project: database.Project{Name: "fat-t6"}},
	})
	emitBudgetHoldEvent(el, h, 9, holdNow())

	var sev, component, message, details string
	err := db.QueryRow(`SELECT severity, component, message, details FROM events ORDER BY id DESC LIMIT 1`).
		Scan(&sev, &component, &message, &details)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if sev != string(SeverityHigh) {
		t.Errorf("severity = %q, want %q", sev, SeverityHigh)
	}
	if message != "namespace over weight budget: work held this cycle" {
		t.Errorf("message = %q", message)
	}
	for _, want := range []string{`"namespace":"ns-fat"`, `"demand":36`, `"allocation":20`, `"over":16`, `"pass_id":9`} {
		if !strings.Contains(details, want) {
			t.Errorf("event details missing %s: %s", want, details)
		}
	}

	// nil logger must not panic (tooling path).
	emitBudgetHoldEvent(nil, h, 9, holdNow())
}

// TestBudgetHold_ZeroBudgetBehavesAsDefault — acceptance 3: budget 0 and
// unset normalize to the documented default 100, not "hold everything".
func TestBudgetHold_ZeroBudgetBehavesAsDefault(t *testing.T) {
	t.Run("NewPacker zero budget normalizes to 100", func(t *testing.T) {
		db := newTestDB(t)
		p := NewPacker(db, holdCalc(), 0, 10, nil)
		if p.Budget() != 100 {
			t.Errorf("NewPacker(0).Budget() = %d, want 100", p.Budget())
		}
	})
	t.Run("NewMultiPoolPacker zero budget places work", func(t *testing.T) {
		mp := NewMultiPoolPacker(0, 10, nil)
		if mp.allocator.budget != 100 {
			t.Errorf("NewMultiPoolPacker(0) allocator budget = %d, want 100", mp.allocator.budget)
		}
		db := newTestDB(t)
		ctx := context.Background()
		holdSeedNamespace(t, db, "ns-x", 10)
		holdSeedProject(t, db, "x-a", "ns-x", 10)
		projects, err := database.ListProjects(ctx, db, true)
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		namespaces, err := database.ListNamespaces(ctx, db, true)
		if err != nil {
			t.Fatalf("ListNamespaces: %v", err)
		}
		res := mp.Pack(projects, namespaces, holdCalc(), nil, nil, holdNow())
		if len(res.Projects) != 1 {
			t.Errorf("zero-budget packer held everything — selected %d projects, want 1", len(res.Projects))
		}
		if len(res.BudgetHolds) != 0 {
			t.Errorf("zero-budget packer produced holds: %+v", res.BudgetHolds)
		}
	})
	t.Run("NewLoop zero budget normalizes to 100", func(t *testing.T) {
		db := newTestDB(t)
		l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 0, 4)
		if l.WeightBudget() != 100 {
			t.Errorf("NewLoop(budget=0).WeightBudget() = %d, want 100", l.WeightBudget())
		}
	})
}

// TestBudgetHold_EvaluateSurfacesTheHold — end to end through the REAL
// evaluate() in namespace mode: the oversubscribed namespace produces
// deferrals rows for its held lanes (reason=budget, detail naming the
// arithmetic) and a HIGH events row, and the demand/overcommitted columns
// are persisted for the dashboard. The packer-level surfaces are covered
// by the tests above.
func TestBudgetHold_EvaluateSurfacesTheHold(t *testing.T) {
	db := newTestDB(t)

	holdSeedNamespace(t, db, "ns-fat", 20)
	// ns-fit must be present: a LONE namespace receives the whole budget
	// (weight 20 of 20 → alloc 100), fits its demand, and never holds.
	holdSeedNamespace(t, db, "ns-fit", 80)
	holdSeedProject(t, db, "fat-big", "ns-fat", 30)
	for _, n := range []string{"fat-t1", "fat-t2", "fat-t3", "fat-t4", "fat-t5", "fat-t6"} {
		holdSeedProject(t, db, n, "ns-fat", 1)
	}
	holdSeedProject(t, db, "fit-a", "ns-fit", 4)
	holdSeedProject(t, db, "fit-b", "ns-fit", 6)

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 20)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(holdNow()))
	l.SetNamespaceMode(true)

	l.evaluate()

	// 1. Deferrals rows written BY THE HOLD (detail prefix "budget_hold")
	// carry reason=budget with the hold arithmetic. The admission pass's
	// own post-hoc classifier may ALSO write rows for the same lanes
	// (same reason, generic detail) — two honest recorders, one decision;
	// the hold rows are distinguished by their detail prefix.
	rows, err := db.Query(`SELECT project_name, reason, detail FROM deferrals WHERE detail LIKE '%budget_hold ns=ns-fat%' ORDER BY project_name`)
	if err != nil {
		t.Fatalf("query deferrals: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, reason, detail string
		if err := rows.Scan(&name, &reason, &detail); err != nil {
			t.Fatalf("scan deferral: %v", err)
		}
		if reason != AdmissionReasonBudget {
			t.Errorf("deferral reason = %q, want %q", reason, AdmissionReasonBudget)
		}
		if !strings.Contains(detail, "demand=36") || !strings.Contains(detail, "over=16") {
			t.Errorf("deferral detail missing hold arithmetic: %q", detail)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deferrals: %v", err)
	}
	if len(names) != 2 || names[0] != "fat-t5" || names[1] != "fat-t6" {
		t.Errorf("held deferral rows = %v, want [fat-t5 fat-t6]", names)
	}

	// 2. HIGH event row for the hold.
	var sev, details string
	err = db.QueryRow(`SELECT severity, details FROM events WHERE message='namespace over weight budget: work held this cycle' ORDER BY id DESC LIMIT 1`).Scan(&sev, &details)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if sev != string(SeverityHigh) {
		t.Errorf("event severity = %q, want HIGH", sev)
	}
	if !strings.Contains(details, `"namespace":"ns-fat"`) {
		t.Errorf("event details missing namespace: %s", details)
	}

	// 3. The demand/overcommitted columns are persisted for the dashboard.
	var demand, over int
	err = db.QueryRow(`SELECT demand, overcommitted FROM namespace_ticks WHERE namespace_id='ns-fat' ORDER BY created_at DESC LIMIT 1`).Scan(&demand, &over)
	if err != nil {
		t.Fatalf("query namespace_ticks: %v", err)
	}
	if demand != 36 || over != 16 {
		t.Errorf("namespace_ticks demand/overcommitted = %d/%d, want 36/16", demand, over)
	}
}

// TestBudgetHold_Arithmetic — the oversubscription predicate: strictly over
// is over; at-budget and under-budget are not.
func TestBudgetHold_Arithmetic(t *testing.T) {
	if oversubscription(120, 80) != 40 {
		t.Errorf("oversubscription(120,80) = %d, want 40", oversubscription(120, 80))
	}
	if oversubscription(80, 80) != 0 {
		t.Errorf("oversubscription(80,80) = %d, want 0 — at-budget is not over", oversubscription(80, 80))
	}
	if oversubscription(10, 80) != 0 {
		t.Errorf("oversubscription(10,80) = %d, want 0 — under-budget is not over", oversubscription(10, 80))
	}
}
