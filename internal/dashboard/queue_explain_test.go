package dashboard_test

// SCHED-GAP-1589 — the queue page must EXPLAIN itself: tooltips on every
// column, an interpretable urgency presentation (bands + scale, still the
// raw packer sort key), and a per-row "why waiting" cell fed by the
// scheduler's own records (deferrals log + running-tick admission stamps).
// These tests pin the operator-facing contract, not the exact prose.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// queueExplainCalc mirrors the daemon's calculator (30s-24h, 10 levels) so
// per-row interval tooltips carry real numbers.
func queueExplainCalc() *scheduler.UrgencyCalculator {
	return scheduler.NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
}

func renderExplainQueuePage(t *testing.T, calc *scheduler.UrgencyCalculator) string {
	t.Helper()
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 10)
	g := dashboard.NewGenerator(db, calc)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	return buf.String()
}

// TestQueuePage_EveryColumnHasTooltip is acceptance 1: no naked column
// headers — every th carries a title explaining what the value IS, its
// formula/derivation, and where it comes from.
func TestQueuePage_EveryColumnHasTooltip(t *testing.T) {
	out := renderExplainQueuePage(t, queueExplainCalc())

	if got := strings.Count(out, "<th title="); got < 7 {
		t.Fatalf("queue page has %d titled <th> headers, want >= 7 (6 data columns + why-waiting)", got)
	}
	for _, want := range []string{
		// # column: the packer's consideration order.
		`title="Position in this pass`,
		// Project column: nesting annotation semantics.
		`title="Lane name`,
		// Weight column: admission currency against the weight budget.
		`title="What this lane costs the weight budget`,
		// Priority column: multiplies urgency AND sets the tick interval.
		`title="Configured priority`,
		// Cooldown column: pacing floor from last COMPLETION, not the order.
		`title="Cooldown is a pacing floor`,
		// Urgency column: the formula line + sort key + unbounded.
		`urgency = priority * (1 + elapsed / interval) ^ decayRate`,
		`title="The packer&#39;s sort key`,
		// Why-waiting column: deferrals + admission stamps.
		`title="The recorded reason the lane is not running right now`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queue page missing tooltip text %q", want)
		}
	}
}

// TestQueuePage_HeaderExplainsUrgencyScale is acceptance 2's page-level half:
// the header states what urgency is FOR (packer sort key, not a health
// score), that it is unbounded, and gives the live page scale (median, p90).
func TestQueuePage_HeaderExplainsUrgencyScale(t *testing.T) {
	out := renderExplainQueuePage(t, queueExplainCalc())

	for _, want := range []string{
		"packer",       // names the consumer of the sort key
		"not a health", // guards against the misread
		"unbounded",    // no clamp anywhere in ComputeUrgency
		"median",       // live scale
		"p90",          // live scale
		"urgency.go",   // points at the formula's source file
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queue header explanation missing %q", want)
		}
	}
	// The scale must be internally consistent: on the page's own
	// distribution p90 >= median ALWAYS. A descending-slice percentile read
	// from the wrong end renders p10 as p90 (the SCHED-GAP-1589 probe bug).
	med := extractScaleNumber(t, out, "median ")
	p90 := extractScaleNumber(t, out, "p90 ")
	if p90 < med {
		t.Errorf("header scale is inverted: p90 (%v) < median (%v) — percentile read from the wrong end of the descending slice", p90, med)
	}
}

// extractScaleNumber parses the float following marker in the header meta
// sentence ("median X, p90 Y on this page").
func extractScaleNumber(t *testing.T, page, marker string) float64 {
	t.Helper()
	idx := strings.Index(page, marker)
	if idx < 0 {
		t.Fatalf("header scale marker %q not found", marker)
	}
	num := page[idx+len(marker):]
	end := 0
	for end < len(num) && (num[end] == '.' || (num[end] >= '0' && num[end] <= '9')) {
		end++
	}
	var v float64
	if _, err := fmt.Sscanf(num[:end], "%f", &v); err != nil {
		t.Fatalf("parse scale number after %q: %v (raw %q)", marker, err, num[:end])
	}
	return v
}

// TestQueuePage_UrgencyRowIsInterpretable is acceptance 2's row-level half:
// the urgency cell names the lane's own interval, the waited time, and a
// band relative to the page's own distribution — no bare 5-digit number.
func TestQueuePage_UrgencyRowIsInterpretable(t *testing.T) {
	db := newTestDB(t)
	calc := queueExplainCalc()
	// Five lanes so bands are meaningful (bands need >= 4 rows); staggered
	// completions give strictly ordered urgencies.
	staleness := []time.Duration{6 * time.Hour, 3 * time.Hour, 1 * time.Hour, 30 * time.Minute, 2 * time.Minute}
	for i, name := range []string{"lane-a", "lane-b", "lane-c", "lane-d", "lane-e"} {
		mustCreateProject(t, db, name, 10, 10)
		if _, err := db.Exec(
			`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
			time.Now().Add(-staleness[i]).UTC().Format(time.RFC3339), name,
		); err != nil {
			t.Fatalf("stamp %s: %v", name, err)
		}
	}
	g := dashboard.NewGenerator(db, calc)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	// Priority-10 interval under 30s-24h/10 levels is 30s; the tooltip must
	// say so in the lane's own terms.
	if !strings.Contains(out, "interval 30s") {
		t.Errorf("urgency tooltip must state the lane's own interval (30s for priority 10)")
	}
	// The most-starved lane carries the top-decile band.
	if !strings.Contains(out, "top decile") {
		t.Errorf("expected a 'top decile' band badge on the highest-urgency row")
	}
	if !strings.Contains(out, "above median") {
		t.Errorf("expected 'above median' band badges in the staged fixture")
	}
	// Waited time is humanized next to the score.
	if !strings.Contains(out, "waited ") {
		t.Errorf("urgency tooltip must state how long the lane has waited")
	}
}

// TestQueuePage_WhyWaitingColumn is acceptance 3 + 4: an eligible-but-blocked
// lane shows the RECORDED reason it is not running (deferrals vocabulary),
// a running lane shows its admission stamp, and a cooldown-active lane shows
// the remaining window — making visible that high-urgency lanes here are
// past cooldown (slot-held), not cooldown-blocked.
func TestQueuePage_WhyWaitingColumn(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now()

	// lane-blocked: past its 900s cooldown (completed 2h ago) and last
	// passed over by the packer: namespace cap full.
	mustCreateProject(t, db, "lane-blocked", 10, 10)
	if _, err := db.Exec(
		`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		now.Add(-2*time.Hour).UTC().Format(time.RFC3339), "lane-blocked",
	); err != nil {
		t.Fatalf("stamp lane-blocked: %v", err)
	}
	if err := database.RecordDeferral(ctx, db, "lane-blocked", "cap", 0, "ns slot pool full (8/8)"); err != nil {
		t.Fatalf("RecordDeferral: %v", err)
	}

	// lane-cooling: completed 10s ago, still inside its 900s cooldown.
	mustCreateProject(t, db, "lane-cooling", 10, 10)
	if _, err := db.Exec(
		`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		now.Add(-10*time.Second).UTC().Format(time.RFC3339), "lane-cooling",
	); err != nil {
		t.Fatalf("stamp lane-cooling: %v", err)
	}

	// lane-running: a live tick with its SCHED-GAP-157 admission stamp.
	mustCreateProject(t, db, "lane-running", 10, 10)
	if err := database.CreateTick(ctx, db, &database.Tick{
		ID:          "lane-running-tick",
		ProjectName: "lane-running",
		Status:      database.StatusRunning,
		SpawnedAt:   now.Add(-4 * time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := database.RecordTickAdmission(ctx, db, "lane-running-tick", 1234*time.Millisecond, "ok", "", 500, 10); err != nil {
		t.Fatalf("RecordTickAdmission: %v", err)
	}

	g := dashboard.NewGenerator(db, queueExplainCalc())
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	// Eligible-but-blocked: the recorded pass-over reason, glossed. The
	// template escapes the badge's ">" marker, hence &gt;.
	if !strings.Contains(out, `&gt;cap</span>`) {
		t.Errorf("lane-blocked row must show its recorded pass-over reason 'cap'")
	}
	if !strings.Contains(out, "namespace concurrency cap") {
		t.Errorf("'cap' must be glossed in operator terms (namespace concurrency cap)")
	}
	if !strings.Contains(out, "ns slot pool full (8/8)") {
		t.Errorf("the deferral detail must reach the row's tooltip")
	}

	// Cooldown-active: remaining window shown as countdown text.
	if !strings.Contains(out, "cooldown 14m") {
		t.Errorf("lane-cooling must show its remaining cooldown window (~14m)")
	}

	// Running: admission stamp, humanized slot wait.
	if !strings.Contains(out, "1.2s") {
		t.Errorf("lane-running must show its slot_wait_ms humanized (1234ms -> 1.2s)")
	}
	if !strings.Contains(out, `admitted "ok"`) {
		t.Errorf("lane-running tooltip must carry its admit_reason")
	}
}

// TestQueuePage_QueryCountStaysConstant guards the explanation columns: the
// why-waiting evidence must ride in the SAME single query the queue already
// runs (SCHED-GAP-174 one-query budget) — the count must never grow with
// project count or gain an explanation round-trip (the DASH-PERF regression
// shape).
func TestQueuePage_QueryCountStaysConstant(t *testing.T) {
	db, queryCount := newQueryCountingTestDB(t)
	for i := range 39 {
		mustCreateProject(t, db, fmt.Sprintf("project-%02d", i), 1, 1)
	}
	queryCount.Store(0)
	g := dashboard.NewGenerator(db, queueExplainCalc())
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	if got := queryCount.Load(); got != 1 {
		t.Errorf("GenerateQueue executed %d queries for 39 projects, want exactly 1 (evidence rides in the rank query)", got)
	}
}
