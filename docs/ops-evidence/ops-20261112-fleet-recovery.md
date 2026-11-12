# Fleet ops recovery evidence — 2026-11-12 11:32 tick

Tick: fleet-ops-check-2026-11-12-11-32-05
Project: coding-hermes-scheduler
Published base checked: origin/main 88856d4cf4b22a60fe305f8e7641ff24899e6862

## Recovery

The injected predecessor wave was coding-hermes-scheduler-foreman-2026-11-12-09-41-13. Its manifest declared RELEASE-058 and RELEASE-057. RELEASE-058's original branch tip b6b6606c was already an ancestor of origin/main, so no merge was performed. The original worktree was clean apart from stale board/GitReins residue relative to current main and had no process with that worktree as cwd.

The prior tier-2 verdict e858da71 was read back and failed the acceptance criterion because .github/workflows/ci.yaml and .github/workflows/release.yaml still used 1.26 pins. A fresh code-only fixup was created in worktree /home/kara/worktrees/coding-hermes-scheduler-RELEASE-058-fixup on branch wt/RELEASE-058-fixup. Commit 2358891892e2f66afa191453fe2b4fad7dcfd226 changes only .github/workflows/ci.yaml and .github/workflows/release.yaml, replacing the remaining 1.26 pins with 1.26.11. The original worker branch and the fixup branch were preserved as evidence.

Fixup gate evidence at 23588918: go build ./... rc=0; go vet ./... rc=0; go test ./... -count=1 rc=0; govulncheck v1.7.0 with Go 1.26.11 rc=0 and output No vulnerabilities found. The negative pin scan found no remaining exact 1.26 or go1.26.10 pin (grep returned rc=2 because the negative search had no matches). The fixup branch push initially failed over HTTPS because the OAuth App lacked workflow scope; the same branch was then pushed successfully over SSH to github.com:coding-hermes/scheduler.git as refs/heads/wt/RELEASE-058-fixup.

The fixup was not merged into main in this recovery tick because the branch changes tracked workflow files and the owner-controlled recovery path did not have authorization to land the workflow change through the shared main branch. RELEASE-058 remains pending, and RELEASE-057 was not dispatched; its predecessor wave was closed failed with the blocker recorded in the manifest. No normal dispatch was started after recovery.

## Ops census and live state

Command: python3 ~/.hermes/scripts/fleet_ops_check.py --project coding-hermes-scheduler
Output: OPS CENSUS 1 project families service=1; findings by check: container 1; P2 service with no Dockerfile/compose; exit 1.

The census finding is not actionable as a missing deployment mechanism for this project: the tree is systemd-native. The published tree contains deploy/coding-hermes-scheduler.service and deploy/coding-hermes-scheduler-gateway.service. The live user service was active, schedulerd was listening on 127.0.0.1:9090, and curl -fsS --max-time 5 http://127.0.0.1:9090/health returned HTTP HTML with health_rc=0. No Docker/Compose artifact was created because that would be an invented parallel supervisor, not a bounded correction.

No scheduler restart, lane disable/park, scheduler.db write, or destructive cleanup was performed. A normal dispatch was deliberately left alone because recovery failed on an unmerged workflow fix and the recovery barrier forbids new work in that state.
