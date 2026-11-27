# bunker-qa battery analysis — dogfood 2026-11-27 (tick coding-hermes-scheduler-dogfood-2026-11-27-20-13-36)

Launch: 2026-11-27 15:30:58 local (proc 1052255), server bunker-las-02 -> host bunker-mvp, agent 4f5caa42, TTL 4h.
Launcher summary: Passed 14 | Failed 1 | Audited 15 | Findings 1 | NotVerified 2. Teardown: agent destroyed
(destroy initially reported rc=1 but the agent is confirmed GONE from `bunker list` — teardown recovered).

## Tree binding (the DOGFOOD-033 correction's identity requirement)
- target-binding cell: verdict=bound, repo-identity=scheduler (source=origin-remote, remote github.com/coding-hermes/scheduler).
- The battery synced the PARKED shared checkout (bunker-qa refuses worktree paths by design). At launch the parked
  checkout was HEAD 733fe05b with a CLEAN tracked tree (only untracked: .gitreins/review-state.json, board.db) —
  proven by `git status` before launch and by launch time (15:30:58) preceding this fire's first commit (4386da38 @ 15:47).
- KNOWN HARNESS GAP (unchanged): the ledger rows carry NO product_sha/commit field (keys: project, cell, status, detail, ts),
  so the binding above is provenance-by-argument, not a stamped field. A ledger product_sha field would close this class.

## Cell outcomes
- fresh-install OK — toolchain bootstrap (go 1.27.4 user-space, gcc wrappers, make, jq), `go build ./...` green.
- lint OK — golangci-lint rc=0 (QA-CHS-190's unavailable-tool premise did not fire this run).
- tests OK — NATIVE full suite `go test ./... -count=1` rc=0 on the bunker box. Includes internal/scheduler
  (schedgap1666_test.go is untagged in-tree), so DOGFOOD-033's TestSCHEDGAP1666_SevenDaySimulation did NOT fail here.
- upgrade OK — previous release v1.15.2 tree -> HEAD reinstall clean.
- ci-pass FAIL — act rc=1. Three tests fail inside the act container across three jobs (CI/race-blocks, CI/build,
  CI/race-scheduler): TestJSONL_ReadOnlyParentDir (0.01s), TestTimeoutKillsProcessGroup (4.06s),
  TestSpawnTimeoutKillsProcessGroup (3.24s). NATIVE suite is GREEN on the same tree and box — this is environment
  divergence (act container runs privileged/root: permission-bit enforcement and process-group kill semantics differ),
  not a product regression. The act log interleaves [multiarch/Go Multi-Arch/Verify] output between jobs.
- ci-act UNVERIFIED — the second act arm produced no verdict (first act already failed).
- docker-deploy SKIPPED — sanctioned safety gate: controller is the live Hermes host; Compose lifecycle forced off.
- ui-probe INFO — go-native server ran 20s, none of the probed ports [3000 5173 8000 8080 8081 8766 8767 9000]
  answered — corroborates pending DOGFOOD-034 (probe list omits the product's documented default port 9090).

## Verdict on the install leg
INSTALLED AND WORKING on a clean machine by the documented path (build, lint, native tests, upgrade reinstall all green),
target-bound to repo identity `scheduler` at parked-checkout state 733fe05b. The single FAIL is the act CI-simulation
cell whose failure mode is owned by pending HARNESS rows QA-CHS-201/203/204 (act-in-container class). DOGFOOD-033's
fresh-install-red premise does NOT reproduce on a target-bound battery (row kept pending — its own text scopes the
failure as load-sensitive, and this box was quiet; the audit event records the non-reproduction).
