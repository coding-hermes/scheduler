package scheduler

import (
	"log"
	"math"

	"github.com/coding-herms/scheduler/internal/database"
)

// NamespaceAllocator distributes the global budget across namespaces using a
// two-phase algorithm (S07 §4.1 Phase 1): guaranteed reserved floors first,
// then proportional distribution of the remainder by namespace weight, each
// capped by hard_cap.
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

// Allocate runs the two-phase distribution: reserved floor + proportional
// remainder. Returns a map of namespace_id → allocated budget.
//
// Disabled namespaces are excluded entirely. If the sum of reserved floors
// exceeds the budget, all reserved values are proportionally scaled down. The
// remainder is then distributed by weight. HardCap > 0 caps the final
// allocation; HardCap == 0 means no cap.
func (a *NamespaceAllocator) Allocate(namespaces []database.Namespace) map[string]int {
	result := make(map[string]int)

	// --- Phase 0: filter to enabled namespaces ---
	var enabled []database.Namespace
	for _, ns := range namespaces {
		if ns.Enabled {
			enabled = append(enabled, ns)
		}
	}
	if len(enabled) == 0 {
		return result
	}

	budget := a.budget

	// --- Phase 1: reserved floors ---
	// Sum reserved across all enabled namespaces.
	rTotal := 0
	for _, ns := range enabled {
		rTotal += ns.Reserved
	}

	// If R_total exceeds budget, proportionally scale each reserved value down.
	reserved := make([]float64, len(enabled))
	if rTotal > budget {
		log.Printf("WARN NamespaceAllocator: total reserved %d exceeds budget %d; scaling reserved proportionally",
			rTotal, budget)
		scale := float64(budget) / float64(rTotal)
		for i, ns := range enabled {
			reserved[i] = math.Floor(float64(ns.Reserved) * scale)
		}
	} else {
		for i, ns := range enabled {
			reserved[i] = float64(ns.Reserved)
		}
	}

	// Effective reserved total after any scaling.
	rEffective := 0.0
	for _, r := range reserved {
		rEffective += r
	}

	// Remainder left for proportional distribution.
	remainder := float64(budget) - rEffective
	if remainder < 0 {
		remainder = 0
	}

	// --- Phase 2: proportional distribution of remainder by weight ---
	weights := make([]int, len(enabled))
	sumWeights := 0
	for i, ns := range enabled {
		weights[i] = ns.Weight
		sumWeights += ns.Weight
	}
	if sumWeights == 0 {
		log.Printf("WARN NamespaceAllocator: total weight is 0 across %d enabled namespaces; treating all as weight=1",
			len(enabled))
		for i := range weights {
			weights[i] = 1
		}
		sumWeights = len(enabled)
	}

	for i, ns := range enabled {
		proportional := math.Floor((float64(weights[i]) / float64(sumWeights)) * remainder)
		allocation := int(reserved[i]) + int(proportional)

		// Apply hard cap (0 means no cap).
		if ns.HardCap > 0 && allocation > ns.HardCap {
			allocation = ns.HardCap
		}

		result[ns.ID] = allocation
	}

	return result
}
