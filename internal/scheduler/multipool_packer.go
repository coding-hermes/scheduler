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

// Pack runs the full multi-pool algorithm and returns selected projects.
// Falls back to flat single-pool packing when no namespaces exist (the caller
// is expected to short-circuit on NamespaceMode=false, but we also handle it
// here defensively).
func (m *MultiPoolPacker) Pack(
	projects []database.Project,
	namespaces []database.Namespace,
	urgencyCalc *UrgencyCalculator,
	lastCompleted map[string]time.Time,
	running []string,
	now time.Time,
) PackResult {

	// --- Fallback: no namespaces → flat single-pool mode ---
	if len(namespaces) == 0 {
		return PackResult{Projects: []PackedProject{}, NamespaceTicks: nil}
	}

	// Phase 1 — allocation (already implemented by NamespaceAllocator).
	allocations := m.allocator.Allocate(namespaces)

	runningSet := make(map[string]bool, len(running))
	for _, name := range running {
		runningSet[name] = true
	}

	globalRunning := len(runningSet)
	globalSelected := 0

	type nsPackState struct {
		ns         database.Namespace
		alloc      int
		selected   []*ProjectUrgency
		queued     []*ProjectUrgency
		usedBudget int
	}
	states := make(map[string]*nsPackState)

	// --- Phase 2 — intra-namespace packing (per namespace) ---
	for _, ns := range namespaces {
		if !ns.Enabled {
			continue
		}
		alloc, ok := allocations[ns.ID]
		if !ok || alloc == 0 {
			continue
		}

		// Filter projects belonging to this namespace.
		var nsProjects []database.Project
		for i := range projects {
			p := &projects[i]
			if !p.Enabled {
				continue
			}
			if p.NamespaceID != nil && *p.NamespaceID == ns.ID {
				nsProjects = append(nsProjects, *p)
			}
		}
		if len(nsProjects) == 0 {
			states[ns.ID] = &nsPackState{ns: ns, alloc: alloc}
			continue
		}

		// Sum of all project weights in this namespace.
		totalWeightInNS := 0
		for _, p := range nsProjects {
			totalWeightInNS += p.Weight
		}
		if totalWeightInNS == 0 {
			totalWeightInNS = 1 // avoid div-by-zero
		}

		// Compute urgency + effective weight for each project.
		scored := make([]ProjectUrgency, 0, len(nsProjects))
		for _, p := range nsProjects {
			var lastTick *time.Time
			if lt, ok := lastCompleted[p.Name]; ok {
				lastTick = &lt
			}
			createdAt, _ := time.Parse(time.RFC3339, p.CreatedAt)
			urgency := urgencyCalc.ComputeUrgency(
				float64(p.Priority), p.DecayRate, now, lastTick, createdAt,
			)

			effW := CalcEffectiveWeight(p.Weight, totalWeightInNS, alloc)
			scored = append(scored, ProjectUrgency{
				Project:         p,
				Urgency:         urgency,
				EffectiveWeight: effW,
			})
		}

		// Sort by urgency descending.
		sort.Slice(scored, func(i, j int) bool {
			return scored[i].Urgency > scored[j].Urgency
		})

		// Greedy pack into namespace allocation.
		st := &nsPackState{ns: ns, alloc: alloc}
		budgetRemaining := alloc
		for i := range scored {
			pu := &scored[i]

			// Cooldown check.
			if lt, ok := lastCompleted[pu.Project.Name]; ok {
				cooldownDur := time.Duration(pu.Project.CooldownS) * time.Second
				if now.Sub(lt) < cooldownDur {
					continue
				}
			}

			// Concurrency cap check (global across all namespaces).
			if globalRunning+globalSelected >= m.maxConcurrent {
				break
			}

			// Budget check.
			if pu.EffectiveWeight > budgetRemaining {
				st.queued = append(st.queued, pu)
				continue
			}

			st.selected = append(st.selected, pu)
			budgetRemaining -= pu.EffectiveWeight
			globalSelected++
		}
		// Any remaining items (after budget/concurrency break) go to queued.
		for i := range scored {
			pu := &scored[i]
			if !puInList(pu, st.selected) && !puInList(pu, st.queued) {
				// Check if it was skipped by cooldown — those are NOT queued.
				if lt, ok := lastCompleted[pu.Project.Name]; ok {
					cooldownDur := time.Duration(pu.Project.CooldownS) * time.Second
					if now.Sub(lt) < cooldownDur {
						continue // cooldown-skip, not queued
					}
				}
				st.queued = append(st.queued, pu)
			}
		}

		st.usedBudget = alloc - budgetRemaining
		states[ns.ID] = st
	}

	// --- Phase 3 — borrowing ---
	selectedBudget := make(map[string]int, len(states))
	queuedJobs := make(map[string][]*ProjectUrgency, len(states))
	for id, st := range states {
		selectedBudget[id] = st.usedBudget
		queuedJobs[id] = st.queued
	}

	borrower := NewBorrowingEngine()
	newAllocations := borrower.Borrow(allocations, namespaces, queuedJobs, selectedBudget)

	// Re-pack borrowers that received extra budget.
	lentMap := make(map[string]int)   // how much each ns lent
	borrowMap := make(map[string]int) // how much each ns borrowed
	for id, oldAlloc := range allocations {
		newAlloc := newAllocations[id]
		if newAlloc > oldAlloc {
			borrowMap[id] = newAlloc - oldAlloc
		} else if newAlloc < oldAlloc {
			lentMap[id] = oldAlloc - newAlloc
		}
	}

	for id, st := range states {
		extra := borrowMap[id]
		if extra <= 0 {
			continue
		}
		newAlloc := newAllocations[id]
		budgetRemaining := newAlloc - st.usedBudget
		if budgetRemaining < 0 {
			budgetRemaining = 0
		}

		// Re-pack queued jobs with the new allocation.
		var stillQueued []*ProjectUrgency
		totalWeightInNS := 0
		for _, pu := range st.queued {
			totalWeightInNS += pu.Project.Weight
		}
		if totalWeightInNS == 0 && len(st.queued) > 0 {
			totalWeightInNS = 1
		}

		for _, pu := range st.queued {
			// Recalculate effective weight with the new (larger) allocation.
			effW := CalcEffectiveWeight(pu.Project.Weight, totalWeightInNS+sumSelectedWeights(st.selected), newAlloc)
			pu.EffectiveWeight = effW

			if globalRunning+globalSelected >= m.maxConcurrent {
				stillQueued = append(stillQueued, pu)
				continue
			}
			if pu.EffectiveWeight > budgetRemaining {
				stillQueued = append(stillQueued, pu)
				continue
			}
			st.selected = append(st.selected, pu)
			budgetRemaining -= pu.EffectiveWeight
			globalSelected++
		}
		st.queued = stillQueued
		st.alloc = newAlloc
		st.usedBudget = newAlloc - budgetRemaining
	}

	// Update allocations that were lent (for NamespaceTicks reporting).
	for id, st := range states {
		if lent, ok := lentMap[id]; ok && lent > 0 {
			// Lender's effective allocation is reduced for reporting.
			st.alloc = newAllocations[id]
		}
	}

	// --- Build PackResult ---
	result := PackResult{
		Projects:       make([]PackedProject, 0),
		NamespaceTicks: make([]NamespaceTickData, 0, len(states)),
	}

	for _, ns := range namespaces {
		st, ok := states[ns.ID]
		if !ok {
			continue
		}
		for _, pu := range st.selected {
			result.Projects = append(result.Projects, PackedProject{
				Name:     pu.Project.Name,
				Priority: float64(pu.Project.Priority),
				Weight:   pu.EffectiveWeight,
				Urgency:  pu.Urgency,
				Workdir:  pu.Project.Workdir,
				RepoURL:  pu.Project.RepoURL,
				Command:  pu.Project.Command,
				Model:    pu.Project.Model,
				Provider: pu.Project.Provider,
				Deliver:  pu.Project.Deliver,
			})
		}
		result.NamespaceTicks = append(result.NamespaceTicks, NamespaceTickData{
			NamespaceID: ns.ID,
			Allocated:   newAllocations[ns.ID],
			Used:        st.usedBudget,
			Borrowed:    borrowMap[ns.ID],
			Lent:        lentMap[ns.ID],
			JobCount:    len(st.selected),
		})
	}

	return result
}

// FlatFallback delegates to the existing Packer.Pick for flat single-pool mode.
// The caller provides a configured *sql.DB; this is used when NamespaceMode=false
// or no namespaces exist.
func (m *MultiPoolPacker) FlatFallback(db *sql.DB, calc *UrgencyCalculator, budget int, now time.Time) ([]PackedProject, error) {
	p := NewPacker(db, calc, budget, m.maxConcurrent)
	return p.Pick(now)
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
