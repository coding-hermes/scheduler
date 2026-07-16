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

### [ ] TEST-002 — VERIFY-BUG-001: Session ID capture broken for custom commands
**Priority: HIGH. Weight: 8.**
- Spawner fails to parse `session_id: <id>` from stdout of `bash -c '...'` wrapped commands
- All verify ticks show empty session IDs. Root cause in `spawn.go` session_id regex.
- Fix: broaden regex match in spawn.go ~line 145 to handle single-quoted output, or normalize stdout
- Acceptance: `--test-verify 3` shows all 21 ticks with non-empty session IDs

### [ ] TEST-003 — VERIFY-BUG-002: Low-priority projects starved in 3 cycles
**Priority: MEDIUM. Weight: 5.**
- Only 5/7 projects got ticks — delta (p=4), eta (p=1) never selected
- Cooldowns not forcing rotation. Urgency decay insufficient.
- Fix: verify cooldown enforcement in packer.go cooldown check. Ensure completed ticks update `last_tick_completed`
- After fix, add: **TEST-003-A** unit test for starvation prevention (cooldowns force rotation within 5 cycles)
- Acceptance: `--test-verify 5` shows all 7 projects with ≥1 tick each

### [ ] TEST-004 — BUG: alert_escalation.go queries non-existent columns
**Priority: HIGH. Weight: 8.**
- `alert_escalation.go:194`: `SELECT ... min_interval FROM projects` — column missing
- `alert_escalation.go:153`: `SELECT ... tick_id FROM ticks` — should be `id`
- Hot-path fails every evaluation, spamming logs. Invisible to tests (logs-only).
- Fix: align column names with actual schema. Add unit test for StarvationCheck.
- After fix, add: **TEST-004-A** unit test for alert_escalation.go that queries test DB

### [ ] TEST-005 — Idle-time verification cron job
**Priority: HIGH. Weight: 10.**
- Foreman: create cron job `scheduler-verify` running `./bin/schedulerd --test-verify 3` every 2h
- no_agent=true, deliver=telegram (or origin). Alerts on failure.
- Adds an additional **TEST-005-A** task: if verify fails, foreman auto-files a bug task for root cause
- Acceptance: cron exists, last run output shows "✅ SCHEDULER VERIFIED"

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
- TEST-004 (alert_escalation SQL) is highest priority — fixes a hot-path spam bug
- Then TEST-002 (session capture), then TEST-003 (starvation)
- Add sub-tasks marked TEST-xxx-A for unit test coverage after each fix
