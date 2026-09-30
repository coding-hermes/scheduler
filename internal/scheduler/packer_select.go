package scheduler

import (
	"log"
	"sort"
	"time"

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
	// SCHED-GAP-113 (S12 §6.2 admission layer): waveShed[nsID] = the
	// namespace has wave_workers_cap > 0 AND a live wave (running tick
	// with worker_count > 0) in flight. Selected projects there run
	// SERIAL (WaveSerial → WAVE_BUDGET: 0 at spawn); the packer may still
	// select them — this is a coarse tick-boundary shed, not a block.
	waveShed := m.waveShedSet(namespaces)
	// SCHED-GAP-124: per-namespace admission mode map (namespace default;
	// per-project overrides resolve in admissionModeFor).
	nsModes := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		nsModes[ns.ID] = ns.AdmissionMode
	}
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
		// demand (SCHED-GAP-1582): the sum of ENABLED project weights the
		// namespace carried into this pack — the raw demand the allocation
		// is measured against for the oversubscription verdict.
		demand int
		// SCHED-GAP-1614: roster is the held-eligible member names this
		// pass (the dedupe-signature basis); rotatedNames is what BeginPass
		// promoted to the front (rotation provenance, canonical order).
		// Both are read back when the holds are built after borrowing.
		roster       []string
		rotatedNames []string
	}
	states := make(map[string]*nsPackState)

	// SCHED-GAP-1582: budget holds — one entry per namespace whose enabled
	// demand exceeded its FINAL (post-borrowing) allocation. Collected just
	// before the result is built; recorded by the CALLER through the
	// SCHED-GAP-157 deferrals path — the packer never writes DB rows itself.
	var budgetHolds []nsHold

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

		// SCHED-GAP-1614 fairness rotation: ask this namespace's hold state
		// which previously-held lanes lead this pass's candidate order. nil
		// result = no prior hold (or none eligible today) — order untouched.
		// The ring advances in EndPass below, once per held pass.
		var rotate []string
		rotatedPos := map[string]int{}

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
		// SCHED-GAP-1614: this namespace's held-eligible roster — every
		// enabled, budget-gate-passed member that entered the scoring loop.
		// It is the basis of the hold-event dedupe signature (rotation
		// varies the held subset every pass; the roster is what actually
		// changed when the situation changed) and of the escalator's
		// "held by budget" membership verdict.
		var members []string
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
			// SCHED-GAP-107: active bump — the bump cooldown overrides the
			// stored value in the cooldown gates below, and the project gets
			// the bump urgency tier (same class as the pending-work boost).
			bumpCD := 0
			if p.BumpActive && p.BumpCooldownS > 0 {
				bumpCD = p.BumpCooldownS
				if urgency < bumpBoostUrgency {
					urgency = bumpBoostUrgency
				}
			}

			urgency = cadenceAdjustedUrgency(urgency, p, m.cadenceRates[p.Name])

			effW := CalcEffectiveWeight(p.Weight, totalWeightInNS, alloc)
			scored = append(scored, ProjectUrgency{
				Project:         p,
				Urgency:         urgency,
				EffectiveWeight: effW,
				BumpCooldownS:   bumpCD,
			})
			members = append(members, p.Name)
		}

		// SCHED-GAP-1614: promote the previously-held lanes now that the
		// roster is known (the BeginPass answer is scoped to the members
		// that entered the scoring loop).
		rotate = m.holdStateFor(ns.ID).BeginPass(members)
		for i, name := range rotate {
			rotatedPos[name] = i
		}

		// Sort by urgency descending, then priority, then last-tick ASC.
		// SCHED-GAP-1614: when rotation is active, the promotion is the
		// PRIMARY key of this ONE sort — promoted lanes (in ring order)
		// lead, and the urgency cascade applies within each group. The
		// promotion cannot be a separate pre-sort: a following full
		// urgency sort would move every promoted lane straight back to
		// its urgency position (measured: pass 2 re-held the identical
		// set), which is exactly the defect this row closes.
		sort.SliceStable(scored, func(i, j int) bool {
			pi, iok := rotatedPos[scored[i].Project.Name]
			pj, jok := rotatedPos[scored[j].Project.Name]
			if iok && jok && pi != pj {
				return pi < pj // promoted: ring order
			}
			if iok != jok {
				return iok // promoted before non-promoted
			}
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
		// SCHED-GAP-1614: carry the roster and the rotation promotion into the
		// state so the hold build (after borrowing) can stamp them on the hold.
		st.roster = members
		st.rotatedNames = rotate
		budgetRemaining := alloc
		for i := range scored {
			pu := &scored[i]

			// Never re-pack a project whose tick is already in flight
			// (mirror of packFlat — prevents duplicate concurrent ticks).
			if runningSet[pu.Project.Name] {
				continue
			}

			// Cooldown check (ADV-R03/G5: shared effectiveCooldown —
			// the bump value feeds cooldownS, so the bump, the
			// S-GAP-001 failure backoff, and the blackout multiplier
			// all apply in the one shared place; SCHED-GAP-1661: a
			// set+positive cooldown_pin_s outranks the (bumped)
			// cooldown base inside that shared predicate).
			if lt, ok := lastCompleted[pu.Project.Name]; ok {
				cd := pu.Project.CooldownS
				// SCHED-GAP-107: an active bump owns the effective cooldown.
				if pu.BumpCooldownS > 0 {
					cd = pu.BumpCooldownS
				}
				cooldownDur, skipMode := effectiveCooldown(cd, float64(pu.Project.Priority), pu.Project.ConsecutiveFailures, m.blackoutWindows, now, urgencyCalc, pu.Project.CooldownPinS)
				if skipMode {
					continue // skip mode
				}
				// SCHED-GAP-124: tasks-mode admission — non-perpetual
				// pending board work waives the last-tick spacing,
				// but FailureBackoff still gates (SCHED-GAP-133).
				mode := admissionModeFor(pu.Project.AdmissionMode, nsIDOf(pu.Project), nsModes)
				if mode == database.AdmissionModeTasks && tasksAdmissionDue(pu.Project.Workdir, pu.Project.BoardOwnership) {
					// SCHED-GAP-214: after a FAILED tick the waiver stands
					// down — the lane paces on its full effective cooldown
					// (the shared predicate above), so a gateway outage
					// samples the lane once per cooldown instead of once
					// per eval. Every other status keeps the original
					// waiver semantics (SCHED-GAP-124 unchanged).
					if lastTickStatusFailed(pu.Project.LastTickStatus) {
						if now.Sub(lt) < cooldownDur {
							continue // post-failure cooldown not elapsed
						}
					}
					// SCHED-GAP-133: only gate on FailureBackoff when the project
					// has actually failed repeatedly.
					if pu.Project.ConsecutiveFailures > 1 {
						if now.Sub(lt) < cooldownDur {
							continue // FailureBackoff not elapsed
						}
					}
					// SCHED-GAP-136: post-tick pacing floor — even a healthy
					// tasks-mode project waits pacing+jitter after ANY terminal
					// tick before re-admission (anti-herd spacing, Bane's
					// "don't spawn at 0ms" ruling).
					if tasksPacingDeferredJittered(&lt, now) {
						continue
					}
				} else {
					// SCHED-GAP-1655: a COOLDOWN-mode BUILDER lane whose
					// board holds no dispatchable row is deferred BEFORE
					// its pin is even consulted — the measured 261
					// zero-tool sessions/week this gate closes. The lane
					// keeps its selection (defer, not drop: no cooldown
					// consumed, no row, no session) and the next
					// evaluation re-checks the board. Reporter-class
					// lanes and tasks lanes are untouched (the deliverable
					// 3 exemption; the tasks branch above).
					//
					// Gate ordering note: the cooldown check below still
					// runs for gate-transparent lanes. For a gated lane
					// the board decision is the FIRST answer — "no work"
					// is true even when the pin has elapsed, so the lane
					// must not fall through to the pack. Logging is at
					// the same site as the decision (one board walk).
					if mode == database.AdmissionModeCooldown &&
						builderAdmissionBlocked(pu.Project.Name, pu.Project.Workdir, database.AdmissionModeCooldown, "") {
						noteBuilderNoWorkDeferral(pu.Project.Name, pu.Project.Workdir)
						continue
					}
					if now.Sub(lt) < cooldownDur {
						continue
					}
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
					cd := pu.Project.CooldownS
					// SCHED-GAP-107: an active bump owns the effective cooldown.
					if pu.BumpCooldownS > 0 {
						cd = pu.BumpCooldownS
					}
					// ADV-R03/G5: same shared predicate as the selection
					// gate above — a project the packer would skip must
					// not be queued either. SCHED-GAP-1661: the pin rides
					// in (pu.Project.CooldownPinS) so the queued check
					// stays identical to the selection gate.
					cooldownDur, skipMode := effectiveCooldown(cd, float64(pu.Project.Priority), pu.Project.ConsecutiveFailures, m.blackoutWindows, now, urgencyCalc, pu.Project.CooldownPinS)
					if skipMode {
						continue // skip mode — not queued
					}
					// SCHED-GAP-1655: a cooldown-mode BUILDER lane the
					// selection gate deferred on its empty board is not
					// queued for borrowing either — borrowed budget must
					// not re-admit a lane the board said has no work.
					// Same conjunction as the selection gate: the tasks
					// branch above never queues here untouched, and a
					// reporter-class lane is exempt by the gate itself.
					if effectiveAdmissionModeFor(pu.Project, nsModes) == database.AdmissionModeCooldown &&
						builderAdmissionBlocked(pu.Project.Name, pu.Project.Workdir, database.AdmissionModeCooldown, "") {
						continue // no-work skip — not queued
					}
					if now.Sub(lt) < cooldownDur {
						continue // cooldown-skip, not queued
					}
				}
				st.queued = append(st.queued, pu)
			}
		}

		st.usedBudget = alloc - budgetRemaining
		st.demand = 0
		for _, p := range nsProjects {
			st.demand += p.Weight
		}
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

	// SCHED-GAP-1582: the oversubscription verdict is taken HERE — after
	// Phase-3 borrowing — because a namespace that borrowed enough budget
	// to place its queued work is NOT over-committed; only the surplus
	// still queued against the FINAL allocation is held.
	//
	// SCHED-GAP-1614: each hold feeds its namespace's hold state — the
	// rotation ring advances by the held set (EndPass, one step per held
	// pass) and the dedupe memory records the hold (NoteHold), whose
	// verdict rides the hold as Emit so the caller emits the HIGH event on
	// STATE CHANGE only. Rotation provenance (which promoted lanes are in
	// this hold) is stamped for the log/deferral/event surfaces. A
	// namespace seen NOT held is noted so a later re-hold is a fresh
	// enter (re-emit).
	for id, st := range states {
		h := newNsHold(id, st.demand, newAllocations[id], st.queued, st.roster...)
		if h.over() <= 0 {
			m.holdStateFor(id).NoteNoHold()
			continue
		}
		var rotatedHeld []string
		if len(st.rotatedNames) > 0 {
			heldSet := make(map[string]bool, len(h.held))
			for _, pu := range h.held {
				heldSet[pu.Project.Name] = true
			}
			for _, n := range st.rotatedNames {
				if heldSet[n] {
					rotatedHeld = append(rotatedHeld, n)
				}
			}
		}
		h.rotated = len(rotatedHeld) > 0
		h.rotatedNames = rotatedHeld
		hs := m.holdStateFor(id)
		hs.EndPass(h.heldNames())
		h.emit = hs.NoteHold(h)
		budgetHolds = append(budgetHolds, h)
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
				DeliverMode:      pu.Project.DeliverMode,
				Prompt:           pu.Project.Prompt,
				PromptMode:       pu.Project.PromptMode,
				NamespacePrompt:  ns.DefaultPrompt,
				NamespaceChain:   ns.ModelChain,
				// SCHED-GAP-111: thread the namespace id for
				// effectiveTickTimeout in the spawn path.
				NamespaceID: ns.ID,
				// SCHED-GAP-113: a namespace in wave-shed still gets its
				// projects packed — serially (WAVE_BUDGET: 0 at spawn).
				WaveSerial: waveShed[ns.ID],
			})
		}
		result.NamespaceTicks = append(result.NamespaceTicks, NamespaceTickData{
			NamespaceID: ns.ID,
			Allocated:   newAllocations[ns.ID],
			Used:        st.usedBudget,
			Borrowed:    borrowMap[ns.ID],
			Lent:        lentMap[ns.ID],
			JobCount:    len(st.selected),
			// SCHED-GAP-1582: demand vs final allocation — the two
			// columns that make an oversubscribed namespace explicit
			// on the utilization history.
			Demand:        st.demand,
			Overcommitted: oversubscription(st.demand, newAllocations[ns.ID]),
		})
	}
	result.BudgetHolds = budgetHolds

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
