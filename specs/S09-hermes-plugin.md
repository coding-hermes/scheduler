# S09 — Hermes Plugin & MCP Server

**Status:** Draft  
**Depends on:** S01, S06  
**Pages target:** 2-3

---

## 1. Overview

The scheduler integrates with Hermes Agent in two ways:

1. **MCP Server** (`/mcp`) — Hermes connects as an MCP client and auto-discovers fleet management tools
2. **Plugin hooks** (`plugin/hooks.py`) — Python-side hooks for the cron trigger integration

## 2. MCP Server (`internal/mcp/`)

```
internal/mcp/
  ├── server.go    — JSON-RPC HTTP handler, tool dispatch
  └── handlers.go  — Individual tool implementations
```

### 2.1 Protocol

- JSON-RPC 2.0 over HTTP POST at `/mcp`
- Standard MCP methods: `initialize`, `tools/list`, `tools/call`
- Tools expose fleet operations to Hermes agents

### 2.2 Available Tools

| Tool | Description |
|---|---|
| `scheduler_status` | Fleet health, active ticks, spawn counts |
| `scheduler_list_projects` | List all projects with status |
| `scheduler_list_namespaces` | List namespaces with allocations |
| `scheduler_tick_history` | Recent tick history |
| `scheduler_evaluate` | Trigger re-evaluation cycle |

### 2.3 Design Decisions

- Stateless JSON-RPC — no session management needed
- All tools are read-only queries against the database
- Write operations (evaluate trigger) go through the API, not MCP
- Tool dispatch uses a `map[string]ToolHandler` registry

## 3. Hermes Plugin (`plugin/hooks.py`)

The Python plugin provides cron-trigger integration:

```
plugin/
  └── hooks.py   — Hermes lifecycle hooks
```

### 3.1 Trigger Flow

1. Hermes cron fires (60s interval, `no_agent=true`)
2. Plugin hooks trigger `POST /api/v1/fleet/evaluate`
3. Scheduler evaluates urgency, packs projects, spawns foremen
4. Trigger returns session IDs to Hermes for tracking

### 3.2 Configuration

- Gateway URL from `HERMES_GATEWAY_URL` or defaults to `http://127.0.0.1:8642`
- Daemon URL from `SCHEDULER_URL` or defaults to `http://127.0.0.1:9090`
- Auth key from `HERMES_GATEWAY_KEY` env var
