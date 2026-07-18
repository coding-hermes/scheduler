### [x] BUG — Events table schema mismatch: level vs severity column ✓ `e6afa32`
**Priority: MEDIUM. Weight: 5.**
- Migration v5 recreates events table with severity, component, details columns matching events.go INSERT
- Database Event struct updated (Severity, Component, Details), old EventLevel type updated to EventSeverity
- LogEvent, ListEvents, API /api/v1/events handler all updated
- 91 insertions, 72 deletions across 5 files. Guard: PASS. All tests: PASS.

---

## TESTING & VERIFICATION — 2026-07-16

> Foreman: run `./bin/schedulerd --test-verify 3` before each tick. Fix failures below.

### [x] TEST-001 — Built-in correctness verification ✓ `71e66db`
**Priority: HIGH. Weight: 15.**
- `cmd/schedulerd/test_verify.go`: temp DB, 7-project fleet, N-cycle test
- 6 invariants: no hangs, full coverage, budget capping, no dupes, session IDs, priority ordering
- Exit 0 = pass, exit 1 = failures. Creates self-contained DB, cleans up.

### [x] TEST-002 — VERIFY-BUG-001: Session ID capture broken for custom commands ✓ `fa23309`
**Priority: HIGH. Weight: 8.**
- Fix: broadened regex match in spawn.go, bash -c commands pass script intact to shell
- Acceptance: `--test-verify 3` now shows all ticks with non-empty session IDs
- Fixed in `fa23309`, verified in `c4bb0eb`. All 6 verify checks green.

### [x] TEST-003 — VERIFY-BUG-002: Low-priority projects starved in 3 cycles ✓ `88b3c72`
**Priority: MEDIUM. Weight: 5.**
- Fix: dynamic cooldown derived from priority when cooldown_s=0. Cooldown enforcement in packer.
- Acceptance: `--test-verify 3` shows all 7 projects with ≥1 tick each
- Fixed in `88b3c72`, verified in `75e29cb`. Starvation prevention works.

### [x] TEST-004 — BUG: alert_escalation.go queries non-existent columns ✓ `e0ff63f`
**Priority: HIGH. Weight: 8.**
- `alert_escalation.go: min_interval → cooldown_s, tick_id → id`
- Hot-path no longer spams logs every evaluation cycle
- Fixed in `e0ff63f`, all alert escalation tests passing.

### [x] TEST-005 — Verification cron job ✓
**Priority: HIGH. Weight: 10.**
- Created `deploy/scheduler-verify.sh` wrapper script
- Host crontab entry: `0 */2 * * *` runs `./bin/schedulerd --test-verify 3` every 2h
- Verified: `--test-verify 3` passes all 6 checks

### [ ] BUG-004 — Goroutine/memory leak: 659 tasks, 8GB after 18h uptime
**Priority: HIGH. Weight: 12.**
- **Symptom:** After 18h, schedulerd has 659 goroutines, 8GB RAM (22.8GB peak), API unresponsive.
  Only 15 ticks running, 10K completed — the bloat is in the daemon, not subprocesses.
- **Likely causes:**
  1. Goroutine leak in spawn loop or lifecycle tracker — each tick spawns a goroutine to read
     stdout for session_id; if the goroutine blocks on a dead pipe, it accumulates forever.
  2. Memory: 10K tick records in SQLite held in-memory via conn pool or unbuffered queries.
  3. Lifecycle tracker polling every N seconds — builds up timers that never fire on dead ticks.
- **Fix candidates:**
  - Add context.WithTimeout to stdout scanner goroutine (kill after tick timeout)
  - Cap in-memory tick cache / paginate queries
  - Add `--tick-timeout` flag with default 30m, reap running ticks that exceed it
  - Periodic goroutine count logging to catch leaks early
- **Verification:** After restart, goroutine count should stabilize under 50 within 10 minutes.
  Memory should stay under 1GB for 10K tick records.

### [ ] INFRA-003 — Telegram delivery for scheduler tick outcomes
**Priority: CRITICAL. Weight: 20.**
- **Root cause:** Scheduler spawns `hermes chat -q -Q` as a subprocess → stdout only, no delivery.
  Cron system runs agent *in-process* via `AIAgent` then calls `_deliver_result()` → Telegram.
- **Fix:** Add `deliver` column to projects table (platform:chat_id:thread_id). After tick
  completes, capture final_response from stdout, wrap with `[Scheduler tick: ...]` header,
  and POST to Telegram via bot API or hermes send_message tool.
- **Pattern:** Cron's `_deliver_result()` wraps with `"Cronjob Response: {name}\n(job_id: {id})"`.
  Scheduler should wrap with `"🤖 Scheduler Tick: {project} [{tick_id}]"`.
- **Delivery targets** available from paused cron jobs (extract `deliver` field, map to projects).
- **Verification:** After deploy, a scheduler tick should produce a Telegram message starting
  with `🤖 Scheduler Tick:` within 5-15 minutes.

### [ ] INFRA-002 — TOML config support for project definitions
**Priority: LOW. Weight: 5.**
- Want: `schedulerd --config fleet.toml` declarative fleet definition
- [projects.alpha], [projects.beta], etc. with weight, priority, command, namespace
- [namespaces.coding-hermes] with weight, reserved, hard_cap
- TOML preferred over YAML — cleaner, no whitespace sensitivity
- Later: hot-reload on SIGHUP

### [ ] FOREMAN-TASK — Run this board
**Priority: HIGH. Weight: ∞.**
- Foreman reads this board before every tick. Self-heals git. Picks highest-priority undone task.
- INFRA-002 (TOML config) is lowest priority — defer to future tick
- Add sub-tasks marked TEST-xxx-A for unit test coverage after each fix
