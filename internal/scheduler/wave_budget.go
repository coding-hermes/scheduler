package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §6.2 item 3 (SCHED-GAP-113): namespace wave_workers_cap ──────────
//
// DECISION 3 enforces the cap in TWO layers because no single layer can
// enforce it alone (§6.3): the COMPOSITION layer advises at the point where
// the wave decision is actually made (the foreman prompt — wave composition
// happens inside the tick, by the foreman), and the ADMISSION layer sheds at
// the tick boundary, where the scheduler has authority. Both layers leave
// slot accounting untouched: a wave tick occupies exactly ONE slot, ONE
// entry in SlotPool.running, ONE in RunningSet() (W1/W3), and namespace
// max_concurrent keeps counting TICKS (W2). Worker processes never enter
// running/reserved/RunningSet()/active_ticks.

// waveBudgetLine renders the single line the composition layer appends to
// the foreman prompt. The EXACT injected line is:
//
//	WAVE_BUDGET: <n> — max concurrent wave workers this tick (0 = serial tick, do not compose a wave).
//
// Semantics: n = wave_workers_cap(namespace) - live_wave_depth(namespace),
// clamped at 0 (never negative). live_wave_depth is SUM(worker_count) over
// the namespace's RUNNING ticks (database.CountRunningWorkersByNamespace).
// n = 0 means "serial tick — do not compose a wave this tick": the foreman
// runs its usual one-task tick with no wave members. The line is ABSENT —
// and the prompt stays byte-identical to the pre-113 builder — for projects
// with no namespace or a namespace whose wave_workers_cap is 0 (unlimited;
// default-off per spec §15). The line always lands on its own line at the
// end of the prompt, after the dynamic workdir/worker-model footer, so a
// prompt_mode="replace" project prompt cannot lose it.
func waveBudgetLine(n int) string {
	return fmt.Sprintf("WAVE_BUDGET: %d — max concurrent wave workers this tick (0 = serial tick, do not compose a wave).", n)
}

// namespaceWaveWorkersCap loads wave_workers_cap for one namespace. Single
// PK lookup per spawn — the same perf class as SCHED-GAP-111's waveNamespace
// (spec §14: serial fleets pay nothing; a capped namespace pays one SELECT
// next to the gateway HTTP call it bounds). ok=false for an empty namespace
// id, unknown namespaces, and DB errors (logged) — every miss means "inject
// nothing", the fail-open default-off answer.
func (s *Spawner) namespaceWaveWorkersCap(ctx context.Context, namespaceID string) (int, bool) {
	if namespaceID == "" {
		return 0, false
	}
	var wcap int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(wave_workers_cap, 0) FROM namespaces WHERE id = ?`,
		namespaceID).Scan(&wcap)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		log.Printf("WARN: wave_workers_cap lookup for namespace %q failed (%v) — WAVE_BUDGET not injected this tick", namespaceID, err)
		return 0, false
	}
	return wcap, true
}

// waveBudget resolves the WAVE_BUDGET figure for one spawn (the composition
// layer's single resolution site). Returns (n, inject):
//
//   - inject=false — no namespace, cap lookup miss, or a live-depth query
//     error: the scheduler has no trustworthy cap signal, so it injects
//     NOTHING (unlimited, today's behavior). Fail-open matches §15.
//   - inject=true, n>0 — the namespace's remaining wave worker budget.
//   - inject=true, n=0 — serial tick: either the cap is exhausted by live
//     depth (n clamped at 0, never negative) or the admission layer already
//     shed the namespace (project.WaveSerial, set by the packer when a live
//     wave was in flight at pack time). The pack-time shed decision is
//     authoritative and deliberately coarse (§6.2: consistent with
//     SCHED-GAP-103's tick-boundary discipline) — the spawn does NOT
//     re-read live depth to un-shed itself, because a wave completing
//     between pack and spawn must not open a second-wave race.
//
// A serial tick (n=0 due to the cap or the shed) logs one WAVE-SHED line so
// the forced serial tick is observable.
func (s *Spawner) waveBudget(project PackedProject) (n int, inject bool) {
	if project.WaveSerial {
		log.Printf("WAVE-SHED: %s namespace=%s WAVE_BUDGET: 0 (packer shed — live wave in flight at pack time) — serial tick",
			project.Name, project.NamespaceID)
		return 0, true
	}
	wcap, ok := s.namespaceWaveWorkersCap(context.Background(), project.NamespaceID)
	if !ok || wcap <= 0 {
		return 0, false // wave_workers_cap = 0 → unlimited → inject NOTHING
	}
	depth, err := database.CountRunningWorkersByNamespace(context.Background(), s.db, project.NamespaceID)
	if err != nil {
		log.Printf("WARN: live wave depth for namespace %q failed (%v) — WAVE_BUDGET not injected this tick", project.NamespaceID, err)
		return 0, false
	}
	n = wcap - depth
	if n < 0 {
		n = 0 // clamp: never a negative budget
	}
	if n == 0 {
		log.Printf("WAVE-SHED: %s namespace=%s cap=%d live_depth=%d — cap forces a serial tick",
			project.Name, project.NamespaceID, wcap, depth)
	}
	return n, true
}

// buildSpawnPrompt assembles the foreman prompt for one spawn: the
// SCHED-GAP-078 body (namespace default_prompt + project append/replace +
// the dynamic tick-id/workdir/worker-model footer) plus, when the project's
// namespace sets wave_workers_cap > 0, the single WAVE_BUDGET line (S12
// §6.2 composition layer). Namespaces without a cap — and every error path
// in waveBudget — return the body BYTE-IDENTICAL to buildForemanPrompt
// (asserted by test), so default-off fleets see zero prompt change.
func (s *Spawner) buildSpawnPrompt(project PackedProject, tickID string) string {
	prompt := buildForemanPrompt(project, tickID)
	n, inject := s.waveBudget(project)
	if !inject {
		return prompt
	}
	return prompt + "\n" + waveBudgetLine(n)
}

// resolveWaveShed returns the per-cycle set of namespace IDs in
// "wave-shed" (S12 §6.2 item 3, admission layer — SCHED-GAP-113): every
// namespace with wave_workers_cap > 0 that currently has an in-flight tick
// with worker_count > 0. This is deliberately COARSER than the composition
// arithmetic — ANY live wave in a capped namespace sheds further waves
// there (the namespace's next ticks go serial), not just a depth-exhausting
// one. Consistent with SCHED-GAP-103: the scheduler sheds at tick
// boundaries, where it has authority, never mid-tick (§6.3).
//
// Cost (spec §14): zero queries while no namespace sets a cap; otherwise
// ONE ListRunningWaves query (indexed over running ticks, the SCHED-GAP-112
// status surface). A query error logs and returns nil — no shed this cycle
// (fail-open: scheduling continues exactly as before SCHED-GAP-113).
func resolveWaveShed(ctx context.Context, db *sql.DB, namespaces []database.Namespace) map[string]bool {
	anyCapped := false
	for _, ns := range namespaces {
		if ns.WaveWorkersCap > 0 {
			anyCapped = true
			break
		}
	}
	if !anyCapped {
		return nil
	}
	waves, err := database.ListRunningWaves(ctx, db)
	if err != nil {
		log.Printf("WARN: wave shed scan failed (%v) — no namespace shed this cycle (fail-open)", err)
		return nil
	}
	shed := make(map[string]bool)
	for _, ns := range namespaces {
		if ns.WaveWorkersCap <= 0 {
			continue
		}
		for _, w := range waves {
			if w.NamespaceID == ns.ID && w.WorkerCount > 0 {
				shed[ns.ID] = true
				break
			}
		}
	}
	return shed
}
