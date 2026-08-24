package scheduler

import (
	"database/sql"
	"log"
	"math"
	"sort"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
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
	allocator       *NamespaceAllocator
	maxConcurrent   int
	blackoutWindows []config.BlackoutWindow
	pendingCounter  *PendingTaskCounter
	// budgetGate, when non-nil, excludes budget-exhausted projects from
	// selection (SCHED-GAP-066). Installed per evaluation cycle by the loop;
	// nil = no budget enforcement (tests, spend-query failure fail-open).
	budgetGate BudgetGate
}

// NewMultiPoolPacker creates a packer with the given global budget and
// concurrency cap. The pending-task counter defaults to the package-level
// shared instance so existing call sites keep working unchanged.
func NewMultiPoolPacker(budget, maxConcurrent int, blackoutWindows []config.BlackoutWindow) *MultiPoolPacker {
	return &MultiPoolPacker{
		allocator:       NewNamespaceAllocator(budget),
		maxConcurrent:   maxConcurrent,
		blackoutWindows: blackoutWindows,
		pendingCounter:  defaultPendingCounter,
	}
}

// SetPendingCounter overrides the pending-task counter (for tests).
func (m *MultiPoolPacker) SetPendingCounter(c *PendingTaskCounter) {
	m.pendingCounter = c
}

// SetBudgetGate installs the per-cycle budget gate (SCHED-GAP-066). Pass nil
// to disable budget enforcement.
func (m *MultiPoolPacker) SetBudgetGate(g BudgetGate) {
	m.budgetGate = g
}

// budgetBlocked reports the block detail for a budget-exhausted project, or
// "" when the project may be scheduled. Beside the !p.Enabled checks in every
// selection path, an exhausted project is skipped and logged — never killed
// mid-run (running ticks are untouched by the packers).
func (m *MultiPoolPacker) budgetBlocked(p *database.Project) string {
	if m.budgetGate == nil {
		return ""
	}
	detail, blocked := m.budgetGate(p.Name, p.DailyBudgetUSD, p.WeeklyBudgetUSD, p.FinalBudgetUSD)
	if blocked {
		log.Printf("BUDGET: %s blocked (%s) — excluded from selection; running ticks untouched", p.Name, detail)
		return detail
	}
	return ""
}

// FlatFallback delegates to the existing Packer.Pick for flat single-pool mode.
// The caller provides a configured *sql.DB; this is used when NamespaceMode=false
// or no namespaces exist.
func (m *MultiPoolPacker) FlatFallback(db *sql.DB, calc *UrgencyCalculator, budget int, now time.Time) ([]PackedProject, error) {
	p := NewPacker(db, calc, budget, m.maxConcurrent, m.blackoutWindows)
	return p.Pick(now, nil)
}

// packFlat greedily packs an in-memory project list into PackedProjects using
// flat single-pool rules (urgency sort, cooldown check, budget, concurrency).
// Used for: (a) the no-namespaces fallback, and (b) projects whose NamespaceID
// is nil or points at a namespace that doesn't exist — they must never be
// silently dropped (Bane 2026-07-31).
// globalSelected is the number of projects already picked by namespace packing;
// budgetRemaining is what's left of the global weight budget after namespaces.
// Returns the newly packed projects (does NOT include namespace-picked ones).
func (m *MultiPoolPacker) packFlat(
	projects []database.Project,
	urgencyCalc *UrgencyCalculator,
	lastCompleted map[string]time.Time,
	runningSet map[string]bool,
	budgetRemaining int,
	globalSelected int,
	now time.Time,
) []PackedProject {
	if budgetRemaining <= 0 {
		return nil
	}

	type scored struct {
		proj     database.Project
		urgency  float64
		lastTick *time.Time
	}
	list := make([]scored, 0, len(projects))
	for i := range projects {
		p := &projects[i]
		if !p.Enabled {
			continue
		}
		// SCHED-GAP-066: budget-exhausted projects are never packed.
		if m.budgetBlocked(p) != "" {
			continue
		}
		if runningSet != nil && runningSet[p.Name] {
			continue
		}
		var lastTick *time.Time
		if lt, ok := lastCompleted[p.Name]; ok {
			lastTick = &lt
		}
		createdAt, _ := time.Parse(time.RFC3339, p.CreatedAt)
		urgency := urgencyCalc.ComputeUrgency(
			float64(p.Priority), p.DecayRate, now, lastTick, createdAt,
		)
		// S-GAP-001 fairness: starvation boost applies in the flat fallback
		// too, or the two selection paths would diverge. Monotonic in
		// starvation age so the most-starved project sorts first regardless
		// of priority (reopen 2026-08-05).
		if isStarving(p.CooldownS, p.ConsecutiveFailures, lastTick, createdAt, now) && urgency < starvationBoostUrgency {
			age := starvationAge(lastTick, createdAt, now)
			urgency = starvationBoostUrgencyFor(age)
			log.Printf("FAIRNESS: %s boosted in flat fallback (cooldown=%ds failures=%d window=%v starved=%v)",
				p.Name, p.CooldownS, p.ConsecutiveFailures, StarvationWindow(p.CooldownS), age)
		}
		// SCHED-GAP-019: a project with pending board tasks gets a urgency
		// boost below the starvation tier but far above organic urgency,
		// so freshly-pending work jumps the eligible queue. Cooldown is NOT
		// bypassed — the boost lives in the scoring loop only.
		if m.pendingCounter != nil {
			if pending := m.pendingCounter.CountPending(p.Workdir); pending > 0 && urgency < pendingBoostUrgency {
				urgency = pendingBoostUrgencyFor(pending)
			}
		}
		list = append(list, scored{proj: *p, urgency: urgency, lastTick: lastTick})
	}

	// Urgency desc, priority desc, oldest-last-tick first.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].urgency != list[j].urgency {
			return list[i].urgency > list[j].urgency
		}
		if list[i].proj.Priority != list[j].proj.Priority {
			return list[i].proj.Priority > list[j].proj.Priority
		}
		li, lj := list[i].lastTick, list[j].lastTick
		if li == nil && lj != nil {
			return true
		}
		if li != nil && lj == nil {
			return false
		}
		if li != nil && lj != nil {
			return li.Before(*lj)
		}
		return list[i].proj.Name < list[j].proj.Name
	})

	var packed []PackedProject
	used := 0
	selected := globalSelected
	for _, s := range list {
		if selected >= m.maxConcurrent {
			break
		}
		if s.proj.Weight > budgetRemaining {
			continue
		}
		// Cooldown check.
		if s.lastTick != nil {
			cooldownDur := time.Duration(s.proj.CooldownS) * time.Second
			// S-GAP-001: consecutive spawn failures back off exponentially.
			if s.proj.ConsecutiveFailures > 0 {
				cooldownDur = FailureBackoff(cooldownDur, s.proj.ConsecutiveFailures)
			}
			if mult, inBlackout := config.ActiveMultiplier(m.blackoutWindows, now); inBlackout {
				if mult <= 0 {
					continue
				}
				if mult > 1.0 {
					cooldownDur = time.Duration(float64(cooldownDur) * mult)
				}
			}
			if now.Sub(*s.lastTick) < cooldownDur {
				continue
			}
		}
		packed = append(packed, PackedProject{
			Name:             s.proj.Name,
			Priority:         float64(s.proj.Priority),
			Weight:           s.proj.Weight,
			Urgency:          s.urgency,
			Workdir:          s.proj.Workdir,
			RepoURL:          s.proj.RepoURL,
			Command:          s.proj.Command,
			Model:            s.proj.Model,
			Provider:         s.proj.Provider,
			FallbackModel:    s.proj.FallbackModel,
			FallbackProvider: s.proj.FallbackProvider,
			NoGlobalFallback: s.proj.NoGlobalFallback,
			IdleModel:        s.proj.IdleModel,
			IdleProvider:     s.proj.IdleProvider,
			WorkerModel:      s.proj.WorkerModel,
			WorkerProvider:   s.proj.WorkerProvider,
			GatewayKey:       s.proj.GatewayKey,
			Deliver:          s.proj.Deliver,
		})
		used += s.proj.Weight
		budgetRemaining -= s.proj.Weight
		selected++
	}

	if len(packed) > 0 {
		log.Printf("FLAT-FALLBACK: packed %d unassigned/fallback project(s), used=%d budget=%d",
			len(packed), used, m.allocator.budget)
	}
	return packed
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
