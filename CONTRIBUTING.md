# Contributing

## Development

```bash
git clone https://github.com/coding-herms/scheduler.git
cd scheduler
make build
make test
```

## Architecture

The scheduler is a single Go binary. It manages a SQLite database (WAL mode) and exposes HTTP endpoints for REST, MCP, and a dashboard.

### Package Map

```
cmd/schedulerd/       Entry point — wires everything
cmd/migrate/          Bootstrap — imports from Hermes jobs.json
internal/database/    SQLite schema, migrations, CRUD
internal/scheduler/   Core loop: urgency, packer, spawner, lifecycle
internal/api/         REST API handlers
internal/mcp/         MCP protocol server
internal/dashboard/   HTML dashboard generation
internal/sync/        DuckBrain read-replica sync
```

### Key Concepts

- **Weight-budget scheduling**: Projects consume budget (1-100) from a fixed pool. The packer greedily fits the most urgent projects.
- **Geometric intervals**: Priority→interval mapping is exponential, not linear.
- **Platform architecture**: The scheduler is a platform, not a plugin. Any HTTP client can query or control the fleet.

## Testing

```bash
make test        # Short tests
make test-full   # All tests including integration
```

Tests use `:memory:` SQLite databases. No external dependencies.

## Commit Style

```
type: description

- feat: new feature
- fix: bug fix
- refactor: code restructuring
- test: test additions/changes
- docs: documentation
- chore: maintenance
```

## Pull Requests

1. Fork and branch from `main`
2. Add tests for new functionality
3. Ensure `make build`, `make lint`, and `make test` pass
4. Open PR against `main`
