# SCHED-GAP-219 Task D findings — task-state inconsistencies in the scheduler

## How task mode is represented

- Global pacing: `[scheduler] tasks_pacing` (root TOML, config.go:89) → flag `--tasks-pacing` (main.go:54, 60s fleet default) → env `SCHEDULER_TASKS_PACING` (main.go:169) → `scheduler.SetTasksPacing(*tasksPacing)` (main.go:369) → packer predicates `tasksPacingDeferredJittered` / `tasksPacingDeferred` (internal/scheduler/tasks_pacing.go).
- Per-project/per-namespace: `projects.admission_mode` ('' | cooldown | tasks, migration v30) overriding `namespaces.admission_mode`; read at packer_select.go:249, multipool_packer.go:276, packer.go:347, loop.go:1714, and post-tick by adaptive_cooldown.go:148 (`admissionModeForProject`).

## FINDING 1 (real bug, boot-order wiring): SetTasksPacing fires BEFORE the TOML block applies

cmd/schedulerd/main.go:369 calls `scheduler.SetTasksPacing(*tasksPacing)` inside loop setup.
The root-TOML default-guard block that may rewrite `*tasksPacing` runs LATER at main.go:471-478 — and when it fires it rewrites the flag variable but NEVER re-calls `scheduler.SetTasksPacing`.

Consequence: every sibling key with the same pattern re-applies to its consumer after the TOML block (SetSpawnMemLimitMB is re-called at main.go:485; gatewayResponseTimeout/slotPatience are read via the resolved-config snapshot and the spawner setter happens before too BUT those are loop methods read later; the pacing global, however, is write-once at wiring). Net effect: a `[scheduler] tasks_pacing` value in fleet.toml is silently DEAD — /api/v1/config reports the TOML value (ResolvedConfig.TasksPacing from the rewritten flag) while the running scheduler still paces at 60s. Config surface and runtime disagree — the exact "written by one path, read under another name" shape this task asks about.

Fix shape: move the `SetTasksPacing(*tasksPacing)` call AFTER the TOML default-guard block (or re-call it when the TOML layer rewrites the flag). Same audit applies to SetLoadGateThreshold (main.go:373, TOML apply at 457-460 — same ordering, gate armed from flag value only; mitigated because the load gate was armed 12 via flag/env in the fleet, but the TOML layer is equally dead) and SetWaveLoadCeiling (main.go:378).

## FINDING 2: tasks_pacing applies regardless of admission mode by design, but the mirror predicates diverge (accepted, documented)

tasks_pacing.go:89-115: packer defers through base+jitter (1.2x), the eligibility mirror defers through base only — a ≤12s undercount window of eligible projects, documented as accepted (GAP-043 safe direction). No action.

## FINDING 3: adaptive cooldown escalation is mode-gated but the FLOOR restore is ownership-blind

adaptive_cooldown.go:148: tasks-mode rows keep the cooldown floor-pin; escalation is mode-gated. Satellites on a shared board (SCHED-GAP-141) keep the floor pin — deliberate per the admission_mode.go header comment. No action.

## FINDING 4 (behaviour change, NOT applied — needs Bane's ruling): task-mode projects still get policy cooldowns

Nothing in the scheduler waives the cooldown PIN for tasks-mode lanes except the board-work waiver (packer_select.go:249) which requires board ownership. A 6h-pinned tasks lane that drains its board waits 6h before re-checking the board — by design (ADV-R07: wall-clock cooldown is the sole admission authority). With the SCHED-GAP-219 authority change, an operator PIN (e.g. 86400) now outlives restarts, so a tasks lane pinned high by a cooldown-era ruling stays slow forever in task mode unless the pin is cleared via API. This is a POLICY question (pins were cooldown-era judgments), not a code bug — filed for Bane's ruling, not changed.

## FINDING 5: boot recomputation vs DB — resolved by SCHED-GAP-219

The classic shape ("a value applied at boot overwriting a live value") WAS the loader re-pin, which this task's sibling change (SCHED-GAP-219 commit) eliminated. The remaining boot-time recomputations are: adaptive policy normalization (only on false→true transition — safe), bump snapshot restore (owns its own state — safe), and derived ceiling (8×floor — no DB contradiction).