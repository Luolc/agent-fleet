# Sourced by inside.sh after lifecycle.sh, which set up origin, ~/dev/$R and
# ~/seed: `worktree` opens a worktree for the caller's job, and `close`
# removes the ones recorded for the job. No agent of the job is spawned; the
# binary runs from this shell with a job's identity variables.
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

out=$(as item-6-lead lead orchestra item-6 -- worktree "$R" 2>&1); rc=$?
check "worktree: again in the same job, exit 0 with the same path" "0 $WT/item-6" "$rc $out"
check "worktree: again in the same job, no new row" "1 " "$(ledger "SELECT count(*) FROM worktrees WHERE job = 'item-6'")"

out=$(in_job item-6 worktree "$R" --name review --branch fix/item-6 2>&1); rc=$?
check "worktree --name --branch: exit 0 with <job>-<name>" "0 $WT/item-6-review" "$rc $out"
check "worktree --branch: on the given branch" fix/item-6 "$(git -C "$WT/item-6-review" branch --show-current 2>&1)"

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
check "worktree: refusals wrote no row" "0 " \
  "$(ledger "SELECT count(*) FROM worktrees WHERE job IN ('item-6-review', 'item-8', 'item-9')")"
rmdir "$WT/item-8"

# close: an agent's cwd inside one of the job's worktrees, then an
# uncommitted change, each keeps both worktrees.
spane=$("${S[@]}" tab create --workspace "$("${S[@]}" pane get "$opane" | jq -r .result.pane.workspace_id)" \
  --cwd "$WT/item-6-review" --label squatter-2 --no-focus | jq -r '.result.root_pane.pane_id')
"${S[@]}" agent start squatter-2 --kind claude --pane "$spane" --timeout 20000 >/dev/null
out=$(orch close item-6 2>&1); rc=$?
check "close: exit 5 while an agent's cwd is in a recorded worktree" 5 "$rc"
has "close: names the agent in the recorded worktree" "$out" "agent squatter-2"
check "close: recorded worktrees kept while one is in use" "yes yes" \
  "$([ -d "$WT/item-6" ] && echo yes || echo no) $([ -d "$WT/item-6-review" ] && echo yes || echo no)"
"${S[@]}" pane close "$spane" >/dev/null

echo draft > "$WT/item-6-review/notes.txt"
orch close item-6 >/dev/null 2>&1; rc=$?
check "close: exit 5 when a recorded worktree has uncommitted changes" 5 "$rc"
check "close: the uncommitted change is kept" draft "$(cat "$WT/item-6-review/notes.txt" 2>&1)"
rm "$WT/item-6-review/notes.txt"

out=$(orch close item-6 2>&1); rc=$?
check "close: recorded worktrees, exit 0 once nothing is in the way" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "close: recorded worktrees removed" "no no" \
  "$([ -e "$WT/item-6" ] && echo yes || echo no) $([ -e "$WT/item-6-review" ] && echo yes || echo no)"
check "close: their branches deleted" "" "$(git -C "/home/agent/dev/$R" branch --list item-6 fix/item-6)"
check "close: ledger marks them removed" "$WT/item-6|$R|item-6|item-6|item-6-a|0 $WT/item-6-review|$R|fix/item-6|item-6|item-6-a|0 " \
  "$(wt_rows item-6)"
