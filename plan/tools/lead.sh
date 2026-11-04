#!/usr/bin/env bash
# Run by a LEAD worker (sol) on an L3 task: delegates implementation to a luna worker in the
# current worktree, waits for it and prints its report.
#
#   plan/tools/lead.sh <ID> implement            luna implements plan/specs/<ID>.plan.md
#   plan/tools/lead.sh <ID> fix <N> <notes.md>   luna fixes round N (N = 1 or 2) from your review notes
#   plan/tools/lead.sh --wait <dispatch>         keep waiting for an already started implementer
#
# Exit codes: 0 = implementer report printed; 3 = still running / asked a question
# (re-run with --wait <dispatch>, answer questions with orca orchestration reply); 2 = usage; other = error.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
CHUNK_MS=480000   # stay below common 10-minute shell limits
MAX_FIX=2

wait_for() {
  local ds="$1" deadline=$(( $(date +%s) + CHUNK_MS / 1000 ))
  while :; do
    local left=$(( (deadline - $(date +%s)) * 1000 ))
    if (( left <= 0 )); then
      echo "lead: implementer $ds still running; re-run: plan/tools/lead.sh --wait $ds"
      exit 3
    fi
    local out mine
    out="$(orca orchestration check --wait --types "worker_done,escalation,question" \
             --timeout-ms "$left" --json 2>/dev/null)" || { echo "$out"; exit 1; }
    mine="$(jq -c --arg ds "$ds" '[.result.messages[]? | select((.payload.dispatchId // "") == $ds)]' <<<"$out")"
    if [[ "$(jq 'length' <<<"$mine")" -gt 0 ]]; then
      jq -r '.[] | "── [\(.type)] id=\(.id) \(.subject // "")\n\(.body // "")"' <<<"$mine"
      if jq -e 'any(.[]; .type == "worker_done")' <<<"$mine" >/dev/null; then
        orca orchestration worker-release --dispatch "$ds" --json >/dev/null 2>&1 || true
        exit 0
      fi
      echo "lead: the implementer asked something above; answer: orca orchestration reply --id <id> --body '...'; then: plan/tools/lead.sh --wait $ds"
      exit 3
    fi
    jq -r --arg ds "$ds" '.result.messages[]? | select((.payload.dispatchId // "") != $ds)
      | "── (not from implementer) [\(.type)] id=\(.id) \(.subject // "")\n\(.body // "")"' <<<"$out"
  done
}

[[ "${1:-}" == "--wait" ]] && wait_for "${2:?dispatch id}"

ID="${1:?usage: lead.sh <ID> implement | fix <N> <notes.md>}"; MODE="${2:?mode}"
SPEC="plan/specs/$ID.md"; PLAN="plan/specs/$ID.plan.md"
[[ -f "$SPEC" && -f "$PLAN" ]] || { echo "need $SPEC and a committed $PLAN" >&2; exit 2; }
git diff --quiet && git diff --cached --quiet || { echo "commit first (working tree is dirty)" >&2; exit 2; }
S="$(git rev-parse --git-dir)/lead-$ID-$MODE${3:+-$3}.md"

section() { awk -v h="## $1" '$0==h{p=1;next} /^## /{p=0} p' "$SPEC"; }

case "$MODE" in
  implement)
    TITLE="Implement $ID"
    TASK="Implement the plan below exactly. If the plan is wrong or impossible, do not improvise a different design: ask (orchestration ask) and end your turn.

=== PLAN (written by the lead) ===
$(cat "$PLAN")" ;;
  fix)
    N="${3:?round}"; NOTES="${4:?notes file}"
    [[ "$N" =~ ^[0-9]+$ ]] && (( N >= 1 && N <= MAX_FIX )) || { echo "fix round must be 1..$MAX_FIX; after that the lead finishes the task itself" >&2; exit 2; }
    [[ -f "$NOTES" ]] || { echo "no notes file $NOTES" >&2; exit 2; }
    TITLE="Fix $ID round $N"
    TASK="The lead reviewed your implementation. Fix every item below (blocker/major mandatory, minor when cheap). Keep the existing commits, add new ones. The plan is $PLAN.

=== REVIEW NOTES (round $N of at most $MAX_FIX) ===
$(cat "$NOTES")" ;;
  *) echo "unknown mode $MODE" >&2; exit 2 ;;
esac

cat > "$S" <<SPEC
# $TITLE (implementer)

## Target
Worktree \`$ROOT\` (absolute path; work only there), branch $(git branch --show-current). Task spec: \`$SPEC\`; implementation plan: \`$PLAN\`; design: \`plan/design/perf.md\` (Russian; its owner-decisions section overrides the rest).

## Change
$TASK

## Constraints
$(section Constraints)
- Do NOT run plan/tools/self-review.sh or plan/tools/lead.sh and do not start workers: the lead reviews your work.
- Communicate in English.

## Ownership
$(section Ownership)

## Observable acceptance
$(section "Observable acceptance")
Run all of it, commit, then send worker_done with: commits, files changed, acceptance results, and anything you could not do.
SPEC

out="$(orca orchestration worker-start --spec-file "$S" --task-title "$TITLE" \
        --worktree current --agent codex --model gpt-6-luna --effort high --json)" || { echo "$out"; exit 1; }
ds="$(jq -r '.result.dispatchId // .result.dispatch.id // .dispatchId // empty' <<<"$out")"
[[ -n "$ds" ]] || { echo "$out"; exit 1; }
echo "lead: started implementer $ds ($TITLE)"
wait_for "$ds"
