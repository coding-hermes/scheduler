package scheduler

// SCHED-GAP-1632: the sim-fixture report's elapsed label must be honest per
// clock. Dogfood evidence: a sim-clock run printed "Ticks: 10 (39.8s real
// time)" while `time -p` measured 0.20s — report.Elapsed on a sim clock is
// VIRTUAL time (sim.Since across the run), so labeling it "real" misinformed
// the operator by ~200x. RunMultiTick now captures the real wall span and the
// clock's own Describe() (the boot-line source), and Summary renders
// "Xs virtual / Ys real" under a sim clock while every real-clock path keeps
// the historical "(Xs real time)" wording.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// TestSimReportSummary_LabelsVirtualVsReal is the pure-label table: Summary
// picks the wording from ClockDescribe alone, so the sim arm renders both
// spans and the real/unknown arm keeps "(Xs real time)".
func TestSimReportSummary_LabelsVirtualVsReal(t *testing.T) {
	cases := []struct {
		name        string
		elapsed     time.Duration
		elapsedReal time.Duration
		describe    string
		wantSubstr  string
		wantAbsent  string
	}{
		{
			name:        "sim clock renders virtual/real pair",
			elapsed:     39800 * time.Millisecond,
			elapsedReal: 200 * time.Millisecond,
			describe:    "sim (scale=1000 start=2026-10-06T00:00:00Z auto=false driven=true)",
			wantSubstr:  "Ticks:       10 (39.8s virtual / 0.2s real)",
			wantAbsent:  "39.8s real time",
		},
		{
			name:        "real clock keeps historical wording",
			elapsed:     18500 * time.Millisecond,
			elapsedReal: 18500 * time.Millisecond,
			describe:    clock.ModeReal,
			wantSubstr:  "Ticks:       10 (18.5s real time)",
			wantAbsent:  "virtual",
		},
		{
			name:        "empty describe (pre-1632 hand-built report) keeps historical wording",
			elapsed:     5 * time.Second,
			elapsedReal: 5 * time.Second,
			describe:    "",
			wantSubstr:  "Ticks:       1 (5.0s real time)",
			wantAbsent:  "virtual",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &SimReport{
				TickCount:     10,
				Elapsed:       tc.elapsed,
				ElapsedReal:   tc.elapsedReal,
				ClockDescribe: tc.describe,
			}
			if tc.name == "empty describe (pre-1632 hand-built report) keeps historical wording" {
				r.TickCount = 1
			}
			s := r.Summary()
			if !strings.Contains(s, tc.wantSubstr) {
				t.Errorf("Summary missing %q:\n%s", tc.wantSubstr, s)
			}
			if tc.wantAbsent != "" && strings.Contains(s, tc.wantAbsent) {
				t.Errorf("Summary must not carry %q on this clock:\n%s", tc.wantAbsent, s)
			}
		})
	}
}

// TestRunMultiTick_SummaryVirtualRealLabel_SimClock: a REAL RunMultiTick on a
// sim clock must render the virtual/real pair — the virtual span from the sim
// clock (Elapsed), the real span from the wall (ElapsedReal) — and the
// report's ClockDescribe must be the clock's own Describe() string (the same
// source the boot line prints).
func TestRunMultiTick_SummaryVirtualRealLabel_SimClock(t *testing.T) {
	db := newTestDB(t)
	sim := clock.NewSimClockAt(1000, time.Now())
	t.Cleanup(sim.Close)

	runner, _ := newSimRunner1630(t, db, sim)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := runner.RunMultiTick(ctx, 2)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}

	if !strings.HasPrefix(report.ClockDescribe, clock.ModeSim) {
		t.Errorf("report.ClockDescribe = %q, want the sim clock's Describe() (prefix %q)", report.ClockDescribe, clock.ModeSim)
	}
	if report.ElapsedReal <= 0 {
		t.Errorf("report.ElapsedReal = %v, want the wall-clock span (> 0)", report.ElapsedReal)
	}
	s := report.Summary()
	want := fmt.Sprintf("%.1fs virtual / %.1fs real", report.Elapsed.Seconds(), report.ElapsedReal.Seconds())
	if !strings.Contains(s, want) {
		t.Errorf("Summary missing the virtual/real label %q:\n%s", want, s)
	}
	if strings.Contains(s, fmt.Sprintf("%.1fs real time", report.Elapsed.Seconds())) {
		t.Errorf("Summary still labels the VIRTUAL span as 'real time':\n%s", s)
	}
}

// TestRunMultiTick_SummaryRealClockWording: a run on the wall clock keeps the
// historical "(Xs real time)" wording — the label change must be invisible to
// real-clock operators.
func TestRunMultiTick_SummaryRealClockWording(t *testing.T) {
	db := newTestDB(t)
	runner, _ := newSimRunner1630(t, db, nil) // nil → wall clock
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := runner.RunMultiTick(ctx, 1)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}

	if report.ClockDescribe != clock.ModeReal {
		t.Errorf("report.ClockDescribe = %q, want %q on the wall clock", report.ClockDescribe, clock.ModeReal)
	}
	s := report.Summary()
	want := fmt.Sprintf("%.1fs real time)", report.Elapsed.Seconds())
	if !strings.Contains(s, want) {
		t.Errorf("Summary lost the historical real-time wording on the wall clock (want %q):\n%s", want, s)
	}
	if strings.Contains(s, " virtual / ") {
		t.Errorf("Summary must not render the virtual/real pair on the wall clock:\n%s", s)
	}
}
