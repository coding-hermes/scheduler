# Contributing to Coding Hermes

## Development Setup

```bash
git clone https://github.com/coding-herms/scheduler.git
cd scheduler
make build
make test
```

## Code Style

- Go: standard `gofmt` formatting, 88-char line length recommendation
- Imports: standard library → third-party → local
- Tests: one test file per package, `go test` must pass before commit

## Pull Request Process

1. Fork the repo
2. Create a feature branch
3. Write code + tests
4. Run `make build && make test && make lint`
5. Submit PR against `main`
6. CI must pass (build, vet, test, lint)

## Commit Format

```
type: description. Addresses <issue-or-task>.

Types: feat, fix, docs, test, refactor, chore
```

## Architecture

See [`specs/`](specs/) for detailed implementation specs.

Key packages:
- `cmd/schedulerd` — daemon entry point
- `cmd/migrate` — cron → scheduler migration tool
- `internal/scheduler` — urgency, packer, spawn, lifecycle, loop
- `internal/api` — REST API server
- `internal/mcp` — MCP JSON-RPC server
- `internal/dashboard` — HTML dashboard generator
- `internal/database` — SQLite schema, migrations, CRUD
- `internal/sync` — DuckBrain read-replica sync
- `plugin/` — Hermes plugin (Python)

## Skills

The [coding-herms/skills](https://github.com/coding-herms/skills) repo defines the processes this scheduler orchestrates.
