# ops/pm-standin — PM stand-in live scripts

Tracked, versioned copies of the PM stand-in driver, its role-report
helper, and the board/ledger reconciler. The live scripts under
`~/.hermes/scripts/` and `~/.hermes/stand-in/` are symlinks into this
directory — **the tracked files are the source of truth**: normal future
edits happen here, get committed, and flow to the live scripts through the
symlinks. Untracked live edits are never copied upstream.

## What lives here

| File | Live destination | Role |
|------|------------------|------|
| `pm-standin-tick.sh` | `~/.hermes/scripts/pm-standin-tick.sh` | The PM stand-in driver. The tracked copy IS the live file. Edit and commit here; do not edit the live path. |
| `dagger-role-report.py` | `~/.hermes/scripts/dagger-role-report.py` | The role-report helper (invoked by the driver, line 320). The tracked copy IS the live file. |
| `ledger_board_reconcile.py` | `~/.hermes/stand-in/ledger_board_reconcile.py` | Ledger ↔ board reconciler (board task GAP-047). Stdlib-only, **defaults to dry-run**; `--apply` mutates ledger state — see the warning below. The tracked copy IS the live file. |
| `install.sh` | (not deployed) | Idempotent installer — symlinks the three live paths to the tracked copies, backing up any pre-existing regular files first. |

## The reconciler is dry-run by default — `--apply` mutates ledger state

`ledger_board_reconcile.py` reads the stand-in ledger
(`~/.hermes/stand-in/ledger.json`), resolves each item's project to its
board via `scheduler.db` (opened read-only), and — with `--apply` — flips
non-terminal ledger items to `verified` when their board row is complete.
Behavior:

- **Default (no flags): dry-run.** Prints what would change, per-project
  counts, and totals; exits 0; writes nothing.
- **`--apply`: mutates the ledger.** Copies `ledger.json` to a timestamped
  backup, then atomically rewrites the ledger (tmp file + `os.replace`).

Only three ledger fields are ever touched by `--apply` (status,
last_checked_at, verification_evidence). Ops tasks in this repository must
NOT run `--apply` — verify with the default dry-run only.

## Install

```sh
bash /home/kara/coding-hermes-scheduler/coding-herms-scheduler/ops/pm-standin/install.sh
```

The installer only touches the three live script paths under `$HERMES_HOME`
(default `~/.hermes`), their timestamped backups, and the two directories
that contain them (`scripts/` and `stand-in/`; `stand-in/` is created if
missing). No credentials, state, or other files under `~/.hermes` are read,
printed, or modified — `ledger.json` and `scheduler.db` are never opened by
the installer. Payload syntax is validated before any live path is touched
(`bash -n` for the driver, `py_compile` with the bytecode cache in a temp
file outside the repository for both Python payloads).

## Rollback

**Rule first: never `cp` a backup onto a live path while that path is still a
symlink.** `cp` follows destination symlinks by default, so
`cp -p <backup> ~/.hermes/scripts/pm-standin-tick.sh` would write THROUGH the
symlink into the tracked repository file, silently overwriting version
history. Remove the symlink first, then restore as a regular file:

```sh
# 1. See which backups exist (only created if a pre-existing regular file
#    was replaced by the symlink; none may exist on this host):
ls -la ~/.hermes/scripts/pm-standin-tick.sh.bak-* \
       ~/.hermes/scripts/dagger-role-report.py.bak-* \
       ~/.hermes/stand-in/ledger_board_reconcile.py.bak-* 2>/dev/null

# 2. Remove the LIVE SYMLINK (rm on a symlink never touches the target):
rm ~/.hermes/scripts/pm-standin-tick.sh
cp -p ~/.hermes/scripts/pm-standin-tick.sh.bak-YYYYmmdd-HHMMSS \
      ~/.hermes/scripts/pm-standin-tick.sh

# 3. Same sequence for the helper:
rm ~/.hermes/scripts/dagger-role-report.py
cp -p ~/.hermes/scripts/dagger-role-report.py.bak-YYYYmmdd-HHMMSS \
      ~/.hermes/scripts/dagger-role-report.py

# 4. Same sequence for the ledger reconciler:
rm ~/.hermes/stand-in/ledger_board_reconcile.py
cp -p ~/.hermes/stand-in/ledger_board_reconcile.py.bak-YYYYmmdd-HHMMSS \
      ~/.hermes/stand-in/ledger_board_reconcile.py
```

Generic form for any payload (replace `<live>` and `<backup>`):

```sh
rm <live>                       # remove the symlink — must be done FIRST
cp -p <backup> <live>           # backup lands as a REGULAR file
```

`install.sh` is idempotent, NOT a rollback: re-running it re-creates the
symlinks pointing at the tracked repo copies. If that is the state you want
(after checking that the tracked copy is correct), just run `install.sh`
again. **Future edits happen in this tracked directory and are committed
here — never as untracked live edits copied upstream.**

While a live path is a symlink into this repo, everything under the
`ops/pm-standin/` payloads is protected by version control: even an
accidental write through the symlink shows up in `git diff` and is
recoverable from git.

## Verify

```sh
# All three live paths are symlinks into this repository:
readlink -f ~/.hermes/scripts/pm-standin-tick.sh
readlink -f ~/.hermes/scripts/dagger-role-report.py
readlink -f ~/.hermes/stand-in/ledger_board_reconcile.py

# Tracked copy == live file for all three payloads:
cmp -s ops/pm-standin/pm-standin-tick.sh ~/.hermes/scripts/pm-standin-tick.sh && echo driver-ok
cmp -s ops/pm-standin/dagger-role-report.py ~/.hermes/scripts/dagger-role-report.py && echo helper-ok
cmp -s ops/pm-standin/ledger_board_reconcile.py ~/.hermes/stand-in/ledger_board_reconcile.py && echo reconciler-ok

# Payload hashes (driver 86a2e15d…, helper 5975cc4a…, reconciler 14c88afc…):
sha256sum ops/pm-standin/pm-standin-tick.sh \
          ops/pm-standin/dagger-role-report.py \
          ops/pm-standin/ledger_board_reconcile.py

# Reconciler sanity check — DEFAULT DRY-RUN (never run --apply from ops):
python3 ~/.hermes/stand-in/ledger_board_reconcile.py
```

## Scheduler command paths stay unchanged

Scheduler project rows intentionally keep running the **live path**, not the
repo path:

```
bash /home/kara/.hermes/scripts/pm-standin-tick.sh
```

Because the live path is a symlink into this repository, the scheduler keeps
working unchanged while the payload becomes version-controlled here.

## Current live premise (honest status)

`~/.hermes/coding-hermes/scheduler.db` currently has **42 rows** whose
`command` references that live path, and **all 42 are disabled**. This task is
durability hardening of a dormant-but-referenced path — not an active outage
fix. Nothing in this change enables any scheduler row or runs the PM
pipeline (a full PM tick run takes 13–20 minutes and is deliberately NOT
exercised here). The ledger reconciler is likewise only exercised in its
default dry-run mode here; `--apply` (which mutates ledger state) is an
explicit operator action outside these ops tasks.
