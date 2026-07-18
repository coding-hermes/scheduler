# Coding Hermes Skills

Skills for the Coding Hermes fleet — a fleet of AI coding agents managed by a
weight-budget priority scheduler.

These are **template** versions. Before use, fill in the placeholders for your
fleet. Never commit your actual credentials, thread IDs, or email addresses.

## Quick Start

```bash
# 1. Copy the template to your Hermes skills directory
cp skills/templates/coding-hermes-foreman.template.md ~/.hermes/skills/<your-name>/coding-hermes-foreman/SKILL.md

# 2. Fill in placeholders (see below)
# 3. Load the skill in your foreman prompt
hermes chat -s coding-hermes-foreman "Run a foreman tick"
```

## Placeholders

Replace these in every skill before use:

| Placeholder | What to put | Example |
|-------------|------------|---------|
| `{HERMES_HOME}` | Your home directory | `/home/you` |
| `{YOUR_EMAIL}` | Your primary email | `you@gmail.com` |
| `{CO_AUTHOR_EMAIL}` | Co-author email for git | `partner@gmail.com` |
| `{TELEGRAM_GROUP}` | Telegram group chat ID | `-1001234567890` |
| `{PROJECT_THREAD}` | Per-project thread ID | `12345` |
| `{FLEET_NAME}` | Name of your fleet | `my-coding-fleet` |
| `{ORG_NAME}` | GitHub org name | `my-org` |
| `{SCHEDULER_PORT}` | Scheduler daemon port | `9090` |
| `{SCHEDULER_HOST}` | Scheduler daemon host | `127.0.0.1` |

## Available Skills

### Core (required)

| Skill | Size | Description |
|-------|------|-------------|
| `coding-hermes-foreman` | ~110KB | The foreman loop: self-heal, board read, sweep, fix, commit. Runs one tick per project. |
| `coding-hermes-supervisor` | ~60KB | Fleet governance: rebalance priorities, detect orphans, manage cooldowns. Runs on a cron schedule. |
| `coding-hermes-scheduler` | ~22KB | How to operate the scheduler daemon: API, project management, debugging. Loaded by the foreman. |
| `coding-hermes-north-star` | ~28KB | Architecture principles: 3-tier fleet, escalation paths, model rules, provider palette. |

### Supporting

| Skill | Size | Description |
|-------|------|-------------|
| `coding-hermes-cron` | ~32KB | Deprecated — cron-based foreman docs. Reference only. Foremen now use the scheduler. |
| `coding-hermes-goimports-pitfall` | ~6KB | Go-specific: local import prefixes that break builds. |
| `coding-hermes-adhoc-verification` | ~8KB | Script pattern for ad-hoc verification in foreman ticks. |
| `coding-hermes-contradictory-tests` | ~4KB | How to handle tests that contradict user requirements. |
| `coding-hermes-containers` | ~3KB | Container patterns for foreman-managed services. |
| `client-demo-delivery` | ~5KB | How to format foreman output for client demo delivery. |

## Sanitizing Your Skills

If you already have skills with real data, use the sanitizer:

```bash
# From the scheduler repo
./scripts/sanitize-skills.sh ~/.hermes/skills/coding-hermes* skills/templates/
```

This strips emails, home paths, thread IDs, and replaces them with placeholders.
