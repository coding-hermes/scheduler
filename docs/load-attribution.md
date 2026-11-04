# Host load attribution and the heavy-job policy

`GET /api/v1/status` and `GET /api/v1/health` stay cheap. What does not stay
cheap is the host they run on, and the scheduler's own admission path is the
first thing that suffers when it is saturated.

This page defines two things: **who is holding the host** (a probe) and **how
many heavy jobs a host may run at once** (a policy). Both exist because the load
gate is correct but blind — it can refuse every candidate without being able to
say why, and an operator staring at `ADMIT ... load_gate` for 677 candidates has
no way to tell "a legitimate verify lane is running" from "the box is wedged".

## The gate is a ceiling, not a target

`--load-gate-threshold` (default 20, published as `load_gate_threshold` on
`/api/v1/config`) is the load1 above which **no tick is admitted**. It is not a
budget to spend: a host that sits at 19 admits ticks but schedules them into a
machine that has no headroom left, and a host that crosses 20 stops working for
the fleet entirely.

Measured on this box (8 vCPU, shared), 2026-11-03:

```
20:59  load1=33.12  gate=20.0  ADMIT deferring all 677 candidates
21:20  load1=58.47  three unrelated heavy jobs + dockerd
         auger 9-suite pytest gate        cwd /home/kara/auger
         crier lane: go test ./... -count=1  cwd /home/kara/crier-wt-…
         xz -T4 rehearsal, 42 GB dump     cwd /mnt/bulk/rehearsal-full
```

None of those three was wrong to run. Running them **together** was. An
additional hazard: earlyoom is armed with a preference list that includes
`pytest`, `go`, `cargo` and `rustc`, so the scheduler's own verification can be
killed mid-run by the kernel's OOM policy while a compression job holds memory.

## Policy

**At most ONE heavy job per host at a time.**

A heavy job is a process matching any of these classes *and* holding at least
25 % CPU:

| class | examples |
|---|---|
| `test` | `pytest`, `go test`, `vitest`, `jest` |
| `build` | `go build`, `cargo`/`rustc`, `tsc`, `webpack`, `make`, `cmake`, `ninja` |
| `compress` | `xz`, `pigz`, `gzip`, `zstd`, `tar -` |
| `image` | `docker build`, `buildx`, `compose build` |
| `install` | `uv sync`, `pip install`, `npm ci`/`install`, `pnpm`, `yarn install` |

The 25 % floor exists so a stray `grep` or a shell that happens to mention
`pytest` in its argv is not counted as a job. The list is intentionally
command-shaped rather than owner-shaped: the probe must not care who started the
work.

Coordination is by **checking before starting**, not by a central lock. There is
no daemon that can refuse you: run the probe, and if it reports a violation, wait
or move your job to another host.

## The probe

```bash
ops/load-attribution.sh                 # human summary, exit 1 on violation
ops/load-attribution.sh --json          # machine shape
ops/load-attribution.sh --limit 2       # grade a host that is allowed two
```

It is **read-only**: it reads `/proc`, `ps`, and the scheduler HTTP API, and
writes nothing, signals nothing, kills nothing.

Exit codes: `0` within policy (including idle / merely busy), `1` violation
(more concurrent heavy jobs than the limit, or the gate is shut *and* a heavy job
is named as the holder), `2` cannot determine (no gate value, or no process
table).

Each named job carries its pid, class, CPU, elapsed time, working directory, a
**digest** of its command line (never the raw argv — probe output is a replayable
surface and a full command line can carry a token), and the **lane** whose
workdir contains that directory. Attribution matches the longest workdir prefix,
so a worktree nested inside a project still reports its own row.

Sample — the SHAPE of a report, with the 21:20 incident's jobs filled in for
readability. Lane names appear only when the scheduler API answers; the selftest
runs against a closed port on purpose, and a live run today (calm host) prints
`heavy jobs : 0`. Do not read the numbers below as a captured run:

```
host load1 : 58.47   gate: 20   gate state: shut
heavy jobs : 3 (policy limit 1 per host — docs/load-attribution.md)

  pid 2001    test     cpu=95  up=900s   lane=auger        cwd=/home/kara/auger
           cmd=f6644f1b… python -m pytest -q tests/
  pid 2002    test     cpu=88  up=40s    lane=crier        cwd=/home/kara/crier-wt-t20261104-close
           cmd=3e27f65b… go test ./... -count=1
  pid 2003    compress cpu=70  up=1800s  lane=(unattributed) cwd=/mnt/bulk/rehearsal-full
           cmd=37e1fd73… xz -T4 -6 …/full.sql

verdict    : VIOLATION — 3 concurrent heavy jobs, policy allows 1
```

## What the probe cannot see

Stated plainly so the report is not over-trusted:

- **Containers.** A `docker build` running inside a container shows up as
  `dockerd`/`buildkitd`, not as the compiler actually burning the CPU.
- **Short bursts.** A job that peaks above 25 % for ten seconds and then idles
  may be missed by a single sample. The probe is a snapshot, not an average.
- **Unreadable processes.** Without permission for `/proc/<pid>/cwd` the lane is
  reported as `(unattributed)` and the cwd as `(unknown)` — the count and the
  classification are still right.
- **Load that is not a process.** Kernel threads, NFS stalls and disk wait can
  raise load1 with no heavy job present. In that case the probe reports
  `heavy jobs : 0` and says so; a shut gate with zero named holders is an
  infrastructure question, not a scheduling one.

## Related

- `SCHED-GAP-16977` — the row that produced this page.
- `ops/load-attribution-selftest.sh` — proves the probe grades a host instead of
  printing a happy line (17 checks, including a mutated copy of the probe that
  must fail its own claim 2).
- `SCHED-GAP-16976` — `/api/v1/status` degrades to a named `partial` instead of
  504-ing under load, so this probe is readable while the host is busy.
