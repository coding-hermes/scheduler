package scheduler

import (
	"log"
	"math"
	"sort"

	"github.com/coding-hermes/scheduler/internal/database"
)

// NamespaceAllocator distributes the global budget across namespaces using a
// two-phase algorithm (S07 §4.1 Phase 1): guaranteed reserved floors first,
// then proportional distribution of the remainder by namespace weight, each
// capped by hard_cap.
//
// SCHED-GAP-1697: both phases apportion with largest-remainder (Hamilton)
// arithmetic instead of a per-namespace math.Floor. Flooring alone silently
// dropped every slot that did not divide cleanly: on the live 2026-10-02 fleet
// (48 reserved units against a budget of 20, then a 12-slot remainder spread
// over 15 satellites of weight 10/5) the allocator placed 9-11 of the 20
// slots. The leftover is now handed out one slot at a time to the namespaces
// with the largest fractional remainders, so no slot is left on the floor and
// a weight>0 namespace is no longer floored to an unrunnable 0.
type NamespaceAllocator struct {
	budget int
}

// NewNamespaceAllocator creates an allocator with the given global budget.
func NewNamespaceAllocator(budget int) *NamespaceAllocator {
	return &NamespaceAllocator{budget: budget}
}

// SetBudget updates the global budget at runtime.
func (a *NamespaceAllocator) SetBudget(b int) {
	a.budget = b
}

// allocOrderKey carries the deterministic leftover tie-break fields for one
// namespace (SCHED-GAP-1697): larger reserved wins, then larger weight, then
// lexicographically smaller ID. The order keys are index-aligned with the
// enabled-namespace slice the allocator works on.
type allocOrderKey struct {
	id       string
	reserved int
	weight   int
}

// apportionSeats distributes seats slots across the items described by shares
// (each treated as >= 0) using largest-remainder (Hamilton) apportionment:
//
//  1. give item i floor(shares[i] * seats / sum(shares)) slots, then
//  2. hand the leftover slots out one each, in descending fractional-remainder
//     order, breaking ties with the order keys (larger reserved, then larger
//     weight, then lexicographically smaller ID).
//
// The returned slice has len(shares) entries and sums to seats whenever
// seats > 0 and at least one share is positive. Parallel to shares, order
// supplies the tie-break keys by item index.
func apportionSeats(shares []float64, seats int, order []allocOrderKey) []int {
	out := make([]int, len(shares))
	if seats <= 0 || len(shares) == 0 {
		return out
	}

	total := 0.0
	for _, s := range shares {
		if s > 0 {
			total += s
		}
	}
	if total <= 0 {
		// Nothing to apportion by. Hand the slots to the first items in the
		// caller's order rather than dropping them.
		for i := 0; i < seats && i < len(out); i++ {
			out[i] = 1
		}
		return out
	}

	type remainder struct {
		idx  int
		frac float64
	}
	rems := make([]remainder, len(shares))
	assigned := 0
	for i, s := range shares {
		if s < 0 {
			s = 0
		}
		exact := s * float64(seats) / total
		fl := math.Floor(exact)
		out[i] = int(fl)
		assigned += out[i]
		rems[i] = remainder{idx: i, frac: exact - fl}
	}

	leftover := seats - assigned
	if leftover <= 0 {
		return out
	}

	sort.SliceStable(rems, func(x, y int) bool {
		if rems[x].frac != rems[y].frac {
			return rems[x].frac > rems[y].frac
		}
		kx, ky := order[rems[x].idx], order[rems[y].idx]
		if kx.reserved != ky.reserved {
			return kx.reserved > ky.reserved
		}
		if kx.weight != ky.weight {
			return kx.weight > ky.weight
		}
		return kx.id < ky.id
	})
	for i := 0; i < leftover && i < len(rems); i++ {
		out[rems[i].idx]++
	}
	return out
}

// Allocate runs the two-phase distribution: reserved floor + proportional
// remainder. Returns a map of namespace_id → allocated budget.
//
// Disabled namespaces are excluded entirely. If the sum of reserved floors
// exceeds the budget, all reserved values are proportionally scaled down with
// largest-remainder apportionment so the scaled values sum EXACTLY to the
// budget. The remainder is then distributed by weight with the same method,
// with a minimum-one guarantee for weight>0 / reserved==0 namespaces (since a
// packer skips any namespace whose allocation is 0). HardCap > 0 caps the
// final allocation; HardCap == 0 means no cap.
func (a *NamespaceAllocator) Allocate(namespaces []database.Namespace) map[string]int {
	result := make(map[string]int)

	// --- Phase 0: filter to enabled namespaces ---
	enabled := make([]database.Namespace, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns.Enabled {
			enabled = append(enabled, ns)
		}
	}
	if len(enabled) == 0 {
		return result
	}

	budget := a.budget
	if budget < 0 {
		budget = 0
	}

	// Deterministic leftover tie-break keys, index-aligned with `enabled`.
	order := make([]allocOrderKey, len(enabled))
	for i, ns := range enabled {
		order[i] = allocOrderKey{id: ns.ID, reserved: ns.Reserved, weight: ns.Weight}
	}

	// --- Phase 1: reserved floors ---
	// Sum reserved across all enabled namespaces.
	rTotal := 0
	for _, ns := range enabled {
		rTotal += ns.Reserved
	}

	// If R_total exceeds budget, scale each reserved value down with
	// largest-remainder apportionment (floors alone leave slots unused).
	reserved := make([]int, len(enabled))
	if rTotal > budget {
		log.Printf("WARN NamespaceAllocator: total reserved %d exceeds budget %d; scaling reserved proportionally (largest-remainder)",
			rTotal, budget)
		shares := make([]float64, len(enabled))
		for i, ns := range enabled {
			r := ns.Reserved
			if r < 0 {
				r = 0
			}
			shares[i] = float64(r)
		}
		reserved = apportionSeats(shares, budget, order)
	} else {
		for i, ns := range enabled {
			r := ns.Reserved
			if r < 0 {
				r = 0
			}
			reserved[i] = r
		}
	}

	// Effective reserved total (after any scaling).
	reservedTotal := 0
	for _, r := range reserved {
		reservedTotal += r
	}

	// Remainder left for proportional distribution.
	remainder := budget - reservedTotal
	if remainder < 0 {
		remainder = 0
	}

	// --- Phase 2: proportional distribution of remainder by weight ---
	weights := make([]int, len(enabled))
	sumWeights := 0
	for i, ns := range enabled {
		w := ns.Weight
		if w < 0 {
			w = 0
		}
		weights[i] = w
		sumWeights += w
	}
	if sumWeights == 0 {
		log.Printf("WARN NamespaceAllocator: total weight is 0 across %d enabled namespaces; treating all as weight=1",
			len(enabled))
		for i := range weights {
			weights[i] = 1
		}
	}

	extra := make([]int, len(enabled))
	if remainder > 0 {
		// SCHED-GAP-1697: the remainder is apportioned by weight with the
		// same largest-remainder method as Phase 1, so shares that fall
		// below 1 no longer floor to an unrunnable 0 while nobody's share
		// is shifted by a pre-seed.
		shares := make([]float64, len(enabled))
		for i := range shares {
			shares[i] = float64(weights[i])
		}
		extra = apportionSeats(shares, remainder, order)
	}

	// SCHED-GAP-1697 minimum-one guarantee (owner ruling R3.4: "anytime
	// there is a slot open a foreman is running" — satellites take only
	// the remainder, but a remainder slot must actually be usable): any
	// weight>0 namespace that apportioned to 0 overall is upgraded to 1.
	// The upgrade is a TRANSFER, never a mint: each slot comes from the
	// largest surplus holder (extra > 1, or any extra when a reserved
	// floor keeps the donor's total >= 1), so the total stays exactly at
	// budget, no namespace's total ever drops below its reserved floor or
	// below 1 once held, and the loop runs to a fixpoint so an early
	// recipient can never be drained back to 0 by a later one. If no
	// eligible donor remains, the remaining zeros stand: the allocator
	// cannot invent capacity, and a hard-capped namespace at 0 is an
	// explicit operator decision rather than a floor artifact.
	for {
		upgraded := false
		for i := range enabled {
			if weights[i] <= 0 || reserved[i]+extra[i] > 0 {
				continue
			}
			donor := -1
			for j := range enabled {
				if j == i {
					continue
				}
				surplus := extra[j] > 1 || (reserved[j] > 0 && extra[j] > 0)
				if !surplus {
					continue
				}
				if donor == -1 || extra[j] > extra[donor] {
					donor = j
				}
			}
			if donor >= 0 {
				extra[i]++
				extra[donor]--
				upgraded = true
			}
		}
		if !upgraded {
			break
		}
	}

	for i, ns := range enabled {
		allocation := reserved[i] + extra[i]

		// Apply hard cap (0 means no cap).
		if ns.HardCap > 0 && allocation > ns.HardCap {
			allocation = ns.HardCap
		}

		result[ns.ID] = allocation
	}

	return result
}
