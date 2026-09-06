---
name: Release check (pre-push gate)
about: Verify a fresh clone of the public repo before trusting the README Quick Start, and gate any push to main on human review.
title: "release-check: <branch-or-commit>"
labels: [release, human-gated]
assignees: ''
---

## Release check — fresh-clone verification + human-gated push

Run this before trusting the README Quick Start on the public repo, and before any push to `main`. **Agents never auto-push — a human reviews and pushes.**

- [ ] **1. Fresh clone into a clean dir** — clone the public repo into a clean directory (no inherited state):

      ```bash
      rm -rf /tmp/release-check && git clone https://github.com/coding-hermes/scheduler.git /tmp/release-check && cd /tmp/release-check
      ```

- [ ] **2. Tests must pass** — `make test` must pass; also run `go test ./...` (full suite, no `-short`):

      ```bash
      make test
      go test -count=1 ./...
      ```

- [ ] **3. Verify CI/commit state** — confirm the branch is up to date with `origin/main`, the CI workflow is green on the exact commit being shipped, and the SHA reviewed is the SHA pushed:

      ```bash
      git fetch origin
      git log --oneline -1 origin/main
      git status --short --branch
      ```

- [ ] **4. HUMAN-GATED PUSH** — a human reviews and pushes; agents never auto-push. No agent may run `git push` for this release. If an agent prepared the branch, it is left un-pushed by design.

## Push handoff (GAP-042)

Exactly what a human must do to ship:

1. **Review the branch diff** — `git diff origin/main...<branch>` (or review the PR's diff).
2. **Re-run the gate in a fresh clone** — repeat steps 1–3 above against the branch, not just `main`.
3. **Push and merge** — push the branch (`git push origin <branch>` or the PR head), then merge to `main` via PR/merge once CI is green.
4. **No agent pushes this branch.** The branch was prepared agent-side and left un-pushed on purpose; only a human executes the push.
