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
(cd "$TMP/wt/qq-qa" && sleep 5) & SLEEPER=$!
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
wait $SLEEPER 2>/dev/null

echo
echo "hooks: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
