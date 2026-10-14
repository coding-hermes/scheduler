package main

import "testing"

// INT-CI-169 regression: the first-cycle tolerance window was computed by
// parsing the second-resolution string substr(spawned_at, 1, 19) with
// time.RFC3339, which requires a timezone designator the substring has
// already cut off. The parse ALWAYS failed and the degrade branch shrank
// the window to an exact-second match, so any first-cycle spawn landing
// in second+1 on a loaded CI runner fell outside and the check undercounted
// ("2/5 expected projects spawned" while all 5 were stamped the same
// second). The window end must be min+2s in the truncated layout.
func TestFirstCycleWindowEnd(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		want      string
		wantWider bool // true when the window must be WIDER than exact match
	}{
		{
			name:      "second-resolution stamp gets real 2s window",
			in:        "2026-09-30T09:44:31",
			want:      "2026-09-30T09:44:33",
			wantWider: true,
		},
		{
			name:      "empty input degrades to empty",
			in:        "",
			want:      "",
			wantWider: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := firstCycleWindowEnd(tc.in)
			if got != tc.want {
				t.Fatalf("firstCycleWindowEnd(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.wantWider && got == tc.in {
				t.Fatalf("INT-CI-169 regression: window end %q equals window start %q — exact-second degrade, tolerance window lost", got, tc.in)
			}
		})
	}
}

// A spawn stamped in second+1 of the first cycle must fall INSIDE the
// window: the exact-second degrade excluded it and caused the CI
// undercount.
func TestFirstCycleWindowCoversNextSecondSpawn(t *testing.T) {
	start := "2026-09-30T09:44:31"
	end := firstCycleWindowEnd(start)
	if start > "2026-09-30T09:44:32" || "2026-09-30T09:44:32" > end {
		t.Fatalf("a same-cycle spawn at 2026-09-30T09:44:32 is outside window [%s, %s]", start, end)
	}
}
