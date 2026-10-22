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

// reservedScale is the fixed-point precision used for reserved floors. Three
// decimal places preserve integer JSON/TOML compatibility plus common fractional
// floors such as 0.5, while keeping admission accumulation deterministic: all
// reservation arithmetic after decoding is integer arithmetic in milli-slots.
const reservedScale int64 = 1000

type allocOrderKey struct {
	id       string
	reserved int64
	weight   int
}

// reservedToUnits converts the public float64 representation at one boundary.
// The allocator never compares or accumulates float64 values; math.Round makes
// the fixed-point conversion deterministic for values such as 0.5.
func reservedToUnits(reserved float64) int64 {
	if reserved <= 0 || math.IsNaN(reserved) || math.IsInf(reserved, 0) {
		return 0
	}
	return int64(math.Round(reserved * float64(reservedScale)))
}

// apportionIntegerShares distributes integer seats with Hamilton's
// largest-remainder method. Using integer quotients and remainders avoids
// floating-point equality in the admission path.
func apportionIntegerShares(shares []int64, seats int64, order []allocOrderKey) []int64 {
	out := make([]int64, len(shares))
	if seats <= 0 || len(shares) == 0 {
		return out
	}

	var total int64
	for _, share := range shares {
		if share > 0 {
			total += share
		}
	}
	if total <= 0 {
		for i := int64(0); i < seats && i < int64(len(out)); i++ {
			out[i] = 1
		}
		return out
	}

	type remainder struct {
		idx  int
		frac int64
	}
	rems := make([]remainder, len(shares))
	var assigned int64
	for i, share := range shares {
		if share < 0 {
			share = 0
		}
		numerator := share * seats
		out[i] = numerator / total
		assigned += out[i]
		rems[i] = remainder{idx: i, frac: numerator % total}
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
	for i := int64(0); i < leftover && i < int64(len(rems)); i++ {
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
		order[i] = allocOrderKey{id: ns.ID, reserved: reservedToUnits(ns.Reserved), weight: ns.Weight}
	}

	// --- Phase 1: reserved floors ---
	// Convert reservations to fixed-point units once at the public type
	// boundary. This lets fractional floors accumulate without float equality.
	reservedUnits := make([]int64, len(enabled))
	var rTotal int64
	for i, ns := range enabled {
		reservedUnits[i] = reservedToUnits(ns.Reserved)
		rTotal += reservedUnits[i]
	}

	// Sum reserved across all enabled namespaces.
	reserved := make([]int, len(enabled))
	budgetUnits := int64(budget) * reservedScale
	if rTotal <= budgetUnits {
		// Whole floors are preserved per namespace. Fractional credits are
		// pooled and the resulting integer slots go to the largest credits.
		var fractionalCredits int64
		fractional := make([]int64, len(enabled))
		for i, units := range reservedUnits {
			reserved[i] = int(units / reservedScale)
			fractional[i] = units % reservedScale
			fractionalCredits += fractional[i]
		}
		fractionalCredits /= reservedScale
		if fractionalCredits > 0 {
			credits := apportionIntegerShares(fractional, fractionalCredits, order)
			for i, credit := range credits {
				reserved[i] += int(credit)
			}
		}
	} else {
		// If R_total exceeds budget, scale each reserved value down with
		// largest-remainder apportionment (floors alone leave slots unused).
		log.Printf("WARN NamespaceAllocator: total reserved %d exceeds budget %d; scaling reserved proportionally (largest-remainder)",
			rTotal, budgetUnits)
		reserved64 := apportionIntegerShares(reservedUnits, int64(budget), order)
		for i, value := range reserved64 {
			reserved[i] = int(value)
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
		shares := make([]int64, len(enabled))
		for i := range shares {
			shares[i] = int64(weights[i])
		}
		extra64 := apportionIntegerShares(shares, int64(remainder), order)
		for i, value := range extra64 {
			extra[i] = int(value)
		}
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
