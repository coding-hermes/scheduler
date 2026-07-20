package scheduler

import (
	"database/sql"
	"math"
	"sort"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

// ProjectUrgency holds a project along with its computed urgency and effective
// weight within a namespace. It is the unit of work passed between the
// intra-namespace packer and the borrowing engine.
type ProjectUrgency struct {
	Project         database.Project
	Urgency         float64
	EffectiveWeight int
}

// NamespaceTickData holds per-namespace utilization for a single evaluation
// cycle. It mirrors database.NamespaceTick but is a plain value type the
// packer produces without touching the DB.
type NamespaceTickData struct {
	NamespaceID string
	Allocated   int
	Used        int // sum of effective weights of selected projects
	Borrowed    int
	Lent        int
	JobCount    int
}

// PackResult holds the packing outcome.
type PackResult struct {
	Projects       []PackedProject     // selected projects across all namespaces
	NamespaceTicks []NamespaceTickData // per-namespace stats for recording
}

// ---------------------------------------------------------------------------
// MultiPoolPacker
// ---------------------------------------------------------------------------

// MultiPoolPacker implements the multi-namespace scheduling algorithm
// (S07 §4.1 Phases 2–3). It takes a set of projects + namespaces, runs
// per-namespace greedy packing, delegates idle-capacity redistribution to
// BorrowingEngine, and returns the selected projects + per-namespace stats.
//
// When no namespaces exist (or namespace mode is disabled) it falls back to
// the flat single-pool Packer.Pick method.
type MultiPoolPacker struct {
	allocator     *NamespaceAllocator
	maxConcurrent int
}

// NewMultiPoolPacker creates a packer with the given global budget and
// concurrency cap.
func NewMultiPoolPacker(budget, maxConcurrent int) *MultiPoolPacker {
	return &MultiPoolPacker{
		allocator:     NewNamespaceAllocator(budget),
		maxConcurrent: maxConcurrent,
	}
}

// FlatFallback delegates to the existing Packer.Pick for flat single-pool mode.
// The caller provides a configured *sql.DB; this is used when NamespaceMode=false
// or no namespaces exist.
func (m *MultiPoolPacker) FlatFallback(db *sql.DB, calc *UrgencyCalculator, budget int, now time.Time) ([]PackedProject, error) {
	p := NewPacker(db, calc, budget, m.maxConcurrent)
	return p.Pick(now, nil)
}

// ---------------------------------------------------------------------------
// BorrowingEngine
// ---------------------------------------------------------------------------

// BorrowingEngine redistributes idle namespace capacity from namespaces that
// have unused budget and no queued jobs to namespaces that have queued jobs
// (hit their allocation ceiling). One level only — no recursion.
type BorrowingEngine struct{}

// NewBorrowingEngine creates a borrowing engine.
func NewBorrowingEngine() *BorrowingEngine {
	return &BorrowingEngine{}
}

// Borrow redistributes idle namespace capacity.
//
// allocations:      namespace_id → current allocated budget (may be 0 for disabled)
// nsDetails:        full namespace info for hard_cap lookups
// queuedJobs:       namespace_id → list of projects still queued (didn't fit in Phase 2)
// selectedBudget:   namespace_id → already-consumed budget from Phase 2
//
// Returns the updated allocations map (same map, mutated in place).
func (b *BorrowingEngine) Borrow(
	allocations map[string]int,
	nsDetails []database.Namespace,
	queuedJobs map[string][]*ProjectUrgency,
	selectedBudget map[string]int,
) map[string]int {

	// Build a quick lookup for namespace details (hard_cap, weight).
	nsMap := make(map[string]database.Namespace, len(nsDetails))
	for _, ns := range nsDetails {
		nsMap[ns.ID] = ns
	}

	// Step 1: collect lenders (unused > 0 AND no queued jobs).
	lentPool := 0
	lenderContrib := make(map[string]int)
	for nsID, alloc := range allocations {
		used := selectedBudget[nsID]
		unused := alloc - used
		if unused > 0 && len(queuedJobs[nsID]) == 0 {
			lenderContrib[nsID] = unused
			lentPool += unused
		}
	}

	if lentPool == 0 {
		return allocations
	}

	// Step 2: build borrower list (has queued jobs).
	type borrower struct {
		nsID   string
		weight int
	}
	var borrowers []borrower
	for nsID, jobs := range queuedJobs {
		if len(jobs) > 0 {
			w := 1
			if ns, ok := nsMap[nsID]; ok {
				w = ns.Weight
			}
			borrowers = append(borrowers, borrower{nsID: nsID, weight: w})
		}
	}

	if len(borrowers) == 0 {
		return allocations
	}

	// Sort borrowers by namespace weight descending.
	sort.Slice(borrowers, func(i, j int) bool {
		return borrowers[i].weight > borrowers[j].weight
	})

	// Step 3: distribute lent_pool to borrowers.
	remainingPool := lentPool
	for _, br := range borrowers {
		if remainingPool <= 0 {
			break
		}

		ns, ok := nsMap[br.nsID]
		if !ok {
			continue
		}

		currentAlloc := allocations[br.nsID]

		// max_borrow = min(hard_cap - current_allocation, remaining_pool)
		hardCap := ns.HardCap
		if hardCap == 0 {
			hardCap = currentAlloc // no cap → can't borrow beyond what exists
		}
		headroom := hardCap - currentAlloc
		if headroom <= 0 {
			continue
		}

		maxBorrow := headroom
		if remainingPool < maxBorrow {
			maxBorrow = remainingPool
		}

		// Calculate need: sum of effective weights of queued jobs.
		need := 0
		for _, pu := range queuedJobs[br.nsID] {
			need += pu.EffectiveWeight
		}

		borrowAmount := need
		if borrowAmount > maxBorrow {
			borrowAmount = maxBorrow
		}

		if borrowAmount <= 0 {
			continue
		}

		allocations[br.nsID] = currentAlloc + borrowAmount
		remainingPool -= borrowAmount
	}

	// Step 4: reduce lender allocations by what they contributed.
	// Only deduct what was actually consumed from the pool.
	consumed := lentPool - remainingPool
	for nsID, contrib := range lenderContrib {
		if consumed <= 0 {
			break
		}
		deduct := contrib
		if deduct > consumed {
			deduct = consumed
		}
		allocations[nsID] -= deduct
		consumed -= deduct
	}

	return allocations
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// CalcEffectiveWeight computes the scaled weight of a project within its namespace.
// Formula: floor(alloc × (projectWeight / totalWeightInNS)), floored at 1.
// Exported for testing and external reuse.
func CalcEffectiveWeight(projectWeight, totalWeightInNS, alloc int) int {
	if totalWeightInNS <= 0 {
		return 1
	}
	raw := math.Floor(float64(projectWeight) / float64(totalWeightInNS) * float64(alloc))
	ew := int(raw)
	if ew < 1 {
		ew = 1
	}
	return ew
}

// puInList checks whether a *ProjectUrgency pointer is already in a slice.
func puInList(target *ProjectUrgency, list []*ProjectUrgency) bool {
	for _, p := range list {
		if p == target {
			return true
		}
	}
	return false
}

// sumSelectedWeights returns the sum of EffectiveWeight for already-selected
// projects — used when recalculating total weight for re-packing.
func sumSelectedWeights(selected []*ProjectUrgency) int {
	total := 0
	for _, pu := range selected {
		total += pu.Project.Weight
	}
	return total
}
