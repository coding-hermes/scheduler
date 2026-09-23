package scheduler

import (
	"database/sql"
	"log"
	"sort"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// PackedProject is a project selected to run in this tick.
type PackedProject struct {
	Name             string
	Priority         float64
	Weight           int
	Urgency          float64
	Workdir          string
	RepoURL          string
	Command          string // optional: custom spawn command (overrides default hermes chat)
	Model            string // LLM model for this project (empty = use spawner default)
	Provider         string // LLM provider for this project (empty = use spawner default)
	FallbackModel    string // optional: fallback model tier for the spawn chain (SCHED-GAP-064)
	FallbackProvider string // optional: fallback provider tier for the spawn chain (SCHED-GAP-064)
	NoGlobalFallback bool   // true → skip the spawner-level (env) fallback tier (SCHED-GAP-064)
	ModelChain       string // ordered list of "model@provider" hops (JSON array); empty = use model/provider + fallback fields (SCHED-GAP-075)
	IdleModel        string // optional: idle-tick model tier, prepended to the spawn chain on zero-pending boards (SCHED-GAP-065)
	IdleProvider     string // optional: idle-tick provider tier (SCHED-GAP-065)
	WorkerModel      string // optional: suggested worker model (foreman can override)
	WorkerProvider   string // optional: suggested worker provider (foreman can override)
	GatewayKey       string // per-foreman Hermes gateway key (empty = shared --gateway-key)
	Deliver          string // delivery target (telegram:chat_id:thread_id)
	Prompt           string // Bane 2026-08-27: per-project extra foreman prompt (append or replace per PromptMode)
	PromptMode       string // "append" (default) | "replace"
	NamespacePrompt  string // namespace default_prompt (empty = built-in prompt)
	NamespaceChain   string // namespace model_chain (JSON array); tier between project chain and router (Bane 2026-08-27)
	// NamespaceID (SCHED-GAP-111): the project's namespace, threaded from
	// the packer's namespace join. Spawn() uses it to resolve the effective
	// tick deadline for wave-enabled namespaces (S12 §4.3). Empty = no
	// namespace → base --tick-timeout, no lookup (byte-identical serial path).
	NamespaceID string
	// WaveSerial (SCHED-GAP-113, S12 §6.2 admission layer): set by the
	// packer when the project's namespace has wave_workers_cap > 0 AND a
	// live wave (running tick with worker_count > 0) was in flight at pack
	// time — the namespace is in wave-shed, so this tick must run SERIAL.
	// Spawn()'s WAVE_BUDGET resolution treats it as authoritative: budget 0,
	// no re-read of live depth. It carries NO slot semantics (W1/W3): the
	// tick still occupies exactly one slot / RunningSet entry.
	WaveSerial bool
}

// Packer selects which projects run given a weight budget and running set.
type Packer struct {
	db              *sql.DB
	calculator      *UrgencyCalculator
	budget          int
	maxConcurrent   int
	blackoutWindows []config.BlackoutWindow
	pendingCounter  *PendingTaskCounter
	// budgetGate, when non-nil, excludes budget-exhausted projects from
	// selection (SCHED-GAP-066). Installed per evaluation cycle by the loop;
	// nil = no budget enforcement (tests, spend-query failure fail-open).
	budgetGate BudgetGate
}

// NewPacker creates a packer with the given budget and concurrency cap. The
// pending-task counter defaults to the package-level shared instance so
// existing call sites keep working unchanged.
func NewPacker(db *sql.DB, calc *UrgencyCalculator, budget, maxConcurrent int, blackoutWindows []config.BlackoutWindow) *Packer {
	return &Packer{
		db:              db,
		calculator:      calc,
		budget:          budget,
		maxConcurrent:   maxConcurrent,
		blackoutWindows: blackoutWindows,
		pendingCounter:  defaultPendingCounter,
	}
}

// SetPendingCounter overrides the pending-task counter (for tests).
func (p *Packer) SetPendingCounter(c *PendingTaskCounter) {
	p.pendingCounter = c
}

// SetBudgetGate installs the per-cycle budget gate (SCHED-GAP-066). Pass nil
// to disable budget enforcement.
func (p *Packer) SetBudgetGate(g BudgetGate) {
	p.budgetGate = g
}

// scored is a project with its computed urgency.
type scored struct {
	name                string
	priority            float64
	weight              int
	urgency             float64
	decayRate           float64
	cooldownS           int
	consecutiveFailures int
	// SCHED-GAP-214: projects.last_tick_status — the terminal status of the
	// most recent tick ("" = never ticked | completed | failed | timeout |
	// deferred). True-prefixed helper on the struct: after a FAILED tick the
	// tasks-mode waiver stands down (see the Pick gate below).
	lastTickStatusFailed bool
	budgetBlocked        bool // SCHED-GAP-066 spend gate excluded this project from the greedy pack (GAP-011 overdue force-select may still pick it)
	lastTickAt           *time.Time
	createdAt            time.Time
	workdir              string
	repoURL              string
	command              string
	model                string
	provider             string
	fallbackModel        string
	fallbackProvider     string
	noGlobalFallback     bool
	modelChain           string
	idleModel            string
	idleProvider         string
	dailyBudgetUSD       float64
	weeklyBudgetUSD      float64
	finalBudgetUSD       float64
	workerModel          string
	workerProvider       string
	gatewayKey           string
	deliver              string
	prompt               string // Bane 2026-08-27: per-project extra foreman prompt
	promptMode           string // "append" (default) | "replace"
	bumpActive           bool   // SCHED-GAP-107: bump owns the effective cooldown + gets an urgency boost
	bumpCooldownS        int
	bumpRemaining        int
	namespaceDefaultPmt  string // namespace default_prompt (empty = built-in)
	namespaceID          string // namespace_id (empty = no namespace)
	namespaceMaxConc     int    // namespace max_concurrent; 0 = unlimited (Bane 2026-08-27)
	namespaceChain       string // namespace model_chain (JSON array string) (Bane 2026-08-27)
	admissionNsMode      string // SCHED-GAP-124: namespace admission_mode ('' = cooldown)
	admissionMode        string // SCHED-GAP-124: project admission_mode override ('' = inherit)
	boardOwnership       string // SCHED-GAP-141: project board_ownership override ('' = auto/derived)
}

// Pick returns the selected projects for this tick, sorted by urgency desc.
func (p *Packer) Pick(now time.Time, spawnerRunning map[string]bool) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT p.name, p.weight, p.priority, p.decay_rate, p.enabled, p.cooldown_s,
		       p.last_tick_completed,
		       p.created_at, p.workdir, p.repo_url, COALESCE(p.command, ''),
		       COALESCE(p.model, ''), COALESCE(p.provider, ''), COALESCE(p.fallback_model, ''), COALESCE(p.fallback_provider, ''), COALESCE(p.no_global_fallback, 0), COALESCE(p.model_chain, ''), COALESCE(p.idle_model, ''), COALESCE(p.idle_provider, ''), COALESCE(p.daily_budget_usd, 0.0), COALESCE(p.weekly_budget_usd, 0.0), COALESCE(p.final_budget_usd, 0.0), COALESCE(p.worker_model, ''), COALESCE(p.worker_provider, ''), COALESCE(p.gateway_key, ''), COALESCE(p.deliver, ''),
		       COALESCE(p.prompt, ''), COALESCE(p.prompt_mode, 'append'), COALESCE(ns.default_prompt, ''), COALESCE(ns.id, ''), COALESCE(ns.max_concurrent, 0), COALESCE(ns.model_chain, ''),
		       COALESCE(p.bump_active, 0), COALESCE(p.bump_cooldown_s, 0), COALESCE(p.bump_remaining_ticks, 0),
		       p.consecutive_failures, COALESCE(p.last_tick_status, ''),
		       COALESCE(ns.admission_mode, ''), COALESCE(p.admission_mode, ''), COALESCE(p.board_ownership, '')
		FROM projects p
		LEFT JOIN namespaces ns ON ns.id = p.namespace_id
		WHERE p.enabled = 1
		ORDER BY p.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []scored
	skippedBudgetCap := 0

	for rows.Next() {
		var s scored
		var lastCompleted *time.Time
		var lastStr sql.NullString
		var createdAtStr string
		var enabled bool
		var lastStatus string
		if err := rows.Scan(&s.name, &s.weight, &s.priority, &s.decayRate, &enabled, &s.cooldownS,
			&lastStr, &createdAtStr, &s.workdir, &s.repoURL, &s.command,
			&s.model, &s.provider, &s.fallbackModel, &s.fallbackProvider, &s.noGlobalFallback, &s.modelChain, &s.idleModel, &s.idleProvider, &s.dailyBudgetUSD, &s.weeklyBudgetUSD, &s.finalBudgetUSD, &s.workerModel, &s.workerProvider, &s.gatewayKey, &s.deliver,
			&s.prompt, &s.promptMode, &s.namespaceDefaultPmt, &s.namespaceID, &s.namespaceMaxConc, &s.namespaceChain,
			&s.bumpActive, &s.bumpCooldownS, &s.bumpRemaining,
			&s.consecutiveFailures, &lastStatus,
			&s.admissionNsMode, &s.admissionMode, &s.boardOwnership); err != nil {
			log.Printf("ERROR scanning project row: %v", err)
			continue
		}
		// SCHED-GAP-214: after a FAILED tick the tasks-mode waiver stands down.
		s.lastTickStatusFailed = lastTickStatusFailed(lastStatus)
		// SCHED-GAP-066: budget-exhausted projects are excluded from the
		// greedy pack — but kept in the candidate list, flagged, so the
		// GAP-011 overdue force-select below can still pick them (a spend
		// gate must not starve an overdue project). The gate filters NEW
		// spawns only — a running tick is never touched.
		if p.budgetGate != nil {
			if detail, blocked := p.budgetGate(s.name, s.dailyBudgetUSD, s.weeklyBudgetUSD, s.finalBudgetUSD); blocked {
				s.budgetBlocked = true
				skippedBudgetCap++
				log.Printf("BUDGET: %s blocked (%s) — excluded from greedy selection; running ticks untouched", s.name, detail)
			}
		}
		s.createdAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if lastStr.Valid && lastStr.String != "" {
			t, err := time.Parse(time.RFC3339, lastStr.String)
			if err == nil {
				lastCompleted = &t
			}
		}
		// SCHED-GAP-107: an active bump overrides the stored cooldown for
		// selection AND boosts urgency into the pending-work tier — a
		// bumped project must outrank every ordinary eligible project so
		// its N fast ticks actually run. Cooldown mechanics (backoff,
		// blackout) still apply on top of the bump value.
		if s.bumpActive && s.bumpCooldownS > 0 {
			s.cooldownS = s.bumpCooldownS
		}
		s.urgency = p.calculator.ComputeUrgency(s.priority, s.decayRate, now, lastCompleted, s.createdAt)
		// S-GAP-001 fairness: starvation boost in the flat path too, or the
		// two selection paths would diverge. Monotonic in starvation age so
		// the most-starved project sorts first regardless of priority.
		if isStarving(s.cooldownS, s.consecutiveFailures, lastCompleted, s.createdAt, now) && s.urgency < starvationBoostUrgency {
			age := starvationAge(lastCompleted, s.createdAt, now)
			s.urgency = starvationBoostUrgencyFor(age)
			log.Printf("FAIRNESS: %s boosted in flat packer (cooldown=%ds failures=%d window=%v starved=%v)",
				s.name, s.cooldownS, s.consecutiveFailures, StarvationWindow(s.cooldownS), age)
		}
		// SCHED-GAP-019: board-aware pending-task boost — same tier as the
		// namespace and flat-fallback paths, so all three selection paths
		// stay in sync. Cooldown is NOT bypassed.
		if p.pendingCounter != nil {
			if pending := p.pendingCounter.CountPending(s.workdir); pending > 0 && s.urgency < pendingBoostUrgency {
				s.urgency = pendingBoostUrgencyFor(pending)
			}
		}
		// SCHED-GAP-107: bump urgency boost — same class as the pending-work
		// boost (above organic, below starvation), applied after it so an
		// active bump always carries the boost even on a quiet board.
		if s.bumpActive && s.urgency < bumpBoostUrgency {
			s.urgency = bumpBoostUrgency
		}
		s.lastTickAt = lastCompleted
		list = append(list, s)
	}

	// Sort by urgency descending, priority descending, then last tick
	// ascending (oldest first — projects that haven't run in longest get
	// priority over projects with the same urgency/priority).
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].urgency != list[j].urgency {
			return list[i].urgency > list[j].urgency
		}
		if list[i].priority != list[j].priority {
			return list[i].priority > list[j].priority
		}
		// Older last-tick = higher priority.
		// nil lastTickAt means never completed — those come first.
		if list[i].lastTickAt == nil && list[j].lastTickAt != nil {
			return true
		}
		if list[j].lastTickAt == nil && list[i].lastTickAt != nil {
			return false
		}
		if list[i].lastTickAt != nil && list[j].lastTickAt != nil {
			return list[i].lastTickAt.Before(*list[j].lastTickAt)
		}
		return list[i].name < list[j].name
	})

	// DEBUG: log top 15 sorted projects
	for i := 0; i < min(15, len(list)); i++ {
		s := list[i]
		lt := "nil"
		if s.lastTickAt != nil {
			lt = s.lastTickAt.Format("15:04")
		}
		log.Printf("PACKER-SORTED[%d]: %s urgency=%.1f pri=%.0f last=%s",
			i, s.name, s.urgency, s.priority, lt)
	}

	// Greedy pack: pick projects that fit in budget.
	// Use the in-memory running set as the SOLE source of truth.
	// DB queries race against goroutines writing to SQLite — the
	// memory-backed SlotPool semaphore is always correct.
	currRunning := len(spawnerRunning)
	used := 0
	packed := make([]PackedProject, 0, max(1, len(list)/2))

	// Per-namespace running counts (Bane 2026-08-27): a namespace with
	// max_concurrent > 0 may have at most that many ticks running at
	// once. Count the already-running projects per namespace, then
	// increment as we pack so one cycle never overshoots the cap.
	nsRunning := make(map[string]int)
	for _, s := range list {
		if spawnerRunning[s.name] {
			nsRunning[s.namespaceID]++
		}
	}

	totalChecked := 0
	totalSkippedBudget := 0
	totalSkippedCooldown := 0
	totalSkippedRunning := 0
	totalSkippedNamespace := 0

	for _, s := range list {
		totalChecked++
		if spawnerRunning[s.name] {
			totalSkippedRunning++
			continue
		}
		// Per-namespace concurrency cap: skip when the namespace already
		// has max_concurrent running ticks (including ones packed earlier
		// in this same cycle). 0 = unlimited — only the global cap applies.
		if s.namespaceMaxConc > 0 && nsRunning[s.namespaceID] >= s.namespaceMaxConc {
			totalSkippedNamespace++
			continue
		}
		// GAP-011: hard due-aware selection. Any enabled, not-running
		// project whose time since last completed tick is STRICTLY greater
		// than 2x its effective cooldown MUST be selected — force-select it
		// before greedy budget packing so neither the weight budget nor the
		// SCHED-GAP-066 spend gate can exclude it. Forced selections do NOT
		// consume the weight budget (the gate's jurisdiction is the greedy
		// pack below) but DO occupy concurrency slots.
		if p.isOverdue(s, now) {
			if currRunning >= p.maxConcurrent {
				log.Printf("PACKER: max concurrency reached (%d), stopping", p.maxConcurrent)
				break
			}
			log.Printf("OVERDUE: %s force-selected (age=%v past 2x cooldown) — bypassing budget gate",
				s.name, starvationAge(s.lastTickAt, s.createdAt, now))
			packed = append(packed, s.packed())
			currRunning++
			nsRunning[s.namespaceID]++
			continue
		}
		// SCHED-GAP-066: budget-exhausted projects are never picked by the
		// greedy pack (the GAP-011 overdue pass above is the only path that
		// may select them).
		if s.budgetBlocked {
			totalSkippedBudget++
			continue
		}
		if used+s.weight > p.budget {
			totalSkippedBudget++
			continue
		}
		if currRunning >= p.maxConcurrent {
			log.Printf("PACKER: max concurrency reached (%d), stopping", p.maxConcurrent)
			break
		}
		cooldownDur, skipMode := p.effectiveCooldownDur(s, now)
		if skipMode {
			totalSkippedCooldown++
			continue // skip mode — don't spawn at all
		}
		// SCHED-GAP-124: tasks-mode admission (legacy non-namespace path).
		// Non-perpetual pending board work waives last-tick spacing;
		// backoff/blackout/skip above still applied.
		mode := admissionModeFor(s.admissionMode, s.namespaceID, map[string]string{"": s.admissionNsMode})
		if mode == database.AdmissionModeTasks && tasksAdmissionDue(s.workdir, s.boardOwnership) {
			// SCHED-GAP-214: after a FAILED tick the waiver stands down —
			// the lane paces on its full effective cooldown (the same
			// arithmetic cooldown mode applies), so a gateway outage
			// samples the lane once per cooldown instead of once per
			// eval (the measured 9-second retry storm). Every other
			// status keeps the original waiver semantics.
			if s.lastTickStatusFailed && s.lastTickAt != nil {
				if now.Sub(*s.lastTickAt) < cooldownDur {
					totalSkippedCooldown++
					continue
				}
			}
			// SCHED-GAP-133: tasks-mode admission still respects FailureBackoff.
			// Without this, a project that has been failing repeatedly (e.g.
			// gateway draining) re-admits instantly on every eval — the
			// ~5/min/lane hot-loop. Normal operation (consecutive_failures ≤ 1)
			// keeps the original tasks-mode semantics: pending work waives
			// the cooldown pin entirely.
			if s.consecutiveFailures > 1 {
				backoffCD, skipMode := effectiveCooldown(s.cooldownS, s.priority, s.consecutiveFailures, p.blackoutWindows, now, p.calculator)
				if skipMode {
					totalSkippedCooldown++
					continue
				}
				if s.lastTickAt != nil && now.Sub(*s.lastTickAt) < backoffCD {
					totalSkippedCooldown++
					continue
				}
			}
			// SCHED-GAP-136: post-tick pacing floor (see packer_select.go).
			if tasksPacingDeferredJittered(s.lastTickAt, now) {
				totalSkippedCooldown++
				continue
			}
		} else {
			if s.lastTickAt != nil && now.Sub(*s.lastTickAt) < cooldownDur {
				totalSkippedCooldown++
				continue
			}
		}
		packed = append(packed, s.packed())
		used += s.weight
		currRunning++
		nsRunning[s.namespaceID]++
	}

	if len(packed) == 0 {
		log.Printf("PACKER: nothing packed — checked %d projects, skipped budget=%d cooldown=%d already-running=%d budget-cap=%d namespace-cap=%d, total-running=%d/%d",
			totalChecked, totalSkippedBudget, totalSkippedCooldown, totalSkippedRunning, skippedBudgetCap, totalSkippedNamespace, currRunning, p.maxConcurrent)
	}
	return packed, nil
}

// effectiveCooldown is the SINGLE SOURCE of TRUTH (ADV-R03 / G5) for the
// composed-cooldown arithmetic every eligibility site must agree on:
//
//	cooldownDur = cooldownS seconds, OR the priority-derived dynamic
//	              interval via calc.ComputeInterval when cooldownS == 0
//	if consecutiveFailures > 0: cooldownDur = FailureBackoff(...)
//	if inBlackout: mult <= 0 → skipMode (never eligible); mult > 1.0 →
//	              cooldownDur *= mult
//
// skipMode = true means the caller must NEVER count the project as
// eligible. Consumers: packer.go's greedy pack and isOverdue (via the
// *Packer.effectiveCooldownDur wrapper), packer_select.go's two
// namespace-path gates, multipool_packer.go's packFlat, and loop.go's
// countEligibleProjects watchdog mirror — the GAP-050 drift site this
// consolidation exists to kill.
//
// priority is float64 because the dynamic-interval path feeds it straight
// into calc.ComputeInterval(priority float64); the integer-priority call
// sites convert losslessly with float64(...).
//
// ⚠️ INTENTIONAL SEMANTIC ALIGNMENT (cooldown_s == 0): the dynamic-interval
// branch is part of this authority BY DESIGN — the G5 filing names it among
// the terms to single-source. Before this consolidation only packer.go's
// method carried it; packer_select.go's two gates, multipool_packer.go's
// packFlat and loop.go's watchdog went straight from cooldownS*second to the
// failure backoff, so a cooldown_s==0 project was "always eligible" there
// while the greedy pack treated it as a full priority interval (the same
// packer-vs-watchdog disagreement GAP-050 produced). All consumers now apply
// the identical branch, which is a deliberate behavior change at those four
// sites rather than a no-op refactor: cooldown_s==0 + a recent completion is
// no longer instantly eligible anywhere. No live project uses cooldown_s==0
// (0/256 at the time of the change), and the semantics are pinned by
// TestEligibilityEquivalence_BumpBackoffBlackout's projE row — do NOT
// "restore" the old per-site behavior without re-opening G5.
func effectiveCooldown(cooldownS int, priority float64, consecutiveFailures int, blackoutWindows []config.BlackoutWindow, now time.Time, calc *UrgencyCalculator) (cooldownDur time.Duration, skipMode bool) {
	cooldownDur = time.Duration(cooldownS) * time.Second
	if cooldownS == 0 {
		// Dynamic: derive from priority via the urgency calculator.
		if calc != nil {
			cooldownDur = calc.ComputeInterval(priority)
		}
	}
	// S-GAP-001: consecutive spawn failures back off exponentially.
	if consecutiveFailures > 0 {
		cooldownDur = FailureBackoff(cooldownDur, consecutiveFailures)
	}
	// Apply blackout slowdown if inside a peak-pricing window.
	if mult, inBlackout := config.ActiveMultiplier(blackoutWindows, now); inBlackout {
		if mult <= 0 {
			return cooldownDur, true // skip mode — don't spawn at all
		}
		if mult > 1.0 {
			cooldownDur = time.Duration(float64(cooldownDur) * mult)
		}
	}
	return cooldownDur, false
}

// effectiveCooldownDur resolves the receiver's calculator and blackout
// windows and delegates to the shared effectiveCooldown (ADV-R03 / G5).
// Kept as a method so packer.go's internal callers (the greedy pack and
// isOverdue) are unchanged. s.cooldownS is the POST-BUMP cooldown: Pick
// folds bump_cooldown_s into s.cooldownS while scoring rows (see the
// SCHED-GAP-107 block above), so the bump is already reflected here —
// which is why loop.go's watchdog feeds its own SQL-side bump value into
// the same package-level function instead.
func (p *Packer) effectiveCooldownDur(s scored, now time.Time) (cooldownDur time.Duration, skipMode bool) {
	return effectiveCooldown(s.cooldownS, s.priority, s.consecutiveFailures, p.blackoutWindows, now, p.calculator)
}

// isOverdue reports whether an enabled, not-running project is due under the
// GAP-011 hard rule: its time since last completed tick is STRICTLY greater
// than 2x its effective cooldown. The reference clock is last_tick_completed,
// falling back to created_at for projects that have never completed — a
// never-completed project counts as never cooldown-satisfied, so once past
// 2x it is due. Projects with no usable timestamp are never overdue; a
// skip-mode blackout (multiplier <= 0) suspends due selection entirely.
func (p *Packer) isOverdue(s scored, now time.Time) bool {
	cd, skipMode := p.effectiveCooldownDur(s, now)
	if skipMode {
		return false
	}
	ref := s.createdAt
	if s.lastTickAt != nil {
		ref = *s.lastTickAt
	}
	if ref.IsZero() {
		return false
	}
	age := now.Sub(ref)
	if age < 0 {
		return false
	}
	return age > 2*cd
}

// packed renders the scored project as a PackedProject for selection output.
func (s scored) packed() PackedProject {
	return PackedProject{
		Name:             s.name,
		Priority:         s.priority,
		Weight:           s.weight,
		Urgency:          s.urgency,
		Workdir:          s.workdir,
		RepoURL:          s.repoURL,
		Command:          s.command,
		Model:            s.model,
		Provider:         s.provider,
		FallbackModel:    s.fallbackModel,
		FallbackProvider: s.fallbackProvider,
		NoGlobalFallback: s.noGlobalFallback,
		ModelChain:       s.modelChain,
		IdleModel:        s.idleModel,
		IdleProvider:     s.idleProvider,
		WorkerModel:      s.workerModel,
		WorkerProvider:   s.workerProvider,
		GatewayKey:       s.gatewayKey,
		Deliver:          s.deliver,
		Prompt:           s.prompt,
		PromptMode:       s.promptMode,
		NamespacePrompt:  s.namespaceDefaultPmt,
		NamespaceChain:   s.namespaceChain,
		NamespaceID:      s.namespaceID,
	}
}

// Budget returns the current weight budget.
func (p *Packer) Budget() int { return p.budget }

// ListEnabled returns all enabled projects as PackedProject for simulation.
func (p *Packer) ListEnabled(ctx interface{}) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT name, weight, priority, workdir, repo_url
		FROM projects WHERE enabled = 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PackedProject
	for rows.Next() {
		var pp PackedProject
		if err := rows.Scan(&pp.Name, &pp.Weight, &pp.Priority, &pp.Workdir, &pp.RepoURL); err != nil {
			return nil, err
		}
		out = append(out, pp)
	}
	return out, rows.Err()
}
