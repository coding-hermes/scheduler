# Coding Hermes Fleet — H3 Status

**Current as of 2026-07-18.** Managed by Coding Hermes Scheduler (`schedulerd :9090`).

## Architecture

```
schedulerd (Go daemon, :9090)
├── namespace: coding-hermes (27 projects)
│   ├── hivemind-pulse    W=10 P=8  thread=59430
│   ├── hivemind-work     W=10 P=8  thread=59430
│   ├── helios-work       W=10 P=7  thread=4297
│   ├── helios            W=10 P=7  thread=4297
│   ├── helix             W=10 P=7  thread=56520
│   ├── speclang          W=10 P=8  thread=17441
│   ├── asce              W=10 P=8  thread=12
│   ├── bunker            W=10 P=7  thread=70902
│   ├── Kobayashi-Maru    W=10 P=7  thread=4273
│   ├── wojons-mythos     W=10 P=6  thread=15040
│   ├── dexdat-core       W=10 P=6  thread=62373
│   ├── dexdat-memory     W=10 P=6  thread=4259
│   ├── imhotep           W=10 P=6  thread=81994
│   ├── off-by-one        W=10 P=6  thread=72503
│   ├── hermes4friends    W=10 P=5  thread=7
│   ├── mafia-ai-bench    W=10 P=5  thread=4409
│   ├── muster            W=10 P=5  thread=64245
│   ├── crier             W=10 P=5  thread=82666
│   ├── duckbrain         W=10 P=5  thread=2065
│   ├── deepseek-dash     W=10 P=4  thread=68481
│   ├── totalstack        W=10 P=4  thread=60650
│   ├── warpfs            W=10 P=4  thread=60183
│   ├── chimera-v2        W=10 P=4  thread=50404
│   ├── escalation        W=10 P=4  thread=78404
│   ├── rethinkdb         W=10 P=3  thread=76549
│   ├── eduos             W=10 P=3  thread=50253
│   └── consensus         W=10 P=3  thread=4314
├── namespace: global (4 test projects, disabled)
└── Supervisor cron: 55afdcd33d7f (every 4h, fleet governance)
```

## Key Features (since migration from cron)

| Feature | Status | Description |
|---------|--------|-------------|
| Auto-slowdown | ✅ | IDLE ticks → double cooldown (600s→1200s→...→14400s cap) |
| Cooldown floors | ✅ | All projects >= 1200s (20min), idle capped at 14400s (4h) |
| Per-project delivery | ✅ | `hermes send` through Hermes gateway to correct thread |
| Output trimming | ✅ | `trimSummary()` strips diffs/build logs, keeps foreman report |
| Startup cleanup | ✅ | `cleanDanglingOnStartup()` clears dead-process ticks |
| Goroutine leak | ✅ | BUG-004 fixed — pipe closed on context timeout |
| Git identity | ✅ | Foreman sets `totalwindupflightsystems@gmail.com` per tick + Co-authored-by |
| Dashboard | ✅ | HTML at :9090/dashboard (times out on large fleets — needs optimization) |

## Skills Map

| Skill | Layer | Who loads it |
|-------|-------|-------------|
| `coding-hermes-north-star` | Management | Bane sessions |
| `coding-hermes-supervisor` | Supervisor | Cron 55afdcd33d7f |
| `coding-hermes-cron` | Supervisor+Foreman | Supervisor + all foremen |
| `coding-hermes-foreman` | Foreman | Every per-project tick |
| `hilo-usage` | Foreman | Spatial impact analysis |
| `gitreins` | Foreman | Quality gates |
| `coding-hermes-scheduler` | Operations | API, tuning, troubleshooting |

## Provider Rules (Golden)

- **Foremen:** deepseek-foreman (PAYG) — always reachable, credit-fill unblocks
- **Supervisor:** deepseek-v4-flash @ opencode-go (flat-rate) — light work
- **Workers:** Prepaid flat-rate buckets (zai-glm, minimax, xai-oauth, openai-codex)

## Links

- **Repo:** https://github.com/coding-hermes/scheduler
- **Board:** `.coding-hermes/tasks.md` (9 [x], 1 deferred)
- **Verify:** `bin/schedulerd --test-verify 5`
- **Health:** `curl http://127.0.0.1:9090/api/v1/health`
- **Dashboard:** `http://127.0.0.1:9090/dashboard`
