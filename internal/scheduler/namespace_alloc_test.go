package scheduler

import (
	"fmt"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
)

// approxEqual reports whether got is within delta of want, avoiding brittle
// exact comparisons for proportional floors.
func approxEqual(got, want, delta int) bool {
	if got < want-delta || got > want+delta {
		return false
	}
	return true
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
