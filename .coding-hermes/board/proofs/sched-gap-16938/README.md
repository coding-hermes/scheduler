# SCHED-GAP-16938 — proxy arm in the cluster chain probe

The chain probe measured every hop except the model-serving ingress. This proof
bundle is the evidence for the arm that closes that hole; it lives in the repo
because `/tmp` dies with the box (SCHED-GAP-16946's lesson).

## What was measured (2026-10-29T19:35Z, before the arm existed)

The classified ingress on `127.0.0.1:9391` answered `/health` with:

```
loaded_commit=13ef7482e1d7   repo_commit=73d4795b0dc9   code.stale=true
```

`git -C ~/task-router rev-list --count 13ef7482e1d7..origin/main -- '*.py'` = **2**.
The ingress had been serving a build two Python commits behind the deploy
lineage, and no arm of the chain probe could say so. A watchdog consumed the
flag (TR-255); the chain's own measurement did not.

## Reproduce

```sh
ops/cluster-chain-probe.sh proxy                       # live: FAIL PROXY_BUILD_DRIFT
ops/cluster-chain-probe.sh proxy --proxy-required      # same, on a host that owns :9391
ops/cluster-chain-probe.sh all --against origin/main   # proxy included in the gating list
ops/cluster-chain-probe-selftest.sh                    # 117 checks, exit 0
```

## Files

| file | what it is |
|---|---|
| `selftest.log` | the full 117-check run on the rebased tree (`SELFTEST: PASS=117 FAIL=0`), including the mutation control |
| `live-proxy.json` | the live ingress arm output: `PROXY_BUILD_DRIFT`, `compiled_behind=2` |
| `live-all.json` | the whole chain with the proxy in the gating list (5 gating arms, connector report-only) |
| `arms.txt` | the arm census: `proxy  yes (reports when unmeasurable)` |

## What the selftest proves (and what it does not)

It proves the arm can fail and can pass, hermetically: a stub serving the tip
PASSes `drift=0`; a stub serving `HEAD~2` FAILs `compiled_behind=2` naming both
commits; a docs-only commit after the deploy PASSes with `compiled_behind=0`
while still printing `behind=1`; a `stale=false` flag over a behind build still
FAILs (`flag_agrees=no`); an unattributable build gives `PROXY_BUILD_UNKNOWN_COMMIT`;
`status=degraded` gives `PROXY_UNHEALTHY`; no build stamp gives `PROXY_NO_COMMIT`
as a REPORT; a closed socket REPORTS by default and FAILs under
`--proxy-required`; `all` against a fresh stub chain exits 0 with `passed:5`.
Neutering the compiled-drift comparison in a throwaway copy turns the stale case
green, so the checks are attributable to that comparison.

It does **not** prove the ingress serves a completion, that a restart is safe, or
that the deployed build is the newest one — only that it is not behind the
deploy lineage on compiled paths.

## Residual (open when this row closed)

The live box reads red on two gating arms, both real deploy gaps this probe
cannot fix by design:

* `proxy` — the ingress is 2 `*.py` commits behind; fix is sync + restart in
  `~/task-router` (`make restart-router`). NEVER let this probe deploy.
* `scheduler` — the daemon runs a build 12+ commits behind `origin/main`
  (owner of the deploy-path work: SCHED-GAP-162).
