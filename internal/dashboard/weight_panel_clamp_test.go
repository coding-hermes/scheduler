package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestPctClampsTo100 pins the SCHED-GAP-1583 fix at the unit level: the template
// percent helper can never return a value above 100, so no fill can render wider
// than its track.
//
// The regression this guards is concrete. The fleet weight panel passed the
// fleet-wide sum of enabled lane weights (1218 live on 2026-09-24) as `used` and
// the PER-TICK packing budget (100) as `total`. The packer spends that budget
// once per cycle, so the sum can never sit under it; the unclamped helper
// returned 1218 and the page rendered width:1218%, which is the fill running
// past the card edge that the operator reported.
func TestPctClampsTo100(t *testing.T) {
	arms := []struct {
		name      string
		used, tot int
		want      int
	}{
		{"numerator over denominator clamps (the live 1218/100 case)", 1218, 100, 100},
		{"numerator exactly equal is 100", 100, 100, 100},
		{"numerator under denominator is exact", 50, 100, 50},
		{"zero numerator is 0", 0, 100, 0},
		{"zero denominator does not divide", 5, 0, 0},
		{"negative numerator floors at 0", -5, 100, 0},
		{"extreme ratio still clamps", 1 << 40, 1, 100},
	}
	for _, a := range arms {
		if got := pct(a.used, a.tot); got != a.want {
			t.Errorf("%s: pct(%d, %d) = %d, want %d", a.name, a.used, a.tot, got, a.want)
		}
	}
}

// TestFleetPageRendersNoOverflowingWidth proves the same fix end to end: whatever
// the data, the generated fleet page emits no width greater than 100%.
//
// This is the assertion that would have caught the original bug, stated as a
// property of the output rather than of one template site — so a future caller
// that reintroduces an unclamped width fails here even if it never uses pct.
func TestFleetPageRendersNoOverflowingWidth(t *testing.T) {
	db := newTestDB(t)
	g := NewGenerator(db, nil)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	page := buf.String()

	re := regexp.MustCompile(`width:(\d+)%`)
	matches := re.FindAllStringSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("no rendered widths found — the assertion would be vacuous")
	}
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > 100 {
			t.Fatalf("rendered %s exceeds its track (must be <= 100%%)", m[0])
		}
	}
}

// TestFleetPageWeightPanelIsNotARatio locks the presentation fix: the panel shows
// the two quantities as labelled facts. It must NOT re-introduce a "used/total"
// fraction, because a fleet-wide weight sum and a per-tick budget are different
// quantities and the fraction implied they were comparable.
func TestFleetPageWeightPanelIsNotARatio(t *testing.T) {
	db := newTestDB(t)
	g := NewGenerator(db, nil)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	page := buf.String()

	for _, want := range []string{
		"Fleet weight — sum of enabled lanes",
		"Per-tick weight budget",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("weight panel is missing the label %q", want)
		}
	}
	if strings.Contains(page, "Weight Budget</span>") {
		t.Error("the old 'Weight Budget' ratio label is still rendered")
	}
}
