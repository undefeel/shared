#!/usr/bin/env bash
# Self-review run by a WORKER (not the coordinator): starts a read-only Gemini reviewer
# in the current worktree, waits for its report, prints it and releases the reviewer.
#
#   plan/tools/self-review.sh <ID> [round]        start a review round (default round 1) and wait
#   plan/tools/self-review.sh --wait <dispatch>   keep waiting for an already started reviewer
#
# Exit codes: 0 = report printed; 3 = reviewer still running (re-run with --wait <dispatch>);
#             other = error.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
MODEL=gemini-3.8-flash-medium
CHUNK_MS=480000   # stay below common 10-minute shell limits

wait_for() {
  local ds="$1" deadline=$(( $(date +%s) + CHUNK_MS / 1000 ))
  while :; do
    local left=$(( (deadline - $(date +%s)) * 1000 ))
    if (( left <= 0 )); then
      echo "self-review: reviewer $ds still running; re-run: plan/tools/self-review.sh --wait $ds"
      exit 3
    fi
    local out
    out="$(orca orchestration check --wait --types "worker_done,escalation,question" \
             --timeout-ms "$left" --json 2>/dev/null)" || { echo "$out"; exit 1; }
    local mine
    mine="$(jq -c --arg ds "$ds" '[.result.messages[]? | select((.payload.dispatchId // "") == $ds)]' <<<"$out")"
    if [[ "$(jq 'length' <<<"$mine")" -gt 0 ]]; then
      jq -r '.[] | "── [\(.type)] \(.subject // "")\n\(.body // "")"' <<<"$mine"
      if jq -e 'any(.[]; .type == "worker_done")' <<<"$mine" >/dev/null; then
        orca orchestration worker-release --dispatch "$ds" --json >/dev/null 2>&1 || true
        exit 0
      fi
      echo "self-review: the reviewer asked something above; answer with: orca orchestration reply --id <message_id> --body '...'; then re-run with --wait $ds"
      exit 3
    fi
    # Mail for someone else (e.g. the coordinator's messages to you) — print it, keep waiting.
    jq -r --arg ds "$ds" '.result.messages[]? | select((.payload.dispatchId // "") != $ds)
      | "── (not from reviewer) [\(.type)] \(.subject // "")\n\(.body // "")"' <<<"$out"
  done
}

if [[ "${1:-}" == "--wait" ]]; then
  wait_for "${2:?dispatch id}"
fi

ID="${1:?usage: self-review.sh <ID> [round]}"; ROUND="${2:-1}"
SPEC_TASK="plan/specs/$ID.md"
[[ -f "$SPEC_TASK" ]] || { echo "no $SPEC_TASK" >&2; exit 2; }
BASE="$(git merge-base HEAD origin/main)"
git diff --quiet && git diff --cached --quiet || { echo "commit your work first (working tree is dirty)" >&2; exit 2; }
S="$(git rev-parse --git-dir)/self-review-$ID-r$ROUND.md"
cat > "$S" <<SPEC
# Review $ID, round $ROUND (read-only)

## Target
Current worktree, commits $(git log --format='%h' "$BASE"..HEAD | tr '\n' ' ').
Requirements: \`$SPEC_TASK\`. Design: \`plan/design/perf.md\` (its first section of owner decisions overrides the rest) and \`plan/design/folder.md\`. The design docs are in Russian.

## Change
Review \`git diff $BASE...HEAD\` against the spec and the design. Look for bugs: escaping the root or following symlinks, fd leaks, lock order scanMu → saveMu → mu, races with watcher/Scan/multiple runPeer/downloads, a transient error turning into a delete or tombstone, data loss on a crash between steps, unintended wire-format changes. Check that every test required by the spec exists and really tests what it claims. Check the diff stays inside the spec's Ownership.

## Constraints
Do NOT change, create or delete any file, do not commit, do not run git commands that modify state. You may read files and run read-only commands. Answer in English in the \`worker_done\` body: a list of \`[blocker|major|minor] file:line — issue — how to fix\`, or exactly "no findings". blocker/major only with a concrete execution path.

## Ownership
None (read-only).

## Observable acceptance
Report in the \`worker_done\` body.
SPEC
out="$(orca orchestration worker-start --spec-file "$S" --task-title "Self-review $ID r$ROUND" \
        --worktree current --agent antigravity --model "$MODEL" --json)" || { echo "$out"; exit 1; }
ds="$(jq -r '.result.dispatchId // .result.dispatch.id // .dispatchId // empty' <<<"$out")"
[[ -n "$ds" ]] || { echo "$out"; exit 1; }
echo "self-review: started reviewer $ds for $ID round $ROUND"
wait_for "$ds"
