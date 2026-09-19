# Scheduler Dogfood Integration — 2026-09-19 (fresh-clone install run)

## What this run tested

This was an INSTALLABILITY run, not a feature audit: an ephemeral bunker agent
(las-bunker-03) with no Go toolchain, no Hermes install, and no prior state
cloned the public repo at c30ca1d and followed README "Getting Started (5
minutes)" exactly as written.

## The working fresh-machine recipe (as a user actually experiences it)

```bash
# 0. Go 1.26+ (README prerequisite, correct)
curl -sSLo go.tgz https://dl.google.com/go/go1.26.0.linux-amd64.tar.gz   # 25s
tar -C ~ -xzf go.tgz && export PATH=~/go/bin:$PATH

# 1. README steps 1-2 work verbatim
git clone https://github.com/coding-hermes/scheduler.git && cd scheduler
make build            # 84s from cold, produces bin/schedulerd + bin/migrate

# 2. README step 5 FAILS on a clean machine — SCHED-GAP-182
mkdir -p ~/.hermes/coding-hermes     # the undocumented step
./bin/schedulerd      # boots clean once the dir exists

# 3. Everything below then works:
curl -s localhost:9090/api/v1/health    # {"status":"ok","db":"connected",...}
curl -s localhost:9090/api/v1/openapi.json
# Dashboard on / (HTTP 200), MCP at /mcp:
#   initialize → protocolVersion 2024-11-05, serverInfo v1.3.0-163-gc30ca1d
#   tools/list → fleet_status, fleet_projects, ...

# 4. README step 4 (migrate) on a machine with no Hermes cron jobs — SCHED-GAP-184
./bin/migrate --dry-run
#   FATAL: load jobs: open ~/.hermes/cron/jobs.json: no such file or directory
# (Optional step: the scheduler runs fine with zero projects without it.)

# 5. First project create — SCHED-GAP-183
curl -s -X POST localhost:9090/api/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{"name":"df","repo_url":"https://example.com/x.git","workdir":"/tmp/df","prompt":"tick"}'
# repo_url is REQUIRED (400 without it; README never says so) and the row is
# created enabled:false — you must enable it before it can ever schedule.

# 6. API lifecycle verified end-to-end: evaluate, queue, ticks, events
# (incl. a HIGH duckbrain-sync "unreachable — writes spooled for replay"
# event — correct fail-open behavior), pause w/ provenance, delete confirm.

# 7. README's own gate: make test on the fresh clone → EXIT=0 in 102s.
```

## Judged friction points (filed to the board)

| ID | Severity | Issue |
|----|----------|-------|
| SCHED-GAP-182 | P1 | `schedulerd` FATALs at first boot: `~/.hermes/coding-hermes/` never created (WAL open error 14) |
| SCHED-GAP-183 | P2 | `repo_url` required but undocumented; created projects arrive disabled with no README hint |
| SCHED-GAP-184 | P2 | `make migrate-dry` dies raw on machines without Hermes cron jobs instead of reporting an empty import set |

## Verdict

PROMISING-BUT-ROUGH. The core product (single-binary scheduler: build, boot,
API, MCP, dashboard, fresh-clone test suite) is solid — every backend behavior
probed worked first try. The entire friction is concentrated in the first-60-
seconds onboarding path, which the docs assume happens on a machine that
already runs Hermes. Time-to-first-success: ~12 min (a clean 5 if the three
filed gaps were fixed).
