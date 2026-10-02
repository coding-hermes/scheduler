package scheduler

import (
	"fmt"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// approxEqual reports whether got is within delta of want, avoiding brittle
// exact comparisons for proportional floors.
func approxEqual(got, want, delta int) bool {
	if got < want-delta || got > want+delta {
		return false
	}
	return true
}

// assertAllocation asserts the allocation map matches want exactly and (when
// wantTotal >= 0) that its total equals wantTotal. Exact maps are the point of
// the SCHED-GAP-1697 largest-remainder tests: the allocator must place every
// slot, so an approximate sum would hide the defect the tests pin.
func assertAllocation(t *testing.T, got, want map[string]int, wantTotal int) {
	t.Helper()
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s allocation = %d, want %d", id, got[id], w)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected namespace %q in allocation", id)
		}
	}
	if wantTotal >= 0 {
		sum := 0
		for _, v := range got {
			sum += v
		}
		if sum != wantTotal {
			t.Errorf("total allocated = %d, want %d (no slot may be left on the floor)", sum, wantTotal)
		}
	}
}

// TestNamespaceAllocator_ReservedFloors verifies every namespace receives at
// least its reserved allocation, even when one namespace dominates the weights.
func TestNamespaceAllocator_ReservedFloors(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "ns-a", Weight: 100, Reserved: 10, HardCap: 0, Enabled: true},
		{ID: "ns-b", Weight: 1, Reserved: 5, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	if got["ns-a"] < 10 {
		t.Errorf("ns-a allocation = %d, want >= 10 (reserved floor)", got["ns-a"])
	}
	if got["ns-b"] < 5 {
		t.Errorf("ns-b allocation = %d, want >= 5 (reserved floor)", got["ns-b"])
	}
	if got["ns-a"]+got["ns-b"] > 100 {
		t.Errorf("sum = %d, want <= 100", got["ns-a"]+got["ns-b"])
	}
}

// TestNamespaceAllocator_HardCapEnforced verifies HardCap is never exceeded,
// even when the namespace is the only enabled one and has a huge weight.
func TestNamespaceAllocator_HardCapEnforced(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "only", Weight: 1000, Reserved: 0, HardCap: 30, Enabled: true},
	}

	got := a.Allocate(namespaces)

	if got["only"] > 30 {
		t.Errorf("allocation = %d, want <= 30 (hard cap)", got["only"])
	}
}

// TestNamespaceAllocator_SumEqualsBudget verifies the sum of allocations is
// close to the total budget for a typical multi-namespace split.
func TestNamespaceAllocator_SumEqualsBudget(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "ns-50", Weight: 50, Reserved: 10, HardCap: 0, Enabled: true},
		{ID: "ns-30", Weight: 30, Reserved: 5, HardCap: 0, Enabled: true},
		{ID: "ns-20", Weight: 20, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)
	sum := got["ns-50"] + got["ns-30"] + got["ns-20"]

	if !approxEqual(sum, 100, 2) {
		t.Errorf("sum = %d, want ~100", sum)
	}
	if got["ns-50"] < 10 || got["ns-30"] < 5 || got["ns-20"] < 0 {
		t.Errorf("reserved floors violated: %v", got)
	}
}

// TestNamespaceAllocator_ZeroReservedSum verifies a purely proportional
// distribution when all reserved values are zero.
func TestNamespaceAllocator_ZeroReservedSum(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "half", Weight: 50, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "third", Weight: 30, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "fifth", Weight: 20, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	if !approxEqual(got["half"], 50, 2) {
		t.Errorf("half allocation = %d, want ~50", got["half"])
	}
	if !approxEqual(got["third"], 30, 2) {
		t.Errorf("third allocation = %d, want ~30", got["third"])
	}
	if !approxEqual(got["fifth"], 20, 2) {
		t.Errorf("fifth allocation = %d, want ~20", got["fifth"])
	}
}

// TestNamespaceAllocator_ReservedExceedsBudget verifies reserved floors are
// proportionally scaled down when their total exceeds the budget.
func TestNamespaceAllocator_ReservedExceedsBudget(t *testing.T) {
	a := NewNamespaceAllocator(80)
	namespaces := []database.Namespace{
		{ID: "ns-40", Weight: 10, Reserved: 40, HardCap: 0, Enabled: true},
		{ID: "ns-30", Weight: 10, Reserved: 30, HardCap: 0, Enabled: true},
		{ID: "ns-30b", Weight: 10, Reserved: 30, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)
	sum := got["ns-40"] + got["ns-30"] + got["ns-30b"]

	if !approxEqual(sum, 80, 2) {
		t.Errorf("sum = %d, want ~80", sum)
	}
	// Reserved values sum to 100; scale factor is 80/100 = 0.8, floored to int.
	if !approxEqual(got["ns-40"], 32, 2) {
		t.Errorf("ns-40 allocation = %d, want ~32 (scaled reserved)", got["ns-40"])
	}
	if !approxEqual(got["ns-30"], 24, 2) {
		t.Errorf("ns-30 allocation = %d, want ~24 (scaled reserved)", got["ns-30"])
	}
	if !approxEqual(got["ns-30b"], 24, 2) {
		t.Errorf("ns-30b allocation = %d, want ~24 (scaled reserved)", got["ns-30b"])
	}
}

// TestNamespaceAllocator_AllDisabled verifies that an all-disabled namespace
// list returns an empty allocation map.
func TestNamespaceAllocator_AllDisabled(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "ns-a", Weight: 50, Reserved: 10, HardCap: 0, Enabled: false},
		{ID: "ns-b", Weight: 50, Reserved: 10, HardCap: 0, Enabled: false},
	}

	got := a.Allocate(namespaces)

	if len(got) != 0 {
		t.Errorf("len(map) = %d, want 0 for all-disabled namespaces", len(got))
	}
}

// TestNamespaceAllocator_SetBudget verifies SetBudget changes the effective
// budget and subsequent allocations reflect the new total.
func TestNamespaceAllocator_SetBudget(t *testing.T) {
	namespaces := []database.Namespace{
		{ID: "ns-a", Weight: 50, Reserved: 10, HardCap: 0, Enabled: true},
		{ID: "ns-b", Weight: 50, Reserved: 10, HardCap: 0, Enabled: true},
	}

	a := NewNamespaceAllocator(100)
	got100 := a.Allocate(namespaces)
	sum100 := got100["ns-a"] + got100["ns-b"]
	if !approxEqual(sum100, 100, 2) {
		t.Errorf("initial sum = %d, want ~100", sum100)
	}

	a.SetBudget(50)
	got50 := a.Allocate(namespaces)
	sum50 := got50["ns-a"] + got50["ns-b"]
	if !approxEqual(sum50, 50, 2) {
		t.Errorf("after SetBudget(50) sum = %d, want ~50", sum50)
	}
}

// benchNamespaceAllocatorFixtures returns n namespace fixtures with mixed
// weight/reserved/hardcap so both Phase 1 (reserved floor) and Phase 2
// (proportional remainder) paths execute during the benchmark.
//
// Pattern: 1/3 with reserved+hardcap, 1/3 weight-only, 1/3 weight+reserved.
func benchNamespaceAllocatorFixtures(n int) []database.Namespace {
	out := make([]database.Namespace, n)
	for i := 0; i < n; i++ {
		mod := i % 3
		ns := database.Namespace{
			ID:      fmt.Sprintf("ns-%04d", i),
			Weight:  ((i * 7) % 30) + 1, // 1..30
			Enabled: true,
		}
		switch mod {
		case 0:
			ns.Reserved = ((i * 3) % 10) + 1 // 1..10
			ns.HardCap = ((i * 11) % 50) + 20
		case 1:
			// weight only, no reserved, no hardcap
		case 2:
			ns.Reserved = ((i * 5) % 7) + 1
			// hardcap = 0 means no cap
		}
		out[i] = ns
	}
	return out
}

// BenchmarkAllocate measures NamespaceAllocator.Allocate() across namespace
// counts. Allocate is pure CPU work — no DB, no goroutines — so the benchmark
// reflects algorithmic cost directly.
func BenchmarkAllocate(b *testing.B) {
	for _, n := range []int{3, 10, 50} {
		fixtures := benchNamespaceAllocatorFixtures(n)
		alloc := NewNamespaceAllocator(100)

		b.Run(fmt.Sprintf("Namespaces=%d", n), func(b *testing.B) {
			b.ResetTimer()
			var sink int
			for i := 0; i < b.N; i++ {
				got := alloc.Allocate(fixtures)
				// Touch the result so dead-code elimination can't elide the call.
				for _, v := range got {
					sink += v
				}
			}
			// Force sink to escape.
			if sink == 0 {
				b.Fatalf("sink stayed 0 across %d iterations", b.N)
			}
		})
	}
}

// BenchmarkAllocate_ReservedExceedsBudget exercises the reserved-scaling
// branch (when sum of reserved floors > budget) — the warn-log path is the
// most expensive branch in the allocator.
func BenchmarkAllocate_ReservedExceedsBudget(b *testing.B) {
	n := 20
	nss := make([]database.Namespace, n)
	for i := 0; i < n; i++ {
		// Each namespace reserves 10, total reserved = 200, budget = 80.
		nss[i] = database.Namespace{
			ID:       fmt.Sprintf("ns-%04d", i),
			Weight:   10,
			Reserved: 10,
			Enabled:  true,
		}
	}
	alloc := NewNamespaceAllocator(80)

	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		got := alloc.Allocate(nss)
		for _, v := range got {
			sink += v
		}
	}
	if sink == 0 {
		b.Fatalf("sink stayed 0 across %d iterations", b.N)
	}
}

// TestNamespaceAllocator_ZeroWeightNamespaces verifies namespaces with zero
// weight are treated as weight 1 each and the remainder is distributed equally.
func TestNamespaceAllocator_ZeroWeightNamespaces(t *testing.T) {
	a := NewNamespaceAllocator(100)
	namespaces := []database.Namespace{
		{ID: "ns-a", Weight: 0, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "ns-b", Weight: 0, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	if got["ns-a"] != got["ns-b"] {
		t.Errorf("allocations differ: ns-a=%d, ns-b=%d, want equal", got["ns-a"], got["ns-b"])
	}
	if !approxEqual(got["ns-a"]+got["ns-b"], 100, 2) {
		t.Errorf("sum = %d, want ~100", got["ns-a"]+got["ns-b"])
	}
	if got["ns-a"] != 50 && got["ns-a"] != 49 && got["ns-a"] != 51 {
		t.Errorf("ns-a allocation = %d, want ~50 (equal share of weight-0 namespaces)", got["ns-a"])
	}
}

// TestNamespaceAllocator_OvershootScalesToBudgetExactly pins the live
// overshoot case measured on 2026-10-02: 20 enabled namespaces whose reserved
// values summed to 92 against a global budget of 20. Under the old
// math.Floor scaling a reserved=45 floor became floor(9.78)=9, every small
// namespace floored to 0, and only 11 of the 20 slots were handed out.
// Largest-remainder scaling must place all 20 (SCHED-GAP-1697 criterion 4a).
func TestNamespaceAllocator_OvershootScalesToBudgetExactly(t *testing.T) {
	a := NewNamespaceAllocator(20)
	namespaces := []database.Namespace{
		{ID: "foreman", Weight: 100, Reserved: 45, HardCap: 0, Enabled: true},
		{ID: "ns-10", Weight: 10, Reserved: 10, HardCap: 0, Enabled: true},
		{ID: "ns-08", Weight: 10, Reserved: 8, HardCap: 0, Enabled: true},
		{ID: "ns-06", Weight: 10, Reserved: 6, HardCap: 0, Enabled: true},
		{ID: "ns-05", Weight: 10, Reserved: 5, HardCap: 0, Enabled: true},
		{ID: "ns-04", Weight: 10, Reserved: 4, HardCap: 0, Enabled: true},
		{ID: "ns-03", Weight: 10, Reserved: 3, HardCap: 0, Enabled: true},
		{ID: "ns-02a", Weight: 10, Reserved: 2, HardCap: 0, Enabled: true},
		{ID: "ns-02b", Weight: 5, Reserved: 2, HardCap: 0, Enabled: true},
		{ID: "ns-01a", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01b", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01c", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01d", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01e", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01f", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-01g", Weight: 10, Reserved: 1, HardCap: 0, Enabled: true},
		{ID: "ns-00a", Weight: 10, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "ns-00b", Weight: 10, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "ns-00c", Weight: 10, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "ns-00d", Weight: 10, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	// Exact scaled reserved: 45*20/92=9.78→9 (+1), 10→2.17→2, 8→1.74→1 (+1),
	// 6→1.30→1, 5→1.09→1, 4→0.87→0 (+1), 3→0.65→0 (+1), 2→0.43→0 (+1 twice).
	// Floors total 14; the 6 leftovers go to .87(4), .78(45), .74(8),
	// .65(3) and the two .43 reserved=2 namespaces.
	want := map[string]int{
		"foreman": 10, "ns-10": 2, "ns-08": 2, "ns-06": 1, "ns-05": 1,
		"ns-04": 1, "ns-03": 1, "ns-02a": 1, "ns-02b": 1,
		"ns-01a": 0, "ns-01b": 0, "ns-01c": 0, "ns-01d": 0,
		"ns-01e": 0, "ns-01f": 0, "ns-01g": 0,
		"ns-00a": 0, "ns-00b": 0, "ns-00c": 0, "ns-00d": 0,
	}
	assertAllocation(t, got, want, 20)
}

// TestNamespaceAllocator_LiveRemainderShape pins the live shape today: the
// foreman namespace carries the only reserved floor (8) against a budget of
// 20, so 12 slots are apportioned across 15 satellite namespaces by weight
// (10 or 5). The old math.Floor phase gave 12*10/225 = 0 to every satellite
// and placed only 9 of 20; largest-remainder hands all 12 remainder slots to
// satellites and leaves none on the floor (SCHED-GAP-1697 criterion 4b).
//
// 15 satellites compete for 12 slots, so 3 must legitimately receive 0 — the
// allocator cannot invent capacity. The "every weight>0, reserved=0
// namespace gets >= 1 when slots remain" guarantee is proven separately in
// TestNamespaceAllocator_MinimumOneForZeroReserved.
func TestNamespaceAllocator_LiveRemainderShape(t *testing.T) {
	a := NewNamespaceAllocator(20)
	namespaces := []database.Namespace{
		{ID: "coding-hermes", Weight: 100, Reserved: 8, HardCap: 0, Enabled: true},
	}
	// 15 satellites: ten at weight 10, five at weight 5 (sum 125).
	for i := 1; i <= 15; i++ {
		w := 10
		if i > 10 {
			w = 5
		}
		namespaces = append(namespaces, database.Namespace{
			ID:       fmt.Sprintf("sat-%02d", i),
			Weight:   w,
			Reserved: 0,
			HardCap:  0,
			Enabled:  true,
		})
	}

	got := a.Allocate(namespaces)

	// Weight sum 225. Exact shares: foreman 12*100/225=5.33→5, each weight-10
	// satellite 0.53→0, each weight-5 satellite 0.27→0. Floors total 5; the 7
	// leftovers go to the largest remainders — the ten .53 satellites — and
	// the tie-break (equal reserved, equal weight, lexicographically smaller
	// ID) gives them to sat-01..sat-07.
	want := map[string]int{
		"coding-hermes": 13,
		"sat-01":        1, "sat-02": 1, "sat-03": 1, "sat-04": 1,
		"sat-05": 1, "sat-06": 1, "sat-07": 1,
		"sat-08": 0, "sat-09": 0, "sat-10": 0,
		"sat-11": 0, "sat-12": 0, "sat-13": 0, "sat-14": 0, "sat-15": 0,
	}
	assertAllocation(t, got, want, 20)
}

// TestNamespaceAllocator_MinimumOneForZeroReserved proves the minimum-one
// guarantee (SCHED-GAP-1697 criterion 2): when the budget exceeds the total
// reserved floors, a weight>0 / reserved=0 namespace must receive at least one
// slot even if a heavy foreman would otherwise swallow the whole remainder by
// weight. A namespace allocated 0 is skipped by the packer, so this is the
// difference between "idle satellite" and "starved satellite".
func TestNamespaceAllocator_MinimumOneForZeroReserved(t *testing.T) {
	a := NewNamespaceAllocator(20)
	namespaces := []database.Namespace{
		{ID: "coding-hermes", Weight: 100, Reserved: 8, HardCap: 0, Enabled: true},
		{ID: "sat-a", Weight: 1, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "sat-b", Weight: 1, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "sat-c", Weight: 1, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	// remainder = 12. Each of the 3 satellites is seeded 1 slot (12 >= 3),
	// leaving 9: floor(9*100/103)=8 to the foreman, floor(9*1/103)=0 to each
	// satellite, and the one leftover slot goes to the largest remainder
	// (foreman .74 vs satellite .09). Foreman: 8+8+1=17; satellites 1 each.
	for _, id := range []string{"sat-a", "sat-b", "sat-c"} {
		if got[id] < 1 {
			t.Errorf("%s allocation = %d, want >= 1 (weight>0, reserved=0, slots remain)", id, got[id])
		}
	}
	if got["coding-hermes"] < 8 {
		t.Errorf("coding-hermes allocation = %d, want >= 8 (reserved floor)", got["coding-hermes"])
	}
	assertAllocation(t, got, map[string]int{
		"coding-hermes": 17, "sat-a": 1, "sat-b": 1, "sat-c": 1,
	}, 20)
}

// TestNamespaceAllocator_ReservedScalingSumsExactlyToBudget proves the
// scaling half of SCHED-GAP-1697 criterion 3: when reserved total exceeds the
// budget, largest-remainder scaling makes the scaled reserved values sum
// EXACTLY to the budget, so no slot is dropped by the phase-1 floors.
// Seven namespaces reserving 10 each (total 70) against a budget of 20 have
// exact shares of 20*10/70 = 2.857, which floor-only scaling leaves at 14.
func TestNamespaceAllocator_ReservedScalingSumsExactlyToBudget(t *testing.T) {
	a := NewNamespaceAllocator(20)
	namespaces := make([]database.Namespace, 0, 7)
	for i := 1; i <= 7; i++ {
		namespaces = append(namespaces, database.Namespace{
			ID:       fmt.Sprintf("ns-%d", i),
			Weight:   10,
			Reserved: 10,
			HardCap:  0,
			Enabled:  true,
		})
	}

	got := a.Allocate(namespaces)

	// Floors are 2 each (14); the 6 leftovers go to equal fractional
	// remainders (.857) and the tie-break (equal reserved/weight →
	// lexicographically smaller ID) gives them to ns-1..ns-6.
	want := map[string]int{
		"ns-1": 3, "ns-2": 3, "ns-3": 3, "ns-4": 3,
		"ns-5": 3, "ns-6": 3, "ns-7": 2,
	}
	assertAllocation(t, got, want, 20)
}

// TestNamespaceAllocator_ReservedFloorNotScaledUnderBudget proves the other
// half of SCHED-GAP-1697 criterion 3: while the budget covers the reserved
// total, a namespace's reserved floor is never scaled below its value. The
// binding budget (30) still gets fully placed by the weight phase.
func TestNamespaceAllocator_ReservedFloorNotScaledUnderBudget(t *testing.T) {
	a := NewNamespaceAllocator(30)
	namespaces := []database.Namespace{
		{ID: "coding-hermes", Weight: 100, Reserved: 8, HardCap: 0, Enabled: true},
		{ID: "sat-x", Weight: 1, Reserved: 0, HardCap: 0, Enabled: true},
	}

	got := a.Allocate(namespaces)

	if got["coding-hermes"] < 8 {
		t.Errorf("coding-hermes allocation = %d, want >= 8 (floor must not be scaled when budget >= rTotal)", got["coding-hermes"])
	}
	// remainder = 22; sat-x is seeded 1, leaving 21: floor(21*100/101)=20 to
	// the foreman, floor(21/101)=0 to sat-x, and the leftover goes to the
	// foreman (.79 vs .20). Foreman: 8+20+1=29.
	assertAllocation(t, got, map[string]int{"coding-hermes": 29, "sat-x": 1}, 30)
}

// TestNamespaceAllocator_NoSlotLeftUnallocated proves SCHED-GAP-1697
// criterion 1 across shapes where the old floor-only arithmetic under-filled
// the budget: whenever at least one namespace has a runnable lane (positive
// weight or reserved), the allocation total equals the budget exactly.
func TestNamespaceAllocator_NoSlotLeftUnallocated(t *testing.T) {
	cases := []struct {
		name       string
		budget     int
		namespaces []database.Namespace
	}{
		{
			name:   "dominating-weight-foreman",
			budget: 20,
			namespaces: []database.Namespace{
				{ID: "foreman", Weight: 1000, Reserved: 0, Enabled: true},
				{ID: "s1", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "s2", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "s3", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "s4", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "s5", Weight: 1, Reserved: 0, Enabled: true},
			},
		},
		{
			name:   "small-budget-equal-shares",
			budget: 7,
			namespaces: []database.Namespace{
				{ID: "a", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "b", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "c", Weight: 1, Reserved: 0, Enabled: true},
			},
		},
		{
			name:   "reserved-total-equals-budget",
			budget: 20,
			namespaces: []database.Namespace{
				{ID: "foreman", Weight: 100, Reserved: 8, Enabled: true},
				{ID: "sat", Weight: 10, Reserved: 0, Enabled: true},
			},
		},
		{
			name:   "one-weighted-namespace-and-a-flock-of-zeros",
			budget: 20,
			namespaces: []database.Namespace{
				{ID: "heavy", Weight: 50, Reserved: 0, Enabled: true},
				{ID: "z1", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z2", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z3", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z4", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z5", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z6", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z7", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z8", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z9", Weight: 1, Reserved: 0, Enabled: true},
				{ID: "z10", Weight: 1, Reserved: 0, Enabled: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewNamespaceAllocator(tc.budget)
			got := a.Allocate(tc.namespaces)
			sum := 0
			for _, v := range got {
				sum += v
			}
			if sum != tc.budget {
				t.Errorf("total allocated = %d, want %d (budget %d must be fully placed)", sum, tc.budget, tc.budget)
			}
		})
	}
}

// TestNamespaceAllocator_PreservesHardCapAndDisabled proves the surviving
// guarantees (SCHED-GAP-1697 criterion 5): an enabled namespace's HardCap > 0
// still caps its final allocation after the largest-remainder distribution,
// and disabled namespaces are still excluded entirely.
func TestNamespaceAllocator_PreservesHardCapAndDisabled(t *testing.T) {
	a := NewNamespaceAllocator(40)
	namespaces := []database.Namespace{
		{ID: "ns-capped", Weight: 10, Reserved: 0, HardCap: 3, Enabled: true},
		{ID: "ns-open", Weight: 10, Reserved: 0, HardCap: 0, Enabled: true},
		{ID: "ns-disabled", Weight: 10, Reserved: 0, HardCap: 0, Enabled: false},
	}

	got := a.Allocate(namespaces)

	if _, ok := got["ns-disabled"]; ok {
		t.Errorf("disabled namespace present in allocation: %v", got)
	}
	if got["ns-capped"] != 3 {
		t.Errorf("ns-capped allocation = %d, want 3 (hard cap applies after distribution)", got["ns-capped"])
	}
	// The cap legitimately leaves slack (3 + 20 = 23 < 40); what matters is
	// that the cap is still the last word and the un-capped sibling keeps the
	// full largest-remainder share.
	if got["ns-open"] != 20 {
		t.Errorf("ns-open allocation = %d, want 20", got["ns-open"])
	}
}
