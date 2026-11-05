#!/usr/bin/env bash
# Run by a LEAD worker (sol) on an L3 task: delegates implementation to a terra (or luna) worker in the
# current worktree, waits for it and prints its report.
#
#   plan/tools/lead.sh <ID> implement            the implementer implements plan/specs/<ID>.plan.md
#   plan/tools/lead.sh <ID> fix <N> <notes.md>   the implementer fixes round N (N = 1 or 2) from your review notes
#   plan/tools/lead.sh --wait <dispatch>         keep waiting for an already started implementer
#   plan/tools/lead.sh <ID> part <name> <part.md> start a PARALLEL implementer for one plan part (no wait);
#                                                 parts must own disjoint files; the implementer does not commit
#   plan/tools/lead.sh --wait-all <d1> <d2> ...   wait until all parallel implementers reported
#
# Exit codes: 0 = implementer report printed; 3 = still running / asked a question
# (re-run with --wait <dispatch>, answer questions with orca orchestration reply); 2 = usage; other = error.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
CHUNK_MS=480000   # stay below common 10-minute shell limits
MAX_FIX=2
MAX_PARTS=3      # parallel implementers per lead
# Implementer model: terra (default, gpt-6.1-terra medium: follows a plan over many steps) or luna
# (gpt-6-luna high: cheap, only small pieces of ~300 lines). Choose with LEAD_IMPL=luna|terra.
case "${LEAD_IMPL:-terra}" in
  terra) IMPL=(--agent codex --model gpt-6.1-terra --effort medium) ;;
  luna)  IMPL=(--agent codex --model gpt-6-luna --effort high) ;;
  *) echo "LEAD_IMPL must be terra or luna" >&2; exit 2 ;;
esac

MAX_CONT=8       # automatic "continue" nudges per implementer
CONT_MSG="Continue the task from where you stopped. Do not re-read what you already know; keep implementing the remaining items of your plan/part, run the acceptance, commit (parts: do not commit). Send worker_done only when everything is done or on a concrete technical blocker. If you run out of room again, just end your turn: you will be resumed."
# Implementer ended its turn without worker_done (Orca escalation with stopReason): resume the same session.
continue_impl() {
  local ds="$1" f n
  f="$(git rev-parse --git-dir)/lead-cont-$ds"; n=$(( $(cat "$f" 2>/dev/null || echo 0) + 1 ))
  if (( n > MAX_CONT )); then echo "lead: implementer $ds stopped $MAX_CONT times without finishing; review what it committed and continue with a fix round or take over"; return 1; fi
  echo "$n" >"$f"
  orca orchestration send --to "dispatch:$ds" --subject "Continue ($n/$MAX_CONT)" --body "$CONT_MSG" --json >/dev/null \
    && echo "lead: implementer $ds ended its turn without reporting; sent continue $n/$MAX_CONT"
}

wait_for() {
  local ds="$1" deadline=$(( $(date +%s) + CHUNK_MS / 1000 ))
  while :; do
    local left=$(( (deadline - $(date +%s)) * 1000 ))
    if (( left <= 0 )); then
      echo "lead: implementer $ds still running. Do NOT end your turn (nobody will wake you) — re-run now: plan/tools/lead.sh --wait $ds"
      exit 3
    fi
    local out mine
    out="$(orca orchestration check --wait --types "worker_done,escalation,question" \
             --timeout-ms "$left" --json 2>/dev/null)" || { echo "$out"; exit 1; }
    mine="$(jq -c --arg ds "$ds" '[.result.messages[]? | select((.payload.dispatchId // "") == $ds)]' <<<"$out")"
    if [[ "$(jq 'length' <<<"$mine")" -gt 0 ]]; then
      if jq -e 'all(.[]; .type == "escalation" and ((.payload.stopReason // "") != ""))' <<<"$mine" >/dev/null; then
        continue_impl "$ds" || exit 3
        continue
      fi
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

if [[ "${1:-}" == "--wait-all" ]]; then
  shift; pending=("$@"); (( ${#pending[@]} > 0 )) || { echo "usage: --wait-all <d1> ..." >&2; exit 2; }
  deadline=$(( $(date +%s) + CHUNK_MS / 1000 ))
  while (( ${#pending[@]} > 0 )); do
    left=$(( (deadline - $(date +%s)) * 1000 ))
    if (( left <= 0 )); then echo "lead: still running: ${pending[*]}. Do NOT end your turn (nobody will wake you) — re-run now: plan/tools/lead.sh --wait-all ${pending[*]}"; exit 3; fi
    out="$(orca orchestration check --wait --types "worker_done,escalation,question" --timeout-ms "$left" --json 2>/dev/null)" || { echo "$out"; exit 1; }
    for sds in $(jq -r '.result.messages[]? | select(.type == "escalation" and ((.payload.stopReason // "") != "")) | .payload.dispatchId // empty' <<<"$out"); do
      [[ " ${pending[*]} " == *" $sds "* ]] && { continue_impl "$sds" || true; }
    done
    jq -r '.result.messages[]? | select(.type != "escalation" or ((.payload.stopReason // "") == "")) | "── [\(.type)] id=\(.id) from=\(.payload.dispatchId // "?") \(.subject // "")\n\(.body // "")"' <<<"$out"
    if jq -e '[.result.messages[]? | select(.type != "worker_done" and .type != "heartbeat" and ((.payload.stopReason // "") == ""))] | length > 0' <<<"$out" >/dev/null; then
      echo "lead: a question/escalation above needs an answer (orca orchestration reply --id <id> --body ...); then re-run --wait-all with the remaining dispatches"
    fi
    next=()
    for ds in "${pending[@]}"; do
      if jq -e --arg ds "$ds" 'any(.result.messages[]?; .type == "worker_done" and (.payload.dispatchId // "") == $ds)' <<<"$out" >/dev/null; then
        orca orchestration worker-release --dispatch "$ds" --json >/dev/null 2>&1 || true
      else next+=("$ds"); fi
    done
    pending=("${next[@]+"${next[@]}"}")
  done
  echo "lead: all parallel implementers reported; now run the full acceptance yourself and commit"
  exit 0
fi

ID="${1:?usage: lead.sh <ID> implement | fix <N> <notes.md>}"; MODE="${2:?mode}"
SPEC="plan/specs/$ID.md"; PLAN="plan/specs/$ID.plan.md"
[[ -f "$SPEC" && -f "$PLAN" ]] || { echo "need $SPEC and a committed $PLAN" >&2; exit 2; }
if [[ "$MODE" != part ]]; then
  git diff --quiet && git diff --cached --quiet || { echo "commit first (working tree is dirty)" >&2; exit 2; }
fi
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
  part)
    PART="${3:?part name}"; PFILE="${4:?part plan file}"
    [[ -f "$PFILE" ]] || { echo "no part file $PFILE" >&2; exit 2; }
    running="$(orca orchestration worker-list --json 2>/dev/null | jq --arg wt "$ROOT" '[.result.workers[]? | select((.workspace // "") | endswith($wt)) | select(.terminalState == "active")] | length')"
    (( ${running:-0} <= MAX_PARTS )) || { echo "too many active workers in this worktree (max $MAX_PARTS parallel parts)" >&2; exit 2; }
    TITLE="Implement $ID part $PART"
    TASK="Implement ONLY this part of the plan. Other implementers work in the same worktree at the same time on other parts:
- edit ONLY the files listed in this part; never touch, format or revert any other file;
- do NOT commit, do NOT run git commands that change state (add/commit/stash/checkout/reset);
- run only the tests of your own packages; failures clearly caused by unfinished files of other parts are not yours — report them;
- if the part needs a change outside its files, ask (orchestration ask) and end your turn.
Full plan for context: $PLAN.

=== PART $PART ===
$(cat "$PFILE")" ;;
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
- You may work over several turns. If you cannot finish in this turn, simply end your turn WITHOUT worker_done (commit finished pieces first, parts leave files uncommitted): the lead automatically resumes you with "continue" and you keep going in the same session. NEVER send worker_done with outcome failed for unfinished work — only for a concrete technical blocker you name.
- If an existing test contradicts the plan, ask (orchestration ask) instead of stopping.
- Communicate in English.

## Ownership
$(section Ownership)

## Observable acceptance
$(if [[ "$MODE" == part ]]; then echo "Run go vet and the tests of your own packages only. Do not commit. Send worker_done with: files changed, test results, anything you could not do."; else section "Observable acceptance"; echo "Run all of it, commit, then send worker_done with: commits, files changed, acceptance results, and anything you could not do."; fi)
SPEC

out="$(orca orchestration worker-start --spec-file "$S" --task-title "$TITLE" \
        --worktree current "${IMPL[@]}" --json)" || { echo "$out"; exit 1; }
ds="$(jq -r '.result.dispatchId // .result.dispatch.id // .dispatchId // empty' <<<"$out")"
[[ -n "$ds" ]] || { echo "$out"; exit 1; }
echo "lead: started implementer $ds ($TITLE, ${LEAD_IMPL:-terra})"
if [[ "$MODE" == part ]]; then echo "lead: not waiting (parallel part); later: plan/tools/lead.sh --wait-all <all part dispatches>"; exit 0; fi
wait_for "$ds"
