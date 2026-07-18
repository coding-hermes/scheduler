# ADR-001: HTTP Spawn vs Dedicated Gateway Instance

**Status:** Accepted
**Date:** 2026-07-18
**Author:** coding-hermes-scheduler foreman
**Deciders:** Bane (Alexis Okuwa)

## Context

The coding-hermes scheduler dispatches foreman ticks by spawning agent sessions.
Originally, every tick launched a full `hermes chat -q` subprocess (~500MB RAM,
33K token system prompt). FEAT-003 replaced this with HTTP calls to the Hermes
gateway API at `POST /v1/responses`, reusing the already-running gateway process.

This works — but forces the scheduler and main chat to share a single gateway.
A stuck main-chat session, a memory spike from a large context, or a gateway
restart takes down ALL scheduler ticks with it. FEAT-004 proposes a dedicated
gateway instance with its own cgroup, config, and lifecycle.

This ADR documents the tradeoffs between the three approaches.

## Options

### Option A: Shared Gateway (Current — FEAT-003)

The scheduler calls `POST http://127.0.0.1:8642/v1/responses` on the main Hermes
gateway — the same process that serves the interactive chat, Telegram bridge, and
all other Hermes functionality.

**Pros:**
- Zero additional processes. No port management, no extra config.
- Auto-approve works out of the box (the gateway is the same process that approves).
- Simple: one gateway, one config, one place to debug.
- MCP servers (duckbrain, gitreins) loaded once, shared by all consumers.

**Cons:**
- Shared fate. A stuck main-chat session (large context, slow model) blocks the
  gateway's event loop → scheduler ticks queue up or time out.
- No cgroup isolation. A memory spike in the scheduler's foreman ticks can OOM
  the main chat session.
- Recursive self-tick. The `coding-hermes-scheduler` project's own foreman ticks
  run on the same gateway that the scheduler uses to spawn them. If the scheduler
  foreman blocks the gateway, new ticks can't spawn — including the tick that
  would fix the blocking issue.
- Gateway restart kills all in-flight ticks. The scheduler retries (commit `bdc75ea`
  added backoff), but in-flight work is lost.

### Option B: Dedicated Gateway (FEAT-004)

A separate Hermes gateway instance on `127.0.0.1:8643` with its own config
(`deploy/scheduler-profile/config.yaml`), systemd unit with `MemoryMax=16G`,
and minimal MCP footprint (duckbrain + gitreins only, no browser/chimera/flights).

**Pros:**
- Cgroup isolation. Scheduler OOM kills the scheduler gateway, not the main chat.
- Independent restart. Gateway restart only affects scheduler ticks, not the user's
  interactive session.
- Reduced attack surface. No browser, chimera, or flights MCPs loaded — less
  memory, fewer failure modes.
- Clean separation of concerns. Scheduler is a background service; its gateway
  should be too.

**Cons:**
- Extra process. Another Hermes instance to monitor, update, and debug.
- Separate config maintenance. Two `config.yaml` files to keep in sync (model
  list, provider keys, MCP server endpoints).
- Port management. `:8643` must not collide with other services.
- Manual startup. Currently requires manual `API_SERVER_KEY` provisioning and
  `systemctl --user start`. Not auto-launched by the scheduler.
- Two places to check when a tick fails (was it the scheduler, the gateway, or
  the spawned agent?).

### Option C: Hybrid Pool (Future)

A pool of N dedicated gateway workers behind a load balancer, with auto-scaling
based on queue depth. Each worker has its own cgroup, and the scheduler round-robins
or least-connects across them.

**Pros:**
- Horizontal scaling. Handle more concurrent ticks by adding workers.
- Graceful degradation. One worker down doesn't block all ticks.
- Auto-scaling could respond to fleet load (more projects = more workers).

**Cons:**
- Significant complexity. Load balancer, health checks, worker lifecycle management.
- Overkill for current scale. The fleet has ~27 projects; 8 concurrent ticks max
  fits comfortably in one gateway.
- Premature optimization. Build this when the single dedicated gateway becomes
  the bottleneck, not before.

## Decision

**Dedicated gateway for production, shared gateway for development.**

Rationale:
1. The shared gateway (Option A) is the right choice for development — one process
   to start, zero config drift, immediate feedback.
2. For production (the live fleet on `37.27.250.128` or Bunker), cgroup isolation
   is non-negotiable. The scheduler runs 24/7 with hundreds of ticks per day. A
   single OOM event on the main gateway should not cascade-kill every project's
   foreman tick.
3. The dedicated gateway's cons (extra process, separate config) are mitigated by
   the deploy artifacts already created in FEAT-004: the systemd unit, the profile
   config, and the setup docs.
4. Option C (hybrid pool) is deferred. When the fleet grows beyond what one gateway
   can handle, or when a single gateway becomes a reliability bottleneck, revisit.

## Consequences

- **Deployment:** The scheduler's systemd unit must be updated to point at
  `--gateway-url http://127.0.0.1:8643` once the dedicated gateway is provisioned.
- **Monitoring:** Two gateway health checks instead of one. The supervisor should
  monitor both `:8642/health` and `:8643/health`.
- **Config drift:** Changes to the main gateway's model list or provider keys must
  be mirrored to the scheduler profile. A sync script or shared config include
  should be considered.
- **Startup order:** The dedicated gateway must start before the scheduler daemon.
  Systemd `After=` and `Requires=` directives in the scheduler unit enforce this.
- **Fallback:** If the dedicated gateway is unreachable, the scheduler falls back
  to `exec.Command("hermes", "chat", "-q", ...)`. Commit `bdc75ea` added retry
  with backoff for the gateway path; the exec fallback is the last resort.
