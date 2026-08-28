package scheduler

import (
	"log"
	"sort"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// Pack runs the full multi-pool algorithm and returns selected projects.
// Falls back to flat single-pool packing when no namespaces exist (the caller
// is expected to short-circuit on NamespaceMode=false, but we also handle it
// here defensively). Projects with a nil NamespaceID or a namespace ID that
// does not exist (or is disabled) are NEVER silently dropped: they are
// flat-packed into the result with the remaining budget (Bane 2026-07-31 —
// "things are not running... allowing things to be assigned where they can't
// be without putting errors or having a fallback").
func (m *MultiPoolPacker) Pack(
	projects []database.Project,
	namespaces []database.Namespace,
	urgencyCalc *UrgencyCalculator,
	lastCompleted map[string]time.Time,
	running []string,
	now time.Time,
) PackResult {

	// --- Fallback: no namespaces → flat single-pool mode ---
	runningSet := make(map[string]bool, len(running))
	for _, name := range running {
		runningSet[name] = true
	}
	if len(namespaces) == 0 {
		packed := m.packFlat(projects, urgencyCalc, lastCompleted, runningSet, m.allocator.budget, 0, now)
		return PackResult{Projects: packed, NamespaceTicks: nil}
	}

	// Phase 1 — allocation (already implemented by NamespaceAllocator).
	allocations := m.allocator.Allocate(namespaces)

	globalRunning := len(runningSet)
	globalSelected := 0

	// Per-namespace concurrency caps (Bane 2026-08-27): nsCapMap[nsID] =
	// max_concurrent (0 = unlimited), nsRunningMap[nsID] = already-running
	// ticks of that namespace (name-keyed running set resolved via
	// NamespaceID). Used by Phase-2 (local copy) and Phase-3 re-pack.
	nsCapMap := make(map[string]int, len(namespaces))
	nsRunningMap := make(map[string]int, len(namespaces))
	for _, ns := range namespaces {
		cap := ns.MaxConcurrent
		if cap < 0 {
			cap = 0
		}
		nsCapMap[ns.ID] = cap
		nsRunningMap[ns.ID] = 0
	}
	for name := range runningSet {
		for i := range projects {
			p := &projects[i]
			if p.Name == name && p.NamespaceID != nil {
				nsRunningMap[*p.NamespaceID]++
				break
			}
		}
	}

	type nsPackState struct {
		ns         database.Namespace
		alloc      int
		selected   []*ProjectUrgency
		queued     []*ProjectUrgency
		usedBudget int
	}
	states := make(map[string]*nsPackState)

	// Namespaces that exist (enabled or not) — used to detect dangling refs.
	nsSet := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		nsSet[ns.ID] = true
	}

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
				// SCHED-GAP-066: budget-exhausted projects are never
				// packed. Checked AFTER the membership test so a blocked
				// project logs once per eval, not once per namespace.
				if m.budgetBlocked(p) != "" {
					continue
				}
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

		// Per-namespace concurrency cap (Bane 2026-08-27): a namespace with
		// max_concurrent > 0 may have at most that many ticks in flight.
		// Running counts were resolved once into nsRunningMap above (name-keyed
		// running set → NamespaceID); the per-cycle pack adds to the count as
		// it selects (len(st.selected)).
		nsRunning := nsRunningMap[ns.ID]
		nsCap := nsCapMap[ns.ID]

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
			// S-GAP-001 fairness: an eligible project whose last attempt is
			// older than its starvation window jumps the urgency queue so the
			// prio-10 cohort cannot starve it indefinitely. The boost is
			// monotonic in starvation age so the MOST-starved project sorts
			// first regardless of priority (reopen 2026-08-05).
			if isStarving(p.CooldownS, p.ConsecutiveFailures, lastTick, createdAt, now) && urgency < starvationBoostUrgency {
				age := starvationAge(lastTick, createdAt, now)
				urgency = starvationBoostUrgencyFor(age)
				log.Printf("FAIRNESS: %s boosted (cooldown=%ds failures=%d window=%v starved=%v) — starvation guarantee",
					p.Name, p.CooldownS, p.ConsecutiveFailures, StarvationWindow(p.CooldownS), age)
			}
			// SCHED-GAP-019: a project with pending board tasks gets a
			// urgency boost below the starvation tier but far above organic
			// urgency, so freshly-pending work jumps the eligible queue.
			// Cooldown is NOT bypassed — the boost lives in the scoring loop
			// only; the cooldown checks below remain the sole gate.
			if m.pendingCounter != nil {
				if pending := m.pendingCounter.CountPending(p.Workdir); pending > 0 && urgency < pendingBoostUrgency {
					urgency = pendingBoostUrgencyFor(pending)
				}
			}

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

			// Never re-pack a project whose tick is already in flight
			// (mirror of packFlat — prevents duplicate concurrent ticks).
			if runningSet[pu.Project.Name] {
				continue
			}

			// Cooldown check with blackout slowdown.
			if lt, ok := lastCompleted[pu.Project.Name]; ok {
				cooldownDur := time.Duration(pu.Project.CooldownS) * time.Second
				// S-GAP-001: consecutive spawn failures back off exponentially.
				if pu.Project.ConsecutiveFailures > 0 {
					cooldownDur = FailureBackoff(cooldownDur, pu.Project.ConsecutiveFailures)
				}
				// Apply blackout slowdown if inside a peak-pricing window.
				if mult, inBlackout := config.ActiveMultiplier(m.blackoutWindows, now); inBlackout {
					if mult <= 0 {
						continue // skip mode
					}
					if mult > 1.0 {
						cooldownDur = time.Duration(float64(cooldownDur) * mult)
					}
				}
				if now.Sub(lt) < cooldownDur {
					continue
				}
			}

			// Concurrency cap check (global across all namespaces).
			if globalRunning+globalSelected >= m.maxConcurrent {
				break
			}
			// Per-namespace concurrency cap (Bane 2026-08-27): 0 = unlimited.
			if nsCap > 0 && nsRunning+len(st.selected) >= nsCap {
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
			// Running projects must not be queued either (no re-pack by borrowing).
			if runningSet[pu.Project.Name] {
				continue
			}
			if !puInList(pu, st.selected) && !puInList(pu, st.queued) {
				// Check if it was skipped by cooldown — those are NOT queued.
				if lt, ok := lastCompleted[pu.Project.Name]; ok {
					cooldownDur := time.Duration(pu.Project.CooldownS) * time.Second
					// S-GAP-001: consecutive spawn failures back off exponentially.
					if pu.Project.ConsecutiveFailures > 0 {
						cooldownDur = FailureBackoff(cooldownDur, pu.Project.ConsecutiveFailures)
					}
					// Apply blackout slowdown if inside a peak-pricing window.
					if mult, inBlackout := config.ActiveMultiplier(m.blackoutWindows, now); inBlackout {
						if mult <= 0 {
							continue // skip mode — not queued
						}
						if mult > 1.0 {
							cooldownDur = time.Duration(float64(cooldownDur) * mult)
						}
					}
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
			// Running projects must not be re-packed with borrowed budget.
			if runningSet[pu.Project.Name] {
				continue
			}
			// Recalculate effective weight with the new (larger) allocation.
			effW := CalcEffectiveWeight(pu.Project.Weight, totalWeightInNS+sumSelectedWeights(st.selected), newAlloc)
			pu.EffectiveWeight = effW

			if globalRunning+globalSelected >= m.maxConcurrent {
				stillQueued = append(stillQueued, pu)
				continue
			}
			// Per-namespace cap in the borrow re-pack (Bane 2026-08-27):
			// borrowed budget must not exceed the namespace's concurrency cap.
			if cap := nsCapMap[id]; cap > 0 && nsRunningMap[id]+len(st.selected) >= cap {
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
				Name:             pu.Project.Name,
				Priority:         float64(pu.Project.Priority),
				Weight:           pu.EffectiveWeight,
				Urgency:          pu.Urgency,
				Workdir:          pu.Project.Workdir,
				RepoURL:          pu.Project.RepoURL,
				Command:          pu.Project.Command,
				Model:            pu.Project.Model,
				Provider:         pu.Project.Provider,
				FallbackModel:    pu.Project.FallbackModel,
				FallbackProvider: pu.Project.FallbackProvider,
				NoGlobalFallback: pu.Project.NoGlobalFallback,
				ModelChain:       pu.Project.ModelChain,
				IdleModel:        pu.Project.IdleModel,
				IdleProvider:     pu.Project.IdleProvider,
				WorkerModel:      pu.Project.WorkerModel,
				WorkerProvider:   pu.Project.WorkerProvider,
				GatewayKey:       pu.Project.GatewayKey,
				Deliver:          pu.Project.Deliver,
				Prompt:           pu.Project.Prompt,
				PromptMode:       pu.Project.PromptMode,
				NamespacePrompt:  ns.DefaultPrompt,
				NamespaceChain:   ns.ModelChain,
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

	// --- Phase 4 — unassigned projects must NEVER be silently dropped ---
	// Projects with nil NamespaceID or a namespace ID that doesn't exist (or
	// is disabled) fall through the per-namespace filter above. Pack them flat
	// with whatever budget remains, and log loudly when it happens.
	selectedNames := make(map[string]bool, len(result.Projects))
	for _, p := range result.Projects {
		selectedNames[p.Name] = true
	}
	var unassigned []database.Project
	for i := range projects {
		p := &projects[i]
		if !p.Enabled {
			continue
		}
		// SCHED-GAP-066: budget-exhausted projects are never packed.
		if m.budgetBlocked(p) != "" {
			continue
		}
		if selectedNames[p.Name] {
			continue
		}
		if p.NamespaceID == nil || !nsSet[*p.NamespaceID] {
			unassigned = append(unassigned, *p)
		}
	}
	if len(unassigned) > 0 {
		// Budget remaining = global budget minus what namespaces consumed.
		usedGlobal := 0
		for _, st := range states {
			usedGlobal += st.usedBudget
		}
		remainingBudget := m.allocator.budget - usedGlobal
		if remainingBudget < 0 {
			remainingBudget = 0
		}
		flat := m.packFlat(unassigned, urgencyCalc, lastCompleted, runningSet,
			remainingBudget, globalSelected, now)
		result.Projects = append(result.Projects, flat...)
		if len(flat) < len(unassigned) {
			var dropped []string
			picked := make(map[string]bool, len(flat))
			for _, p := range flat {
				picked[p.Name] = true
			}
			for _, p := range unassigned {
				if !picked[p.Name] {
					dropped = append(dropped, p.Name)
				}
			}
			log.Printf("NS-UNASSIGNED: %d project(s) with nil/unknown namespace, "+
				"packed %d, DROPPED %v (budget/concurrency exhausted) — "+
				"assign them to a namespace or raise budget", len(unassigned), len(flat), dropped)
		} else {
			log.Printf("NS-UNASSIGNED: %d project(s) with nil/unknown namespace "+
				"flat-packed into result: %v", len(unassigned), flatNames(flat))
		}
	}

	return result
}

func flatNames(ps []PackedProject) []string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	return names
}
