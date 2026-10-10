# Sourced by inside.sh after lifecycle.sh, which set up origin, ~/dev/$R and
# ~/seed: `worktree` opens a worktree for the caller's job, and `job end`
# removes the ones recorded for the job. No agent of the job is started; the
# binary runs from this shell with a job's identity variables, and the jobs
# have no row in the jobs table (`job end --force` needs none).
in_job() { local job=$1; shift; as "$job-a" worker "$job-lead" "$job" -- "$@"; }
wt_rows() { ledger "SELECT path, repo, branch, job, created_by, removed_at IS NULL FROM worktrees WHERE job = '$1' ORDER BY id"; }

git -C /home/agent/seed commit -q --allow-empty -m "newer for worktree"
git -C /home/agent/seed push -q origin main
stale=$(git -C "/home/agent/dev/$R" rev-parse HEAD)
out=$(in_job item-6 worktree "$R" 2>/home/agent/worktree.err); rc=$?
check "worktree: exit 0" 0 "$rc"
[ "$rc" = 0 ] || cat /home/agent/worktree.err
check "worktree: stdout is only the path" "$WT/item-6" "$out"
check "worktree: branch named after the job, no prefix" item-6 "$(git -C "$WT/item-6" branch --show-current 2>&1)"
check "worktree: branched from origin's latest, not the stale local HEAD" \
  "$(git -C "/home/agent/remote/$R.git" rev-parse main) not $stale" \
  "$(git -C "$WT/item-6" rev-parse HEAD) not $stale"
check "worktree: branch tracks nothing" "" \
  "$(git -C "/home/agent/dev/$R" for-each-ref --format='%(upstream)' refs/heads/item-6)"
check "worktree: ledger records it for the job" "$WT/item-6|$R|item-6|item-6|item-6-a|1 " "$(wt_rows item-6)"
check "worktree: created_at is now" "1 " \
  "$(ledger "SELECT abs(created_at - strftime('%s', 'now')) < 120 FROM worktrees WHERE job = 'item-6'")"

out=$(as item-6-lead lead thread-1 item-6 -- worktree "$R" 2>&1); rc=$?
check "worktree: again in the same job, exit 0 with the same path" "0 $WT/item-6" "$rc $out"
check "worktree: again in the same job, no new row" "1 " "$(ledger "SELECT count(*) FROM worktrees WHERE job = 'item-6'")"

out=$(in_job item-6 worktree "$R" --name review --branch fix/item-6 2>&1); rc=$?
check "worktree --name --branch: exit 0 with <job>-<name>" "0 $WT/item-6-review" "$rc $out"
check "worktree --branch: on the given branch" fix/item-6 "$(git -C "$WT/item-6-review" branch --show-current 2>&1)"

# --detach: a detached checkout of a commit that is on origin only, no branch.
git -C /home/agent/seed commit -q --allow-empty -m "a PR head"
git -C /home/agent/seed push -q origin main:refs/heads/pr-head
head=$(git -C /home/agent/seed rev-parse HEAD)
out=$(in_job item-6 worktree "$R" --name audit --detach "$head" 2>&1); rc=$?
check "worktree --detach: exit 0 with <job>-<name>" "0 $WT/item-6-audit" "$rc $out"
check "worktree --detach: HEAD at the commit, on no branch" "$head|" \
  "$(git -C "$WT/item-6-audit" rev-parse HEAD 2>&1)|$(git -C "$WT/item-6-audit" branch --show-current 2>&1)"
check "worktree --detach: ledger records it for the job with no branch" "$WT/item-6-audit|$R||item-6|item-6-a|1 " \
  "$(ledger "SELECT path, repo, branch, job, created_by, removed_at IS NULL FROM worktrees WHERE path = '$WT/item-6-audit'")"

worktree_refused() { # <label> <needle> <job> <worktree arguments...>
  local label=$1 needle=$2 job=$3
  shift 3
  local out rc
  out=$(in_job "$job" worktree "$@" 2>&1); rc=$?
  check "worktree: $label refused with exit 1" 1 "$rc"
  has "worktree: $label refusal says why" "$out" "$needle"
}
mkdir -p "$WT/item-8"
worktree_refused "an unset job" "${P}JOB" "" "$R"
worktree_refused "another job's path" "belongs to job item-6" item-6-review "$R"
worktree_refused "a path the ledger does not record" "does not record it" item-8 "$R"
worktree_refused "a repo without a checkout" "no checkout" item-9 no-such-repo
worktree_refused "--detach with --branch" "exclude each other" item-9 "$R" --name x --detach "$head" --branch fix/x
worktree_refused "--detach of a ref that is not there" "names no commit" item-9 "$R" --name x --detach no-such-ref
check "worktree: refusals wrote no row" "0 " \
  "$(ledger "SELECT count(*) FROM worktrees WHERE job IN ('item-6-review', 'item-8', 'item-9')")"
rmdir "$WT/item-8"

# job end --force: an agent's cwd inside one of the job's worktrees, then an
# uncommitted change, each keeps both worktrees.
spane=$("${S[@]}" tab create --workspace "$("${S[@]}" pane get "$tpane" | jq -r .result.pane.workspace_id)" \
  --cwd "$WT/item-6-review" --label squatter-2 --no-focus | jq -r '.result.root_pane.pane_id')
"${S[@]}" agent start squatter-2 --kind claude --pane "$spane" --timeout 20000 >/dev/null
out=$(thr job end item-6 --force 2>&1); rc=$?
check "job end --force: exit 5 while an agent's cwd is in a recorded worktree" 5 "$rc"
has "job end --force: names the agent in the recorded worktree" "$out" "agent squatter-2"
check "job end --force: recorded worktrees kept while one is in use" "yes yes" \
  "$([ -d "$WT/item-6" ] && echo yes || echo no) $([ -d "$WT/item-6-review" ] && echo yes || echo no)"
"${S[@]}" pane close "$spane" >/dev/null

echo draft > "$WT/item-6-review/notes.txt"
thr job end item-6 --force >/dev/null 2>&1; rc=$?
check "job end --force: exit 5 when a recorded worktree has uncommitted changes" 5 "$rc"
check "job end --force: the uncommitted change is kept" draft "$(cat "$WT/item-6-review/notes.txt" 2>&1)"
rm "$WT/item-6-review/notes.txt"

out=$(thr job end item-6 --force 2>&1); rc=$?
check "job end --force: recorded worktrees, exit 0 once nothing is in the way" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "job end --force: recorded worktrees removed, the detached one too" "no no no" \
  "$([ -e "$WT/item-6" ] && echo yes || echo no) $([ -e "$WT/item-6-review" ] && echo yes || echo no) $([ -e "$WT/item-6-audit" ] && echo yes || echo no)"
check "job end --force: their branches deleted" "" "$(git -C "/home/agent/dev/$R" branch --list item-6 fix/item-6)"
check "job end --force: ledger marks them removed" "$WT/item-6|$R|item-6|item-6|item-6-a|0 $WT/item-6-review|$R|fix/item-6|item-6|item-6-a|0 $WT/item-6-audit|$R||item-6|item-6-a|0 " \
  "$(wt_rows item-6)"

# An initiative's repo (x-repo-<I>) with no checkout of that name in ~/dev:
# the worktree comes from the initiative's checkout ~/x-repo/<I>, and
# `job end` removes it like the others, deleting its branch there.
I=example-init
git init -q --bare -b main "/home/agent/remote/x-repo-$I.git"
git clone -q "/home/agent/remote/x-repo-$I.git" "/home/agent/seed-$I" 2>/dev/null
git -C "/home/agent/seed-$I" commit -q --allow-empty -m charter
git -C "/home/agent/seed-$I" push -q origin main
git clone -q "/home/agent/remote/x-repo-$I.git" "/home/agent/x-repo/$I"
ihead=$(git -C "/home/agent/seed-$I" rev-parse HEAD)
IWT=/home/agent/wt/x-repo-$I
out=$(in_job item-10 worktree "x-repo-$I" --branch docs/charter 2>&1); rc=$?
check "worktree of an initiative's repo: exit 0, under ~/wt/x-repo-<I>" "0 $IWT/item-10" "$rc $out"
check "worktree of an initiative's repo: on the branch, from the initiative's checkout" "docs/charter $ihead" \
  "$(git -C "$IWT/item-10" branch --show-current 2>&1) $(git -C "$IWT/item-10" rev-parse HEAD 2>&1)"
out=$(in_job item-10 worktree "x-repo-$I" --name review --detach "$ihead" 2>&1); rc=$?
check "worktree --detach of an initiative's repo: exit 0 at the commit" "0 $IWT/item-10-review $ihead" \
  "$rc $out $(git -C "$IWT/item-10-review" rev-parse HEAD 2>&1)"
worktree_refused "a repo in neither checkouts nor initiatives" "/home/agent/x-repo/no-such" item-10 x-repo-no-such
out=$(thr job end item-10 --force 2>&1); rc=$?
check "job end --force: the initiative's worktrees removed and their branch deleted" "0 no no " \
  "$rc $([ -e "$IWT/item-10" ] && echo yes || echo no) $([ -e "$IWT/item-10-review" ] && echo yes || echo no) $(git -C "/home/agent/x-repo/$I" branch --list docs/charter)"

# A scope whose settings move the worktrees and rename the initiative
# prefix: the same initiative's repo is then multi-<I>, under ~/trees.
mkdir -p "/home/agent/.config/$T"
echo '{"paths": {"worktrees": "~/trees"}, "channels": {"initiative_prefix": "multi-"}}' > "/home/agent/.config/$T/custom.json"
out=$(SCOPE=custom in_job item-11 worktree "multi-$I" 2>&1); rc=$?
check "worktree in a scope with paths and channels set: exit 0 under its worktrees" "0 /home/agent/trees/multi-$I/item-11" "$rc $out"
out=$(SCOPE=custom in_job item-11 worktree "x-repo-$I" 2>&1); rc=$?
check "worktree in a scope with another initiative prefix: x-repo-<I> refused" 1 "$rc"
git -C "/home/agent/x-repo/$I" worktree remove "/home/agent/trees/multi-$I/item-11"
git -C "/home/agent/x-repo/$I" branch -q -D item-11
