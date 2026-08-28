package scheduler

import (
	"database/sql"
	"log"
	"sort"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
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
	lastTickAt          *time.Time
	createdAt           time.Time
	workdir             string
	repoURL             string
	command             string
	model               string
	provider            string
	fallbackModel       string
	fallbackProvider    string
	noGlobalFallback    bool
	modelChain          string
	idleModel           string
	idleProvider        string
	dailyBudgetUSD      float64
	weeklyBudgetUSD     float64
	finalBudgetUSD      float64
	workerModel         string
	workerProvider      string
	gatewayKey          string
	deliver             string
	prompt              string // Bane 2026-08-27: per-project extra foreman prompt
	promptMode          string // "append" (default) | "replace"
	namespaceDefaultPmt string // namespace default_prompt (empty = built-in)
	namespaceID         string // namespace_id (empty = no namespace)
	namespaceMaxConc    int    // namespace max_concurrent; 0 = unlimited (Bane 2026-08-27)
}

// Pick returns the selected projects for this tick, sorted by urgency desc.
func (p *Packer) Pick(now time.Time, spawnerRunning map[string]bool) ([]PackedProject, error) {
	rows, err := p.db.Query(`
		SELECT p.name, p.weight, p.priority, p.decay_rate, p.enabled, p.cooldown_s,
		       p.last_tick_completed,
		       p.created_at, p.workdir, p.repo_url, COALESCE(p.command, ''),
		       COALESCE(p.model, ''), COALESCE(p.provider, ''), COALESCE(p.fallback_model, ''), COALESCE(p.fallback_provider, ''), COALESCE(p.no_global_fallback, 0), COALESCE(p.model_chain, ''), COALESCE(p.idle_model, ''), COALESCE(p.idle_provider, ''), COALESCE(p.daily_budget_usd, 0.0), COALESCE(p.weekly_budget_usd, 0.0), COALESCE(p.final_budget_usd, 0.0), COALESCE(p.worker_model, ''), COALESCE(p.worker_provider, ''), COALESCE(p.gateway_key, ''), COALESCE(p.deliver, ''),
		       COALESCE(p.prompt, ''), COALESCE(p.prompt_mode, 'append'), COALESCE(ns.default_prompt, ''), COALESCE(ns.id, ''), COALESCE(ns.max_concurrent, 0),
		       p.consecutive_failures
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
		if err := rows.Scan(&s.name, &s.weight, &s.priority, &s.decayRate, &enabled, &s.cooldownS,
			&lastStr, &createdAtStr, &s.workdir, &s.repoURL, &s.command,
			&s.model, &s.provider, &s.fallbackModel, &s.fallbackProvider, &s.noGlobalFallback, &s.modelChain, &s.idleModel, &s.idleProvider, &s.dailyBudgetUSD, &s.weeklyBudgetUSD, &s.finalBudgetUSD, &s.workerModel, &s.workerProvider, &s.gatewayKey, &s.deliver,
			&s.prompt, &s.promptMode, &s.namespaceDefaultPmt, &s.namespaceID, &s.namespaceMaxConc,
			&s.consecutiveFailures); err != nil {
			log.Printf("ERROR scanning project row: %v", err)
			continue
		}
		// SCHED-GAP-066: budget-exhausted projects are never picked. The
		// gate filters NEW spawns only — a running tick is never touched.
		if p.budgetGate != nil {
			if detail, blocked := p.budgetGate(s.name, s.dailyBudgetUSD, s.weeklyBudgetUSD, s.finalBudgetUSD); blocked {
				log.Printf("BUDGET: %s blocked (%s) — excluded from selection; running ticks untouched", s.name, detail)
				skippedBudgetCap++
				continue
			}
		}
		s.createdAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if lastStr.Valid && lastStr.String != "" {
			t, err := time.Parse(time.RFC3339, lastStr.String)
			if err == nil {
				lastCompleted = &t
			}
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
		if used+s.weight > p.budget {
			totalSkippedBudget++
			continue
		}
		if currRunning >= p.maxConcurrent {
			log.Printf("PACKER: max concurrency reached (%d), stopping", p.maxConcurrent)
			break
		}
		cooldownDur := time.Duration(s.cooldownS) * time.Second
		if s.cooldownS == 0 {
			// Dynamic: derive from priority via urgency calculator.
			cooldownDur = p.calculator.ComputeInterval(s.priority)
		}
		// S-GAP-001: consecutive spawn failures back off exponentially.
		if s.consecutiveFailures > 0 {
			cooldownDur = FailureBackoff(cooldownDur, s.consecutiveFailures)
		}
		// Apply blackout slowdown if inside a peak-pricing window.
		if mult, inBlackout := config.ActiveMultiplier(p.blackoutWindows, now); inBlackout {
			if mult <= 0 {
				totalSkippedCooldown++
				continue // skip mode — don't spawn at all
			}
			if mult > 1.0 {
				cooldownDur = time.Duration(float64(cooldownDur) * mult)
			}
		}
		if s.lastTickAt != nil && now.Sub(*s.lastTickAt) < cooldownDur {
			totalSkippedCooldown++
			continue
		}
		packed = append(packed, PackedProject{
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
		})
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
