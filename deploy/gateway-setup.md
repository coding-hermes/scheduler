# Dedicated Gateway Setup — FEAT-004

## Why

The scheduler currently reuses the main Hermes gateway (PID on :8642) for foreman
ticks via `POST /v1/responses`. All 19+ concurrent foreman ticks run inside that
one process. If a heavy tick OOMs the gateway, **main chat dies too**.

The dedicated gateway runs on a separate port (:8643) with its own systemd cgroup
(`MemoryMax=16G`), isolated MCPs (duckbrain + gitreins only, no browser/chimera),
and independent restart cycle.

```
 Main Gateway (:8642)          Scheduler Gateway (:8643)
   ├─ main chat (Kara)           ├─ foreman tick A
   ├─ Telegram bridge            ├─ foreman tick B
   └─ ...                        └─ ...
         ↑                             ↑
    systemd cgroup              separate systemd cgroup (MemoryMax=16G)
```

## Prerequisites

- Hermes Agent v0.18+ installed at `~/.local/bin/hermes`
- `DEEPSEEK_FOREMAN_API_KEY` set in your environment (`.bashrc` or `.env`)
- DuckBrain MCP wrapper at `~/duckbrain/bin/hermes-mcp-wrapper.sh`
- GitReins MCP wrapper at `~/gitreins-poc/bin/hermes-mcp-wrapper.sh`

## Setup

### 1. Create the scheduler profile

```bash
mkdir -p ~/.hermes/profiles/scheduler
cp deploy/scheduler-profile/config.yaml ~/.hermes/profiles/scheduler/config.yaml
```

Edit `~/.hermes/profiles/scheduler/config.yaml` and verify:
- `providers.deepseek-foreman.api_key` references your `DEEPSEEK_FOREMAN_API_KEY` env var
- MCP paths (`duckbrain`, `gitreins`) exist and are executable

### 2. Install the systemd user unit

```bash
mkdir -p ~/.config/systemd/user
cp deploy/coding-hermes-scheduler-gateway.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable coding-hermes-scheduler-gateway
```

### 3. Start the gateway

```bash
systemctl --user start coding-hermes-scheduler-gateway
```

### 4. Verify

```bash
# Health check
curl -s http://127.0.0.1:8643/health

# Should return: {"status":"ok","version":"0.18.2"}

# Check logs
journalctl --user -u coding-hermes-scheduler-gateway -f
```

### 5. Point the scheduler at the dedicated gateway

```bash
# Restart schedulerd with the dedicated gateway URL
systemctl --user restart coding-hermes-scheduler
# (The schedulerd already has --gateway-url defaulting to :8642;
#  update the systemd unit's ExecStart to add: --gateway-url http://127.0.0.1:8643)
```

Or update `deploy/coding-hermes-scheduler.service` to add:
```
ExecStart=... --gateway-url http://127.0.0.1:8643
```

## Operations

| Command | Purpose |
|---------|---------|
| `systemctl --user status coding-hermes-scheduler-gateway` | Check gateway status |
| `journalctl --user -u coding-hermes-scheduler-gateway -f` | Tail gateway logs |
| `systemctl --user restart coding-hermes-scheduler-gateway` | Restart after config change |
| `curl -s http://127.0.0.1:8643/v1/models` | List available models |
| `systemctl --user stop coding-hermes-scheduler-gateway` | Stop gateway |

## Resource Isolation

| Resource | Main Gateway (:8642) | Scheduler Gateway (:8643) |
|----------|---------------------|--------------------------|
| MemoryMax | System default | 16G |
| Cgroup | Main chat process | Separate cgroup |
| MCPs | All (browser, chimera, flights, duckbrain, gitreins) | duckbrain + gitreins |
| OOM impact | Kills main chat | Scheduler ticks fail, main chat survives |
| Restart | Drops user chats | Drops in-flight foreman ticks (retryable) |

## Known Limitations

- Gateway restart drops in-flight foreman ticks. The scheduler's `spawn.go` falls
  back to `exec.Command` when the gateway is unreachable (2 retries with backoff).
- Profile is a minimal template — you may need to add provider-specific API keys
  if workers use providers other than deepseek-foreman.
- The gateway on :8643 shares the same DuckBrain and GitReins instances as :8642.
  This is intentional — foremen need the same memory namespace.

## Related

- `deploy/coding-hermes-scheduler.service` — main scheduler daemon
- `deploy/scheduler-profile/config.yaml` — gateway profile template
- FEAT-003: HTTP API spawn (the `--gateway-url` flag this builds on)
- ADR: `docs/adr/001-http-spawn-vs-dedicated-gateway.md` (planned — DOC-002)
