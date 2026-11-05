#!/bin/bash
# test-hooks.sh — exercises the S14 lane-worktree hook scripts against a
# throwaway repo. Nothing here touches a fleet repository.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }
check(){ if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 (rc=$1 want=$2)"; fi; }

# ---- fixture: bare origin + working clone -------------------------------
git init -q --bare "$TMP/origin.git"
git clone -q "$TMP/origin.git" "$TMP/repo"
cd "$TMP/repo"
git config user.email t@example.com; git config user.name t
echo one > file.txt; git add .; git commit -qm one
git branch -M main; git push -q -u origin main
cd - >/dev/null

WT="$TMP/wt/cf-qa"

# ---- W1: create ---------------------------------------------------------
out=$("$HERE/worktree-ensure.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" 2>&1); rc=$?
check $rc 0 "W1 ensure creates the tree"
case "$out" in *"branch=lane/cf/qa"*) ok "W1 names the lane branch" ;; *) bad "W1 branch line: $out" ;; esac

# ---- W2: idempotent -----------------------------------------------------
a=$("$HERE/worktree-ensure.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" 2>&1)
b=$("$HERE/worktree-ensure.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" 2>&1)
[ "$a" = "$b" ] && ok "W2 second run is byte-identical" || bad "W2 not idempotent"

# ---- W3: dirty tree is preserved ---------------------------------------
echo wip > "$WT/wip.txt"
out=$("$HERE/worktree-ensure.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" 2>&1)
[ -f "$WT/wip.txt" ] && ok "W3 dirty work survives" || bad "W3 dirty work was destroyed"
case "$out" in *dirty_preserved*) ok "W3 reports dirty_preserved" ;; *) bad "W3 no dirty flag: $out" ;; esac

# ---- W11/W13/W15: commit, merge, prune ---------------------------------
git -C "$WT" add -A >/dev/null 2>&1; git -C "$WT" commit -qm "lane work"
out=$("$HERE/lane-merge.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" --push 1 2>&1); rc=$?
check $rc 0 "W11 merge lands"
case "$out" in *"merge=landed"*) ok "W11 receipts landed" ;; *) bad "W11 receipt: $out" ;; esac
git -C "$TMP/repo" log --oneline origin/main | grep -q "lane work" && ok "W11 commit is on origin/main" || bad "W11 commit missing"

out=$("$HERE/lane-merge.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" --push 0 2>&1); rc=$?
check $rc 10 "W13 zero commits exits 10"
case "$out" in *no_commits*) ok "W13 reports no_commits" ;; *) bad "W13 receipt: $out" ;; esac

out=$("$HERE/worktree-prune.sh" --lane cf-qa --repo "$TMP/repo" --worktree "$WT" --merged 1 2>&1); rc=$?
check $rc 0 "W15 prune after landed"
[ -d "$WT" ] && bad "W15 tree still present" || ok "W15 tree removed"

# ---- W12: conflicting merge is refused and preserved -------------------
git -C "$TMP/repo" checkout -q main
printf 'base\n' > "$TMP/repo/conflict.txt"; git -C "$TMP/repo" add .; git -C "$TMP/repo" commit -qm base; git -C "$TMP/repo" push -q origin main
"$HERE/worktree-ensure.sh" --lane cx-docs --repo "$TMP/repo" --worktree "$TMP/wt/cx-docs" >/dev/null 2>&1
printf 'lane-side\n' > "$TMP/wt/cx-docs/conflict.txt"
git -C "$TMP/wt/cx-docs" add conflict.txt; git -C "$TMP/wt/cx-docs" commit -qm lane-side
printf 'base-side\n' > "$TMP/repo/conflict.txt"; git -C "$TMP/repo" add conflict.txt; git -C "$TMP/repo" commit -qm base-side; git -C "$TMP/repo" push -q origin main
out=$("$HERE/lane-merge.sh" --lane cx-docs --repo "$TMP/repo" --worktree "$TMP/wt/cx-docs" 2>&1); rc=$?
check $rc 65 "W12 conflict exits 65"
[ -d "$TMP/wt/cx-docs" ] && ok "W12 worktree preserved on conflict" || bad "W12 worktree vanished"

# ---- W5: branch in use elsewhere ---------------------------------------
git -C "$TMP/repo" worktree add -q "$TMP/wt/other" lane/cx/docs 2>/dev/null || true
out=$("$HERE/worktree-ensure.sh" --lane cx-docs --repo "$TMP/repo" --worktree "$TMP/wt/cx-docs2" 2>&1); rc=$?
check $rc 75 "W5 branch-in-use exits 75"

# ---- W6: disk floor -----------------------------------------------------
out=$("$HERE/worktree-ensure.sh" --lane df-qa --repo "$TMP/repo" --worktree "$TMP/wt/df-qa" --floor-gib 999999 2>&1); rc=$?
check $rc 78 "W6 disk floor exits 78"

# ---- quiesce ------------------------------------------------------------
"$HERE/worktree-ensure.sh" --lane qq-qa --repo "$TMP/repo" --worktree "$TMP/wt/qq-qa" >/dev/null 2>&1
"$HERE/quiesce-check.sh" --worktree "$TMP/wt/qq-qa" >/dev/null 2>&1; check $? 0 "quiesce on an idle tree"
# The probe scans every /proc/<pid>/{cwd,fd} entry, so on a busy host its own
# scan can take a couple of seconds. The fixture process must outlive the scan
# (a 5s sleeper on a 2k-process box flaked exactly that way), and it is killed
# rather than waited on so the battery does not pay for the extra time.
(cd "$TMP/wt/qq-qa" && sleep 30) & SLEEPER=$!
# wait until the sleeper is actually resident in the tree (a fixed sleep races it)
for _ in $(seq 30); do
  found=0
  for pr in /proc/[0-9]*; do
    [ "$(readlink "$pr/cwd" 2>/dev/null)" = "$(cd "$TMP/wt/qq-qa" && pwd -P)" ] && found=1 && break
  done
  [ "$found" = 1 ] && break
  sleep 0.1
done
"$HERE/quiesce-check.sh" --worktree "$TMP/wt/qq-qa" >/dev/null 2>&1; check $? 10 "quiesce detects a live process"
kill "$SLEEPER" 2>/dev/null || true
wait "$SLEEPER" 2>/dev/null || true

# ---- STORY: the branch names the PROJECT, not a split of the lane name ------
# Before this, `code-factory-qa` produced lane/code/factory/qa: the project
# `code-factory` was cut in half at its own dash. The rule now mirrors
# lanehook.RoleFor: lane/<project>/<role>.
out=$("$HERE/worktree-ensure.sh" --lane code-factory-qa --repo "$TMP/repo" --worktree "$TMP/wt/cfqa" 2>&1); rc=$?
check $rc 0 "S1 ensure accepts a dashed project"
case "$out" in *"branch=lane/code-factory/qa"*) ok "S1 branch keeps the project's dash" ;; *) bad "S1 branch line: $out" ;; esac
case "$out" in *"lane/code/factory/"*) bad "S1 branch still split the project at its dash" ;; *) ok "S1 no fabricated project segment" ;; esac

out=$("$HERE/worktree-ensure.sh" --lane code-factory-ops --repo "$TMP/repo" --worktree "$TMP/wt/cfops" 2>&1)
case "$out" in *"branch=lane/code-factory/ops"*) ok "S2 sibling lane gets its own branch under the same project" ;; *) bad "S2 sibling branch: $out" ;; esac

out=$("$HERE/worktree-ensure.sh" --lane ai-plays-poke-qa --repo "$TMP/repo" --worktree "$TMP/wt/aipp" 2>&1)
case "$out" in *"branch=lane/ai-plays-poke/qa"*) ok "S3 a two-dash project survives intact" ;; *) bad "S3 aipp branch: $out" ;; esac

out=$("$HERE/worktree-ensure.sh" --lane qa --repo "$TMP/repo" --worktree "$TMP/wt/alone" 2>&1)
case "$out" in *"branch=lane/qa/primary"*) ok "S4 a lane that IS the role keeps its name as the project" ;; *) bad "S4 role-only lane: $out" ;; esac

# The lane name alone would derive lane/cfq/primary; the explicit pair must win.
out=$("$HERE/worktree-ensure.sh" --lane cfq --project other-proj --role qa --repo "$TMP/repo" --worktree "$TMP/wt/explicit" 2>&1)
case "$out" in *"branch=lane/other-proj/qa"*) ok "S5 explicit --project/--role are honoured verbatim" ;; *) bad "S5 explicit branch: $out" ;; esac

out=$("$HERE/worktree-ensure.sh" --lane "bad/lane-qa" --repo "$TMP/repo" --worktree "$TMP/wt/bad" 2>&1); rc=$?
check $rc 65 "S6 a slash in the lane name is refused, not silently mangled"

# ---- STORY: merge finds the branch the ensure step created ------------------
( cd "$TMP/wt/cfqa" && echo two > two.txt && git add . && git -c user.email=t@example.com -c user.name=t commit -qm two )
out=$("$HERE/lane-merge.sh" --lane code-factory-qa --repo "$TMP/repo" --worktree "$TMP/wt/cfqa" --push 1 2>&1); rc=$?
check $rc 0 "S7 merge lands a privileged-project lane"
case "$out" in *"merge=landed"*) ok "S7 merge reports landed" ;; *) bad "S7 merge output: $out" ;; esac
git -C "$TMP/repo" log --format=%s origin/main | grep -qx two && ok "S7 commit is on origin/main" || bad "S7 commit missing from origin/main"

# ---- STORY: the prune step speaks the same branch rule as ensure/merge ------
# It was the THIRD copy of lane/<lane>-with-slashes, so it looked for
# lane/code/factory/qa on a tree that lives on lane/code-factory/qa. It also
# rejected --project/--role outright, which is what a copy of the example
# config passes — the POST chain failed at its last step, where an on_failure=open
# step is least noticed.
out=$("$HERE/worktree-prune.sh" --lane code-factory-qa --project code-factory --role qa --repo "$TMP/repo" --worktree "$TMP/wt/cfqa" --policy persistent --merged 0 2>&1); rc=$?
check $rc 0 "S8 prune accepts --project/--role (the example config's argv)"
case "$out" in *"prune=kept"*) ok "S8 an unmerged tree is kept, not pruned" ;; *) bad "S8 prune output: $out" ;; esac
[ -d "$TMP/wt/cfqa" ] && ok "S8 the kept tree is still on disk" || bad "S8 prune removed an unmerged tree"
out=$("$HERE/worktree-prune.sh" --lane code-factory-qa --project code-factory --role qa --repo "$TMP/repo" --worktree "$TMP/wt/cfqa" --policy persistent --merged 1 2>&1); rc=$?
check $rc 0 "S9 prune removes a proved-landed tree"
[ -d "$TMP/wt/cfqa" ] && bad "S9 landed tree still present" || ok "S9 tree pruned after landed"
case "$out" in
  *"prune=branch-removed"*"proven-merged-to=main"*) ok "S9 the merged-but-unpushed branch is removed after proving ancestry" ;;
  *) bad "S9 branch removal output: $out" ;;
esac
git -C "$TMP/repo" branch --list 'lane/code-factory/qa' | grep -q . && bad "S9 lane branch still exists" || ok "S9 lane branch gone"
out=$("$HERE/worktree-prune.sh" --lane code-factory-qa --project code-factory --role qa --repo "$TMP/repo" --worktree "$TMP/wt/cfqa" --policy persistent --merged 1 2>&1); rc=$?
check $rc 0 "S10 a second prune is harmless"
case "$out" in *"nothing-to-remove"*) ok "S10 a prune with no tree says so instead of claiming a removal" ;; *) bad "S10 second prune output: $out" ;; esac
# With push=0 a lane is merged to LOCAL main and absent from origin/main; git's
# `branch -d` compares against the upstream and calls that "not fully merged".
# The prune proves ancestry against the base ref instead, and says which ref the
# branch is missing from when it really is unmerged. (Covered by S10's sibling
# below: the first prune of a merged-but-unpushed lane removes the branch.)

# ---- STORY: merge names the branch it lands on ------------------------------
# Measured failure: the fixture repo sat on `master` with base `main`; the merge
# advanced MASTER and the next run reported "ff-only blocked". A hook that lands
# a lane somewhere nobody asked for is worse than one that refuses.
git -C "$TMP/repo" checkout -q master 2>/dev/null || git -C "$TMP/repo" checkout -q -b master origin/main
git -C "$TMP/repo" checkout -q -B main origin/main
( cd "$TMP/wt/side" && echo side > side.txt && git add . && git -c user.email=t@example.com -c user.name=t commit -qm side ) 2>/dev/null || true
out=$("$HERE/worktree-ensure.sh" --lane side-qa --repo "$TMP/repo" --worktree "$TMP/wt/side2" 2>&1)
( cd "$TMP/wt/side2" && echo s2 > s2.txt && git add . && git -c user.email=t@example.com -c user.name=t commit -qm s2 )
git -C "$TMP/repo" checkout -q master
out=$("$HERE/lane-merge.sh" --lane side-qa --repo "$TMP/repo" --worktree "$TMP/wt/side2" --push 0 2>&1); rc=$?
case "$rc:$out" in
  0:*"merge=landed"*) ok "S11 merge from a repo on another branch switches to the base branch and lands" ;;
  *) bad "S11 merge from a foreign branch: rc=$rc out=$out" ;;
esac
case "$(git -C "$TMP/repo" rev-parse --abbrev-ref HEAD)" in
  main) ok "S11 the repo ends on the base branch" ;;
  *) bad "S11 repo left on $(git -C "$TMP/repo" rev-parse --abbrev-ref HEAD)" ;;
esac
git -C "$TMP/repo" log --format=%s main | grep -qx s2 && ok "S11 the lane commit is on main, not on master" || bad "S11 lane commit missing from main"
git -C "$TMP/repo" log --format=%s master | grep -qx s2 && bad "S11 the lane commit landed on master" || ok "S11 master untouched"

echo
echo "hooks: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
