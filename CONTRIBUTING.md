# Contributing to Coding Hermes Scheduler

Thanks for contributing. This guide covers everything you need to submit a pull request.

## Setup

```bash
git clone https://github.com/coding-hermes/scheduler.git
cd scheduler
make build
make test
```

**Requirements:** Go 1.23+, SQLite3.

For integration testing you need a running Hermes gateway. See [deploy/gateway-setup.md](deploy/gateway-setup.md).

## Development Workflow

### 1. Pick a task

Check the `.coding-hermes/tasks.md` board or open an issue.

### 2. Branch

```bash
git checkout -b feat/my-feature
```

Branch naming: `feat/`, `fix/`, `docs/`, `chore/` prefix.

### 3. Code

Key conventions:
- Go doc comments on all exported types and functions
- Tests in the same package (`_test` suffix OK for integration)
- Logs use tagged prefixes (`SYNC:`, `PACKER:`, `EVENT:`, `ESCALATION:`)
- Config via CLI flags, not hardcoded paths

### 4. Test

```bash
make test        # unit tests (short mode)
make test-full   # full suite including integration
make lint        # go vet
```

All tests must pass before pushing.

### 5. Commit

```bash
git add <files>
git commit -m "feat: description of change"
```

Follow [Conventional Commits](https://www.conventionalcommits.org/) format. Commits include `Co-authored-by: Alexis Okuwa <wojonstech@gmail.com>` via the global commit template.

### 6. Push and open a PR

```bash
git push origin feat/my-feature
```

Open a PR against `main`. The CI checks build + vet + test. A maintainer will review.

## Project Structure

```
cmd/
  schedulerd/    # Scheduler daemon entry point
  migrate/       # Cron → scheduler migration tool
internal/
  api/           # REST API server (15 endpoints)
  config/        # TOML fleet config loader
  dashboard/     # HTML dashboard (dark theme)
  database/      # SQLite schema, migrations, CRUD
  mcp/           # MCP JSON-RPC server (14 tools)
  scheduler/     # Core engine: urgency, packer, spawner, events, alerts, simulator
  sync/          # DuckBrain read-replica sync
plugin/          # Hermes plugin (Python, /fleet commands)
specs/           # Implementation specs
deploy/          # Systemd units, gateway profile
docs/            # Fleet status, architecture docs, ADRs
```

## Running Integration Tests

The full scheduler can be tested with a simulated fleet:

```bash
./bin/schedulerd --test-verify 3
```

This creates a temp DB, loads 7 test projects, and runs 3 evaluation cycles verifying:
- No hangs
- Full coverage (6/7 projects get ticks)
- Budget capping
- No duplicate ticks
- Session ID assignment
- Priority ordering

## Architecture Decisions

See `docs/adr/` for Architecture Decision Records. ADRs document the *why* behind design choices.

## Documentation

- `README.md` — user-facing overview and quick start
- `docs/fleet.md` — current fleet status, thread mappings, skills map
- `deploy/gateway-setup.md` — dedicated gateway for scheduler ticks
- `fleet.example.toml` — annotated TOML config example

## Questions?

Open a GitHub issue or reach out in the project's Hermes workspace.
