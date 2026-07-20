package scheduler

import (
	"sort"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

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

		// Sort by urgency descending, then priority, then last-tick ASC.
		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].Urgency != scored[j].Urgency {
				return scored[i].Urgency > scored[j].Urgency
			}
			if scored[i].Project.Priority != scored[j].Project.Priority {
				return scored[i].Project.Priority > scored[j].Project.Priority
			}
			// Older last-tick = higher priority.
			li, iOk := lastCompleted[scored[i].Project.Name]
			lj, jOk := lastCompleted[scored[j].Project.Name]
			if !iOk && jOk {
				return true
			}
			if iOk && !jOk {
				return false
			}
			if iOk && jOk {
				return li.Before(lj)
			}
			return scored[i].Project.Name < scored[j].Project.Name
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
				Name:           pu.Project.Name,
				Priority:       float64(pu.Project.Priority),
				Weight:         pu.EffectiveWeight,
				Urgency:        pu.Urgency,
				Workdir:        pu.Project.Workdir,
				RepoURL:        pu.Project.RepoURL,
				Command:        pu.Project.Command,
				Model:          pu.Project.Model,
				Provider:       pu.Project.Provider,
				WorkerModel:    pu.Project.WorkerModel,
				WorkerProvider: pu.Project.WorkerProvider,
				Deliver:        pu.Project.Deliver,
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
