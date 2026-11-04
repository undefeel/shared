#!/usr/bin/env bash
# Запуск воркера Orca для задачи плана.
# Использование: plan/tools/start.sh <ID> [исполнитель] [path:<worktree>]
#   исполнитель: gemini (по умолчанию, = antigravity) | gemini-medium | luna | luna-low | sol | sol-low | sol-high
#                | haiku | sonnet | sonnet-low | sonnet-high
#   path:<worktree> — запустить в существующем worktree (повтор, --retry-of передавать через RETRY_OF=<dispatch>)
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
IDS="$ROOT/plan/specs/orca-task-ids.txt"
STATUS="$ROOT/plan/STATUS.md"

ID="${1:?Использование: start.sh <ID> [исполнитель] [path:<worktree>]}"
EXEC="${2:-gemini}"
PLACE="${3:-}"

base="${EXEC%-low}"; base="${base%-high}"
effort_override=""
case "$EXEC" in *-low) effort_override=low ;; *-high) effort_override=high ;; esac

case "$base" in
  opus|claude-opus*) echo "Ошибка: Opus воркером не запускается" >&2; exit 2 ;;
  gemini-medium) agent=(--agent antigravity --model gemini-3.8-flash-medium) ;;  # уровень задан в ID модели
  gemini|antigravity) agent=(--agent antigravity)
       [[ -n "$effort_override" ]] && { echo "Ошибка: у antigravity нет --effort (нужен --model)" >&2; exit 2; } ;;
  haiku)  agent=(--agent claude --model claude-haiku-5-5);  effort=medium ;;
  luna)   agent=(--agent codex  --model gpt-6-luna);        effort=high ;;
  sonnet) agent=(--agent claude --model claude-sonnet-5-5); effort=medium ;;
  sol)    agent=(--agent codex  --model gpt-6.1-sol);       effort=medium ;;
  *) echo "Ошибка: неизвестный исполнитель '$EXEC'" >&2; exit 2 ;;
esac
if [[ "$base" != gemini && "$base" != antigravity && "$base" != gemini-medium ]]; then
  agent+=(--effort "${effort_override:-$effort}")
fi

tk="$(awk -v id="$ID" '$1==id {print $2; exit}' "$IDS")"
[[ -n "$tk" ]] || { echo "Ошибка: нет task_id для $ID в $IDS" >&2; exit 2; }

if [[ -n "$PLACE" ]]; then
  [[ "$PLACE" == path:* ]] || { echo "Ошибка: третий аргумент должен быть path:<worktree>" >&2; exit 2; }
  place=(--worktree "$PLACE")
else
  place=(--worktree new-child --name "t$ID")
fi
retry=()
[[ -n "${RETRY_OF:-}" ]] && retry=(--retry-of "$RETRY_OF")

addendum="$ROOT/plan/specs/$ID.addendum.md"
comment=()
# --comment — метаданные Orca и допустим только при создании worktree.
if [[ -f "$addendum" && -z "$PLACE" ]]; then
  comment=(--comment "$(cat "$addendum")")
fi

out="$(orca orchestration worker-start --task "$tk" "${place[@]}" "${agent[@]}" \
        ${comment[@]+"${comment[@]}"} ${retry[@]+"${retry[@]}"} --json)" || { echo "$out"; exit 1; }

dispatch="$(jq -r '.result.dispatchId // .result.dispatch.id // .result.dispatch_id // .dispatchId // empty' <<<"$out")"
[[ -n "$dispatch" ]] || { echo "$out"; echo "Ошибка: в ответе нет dispatchId" >&2; exit 1; }

# --comment воркер не читает, поэтому addendum доставляется ещё и сообщением.
if [[ -f "$addendum" ]]; then
  orca orchestration send --to "dispatch:$dispatch" --subject "Addendum к $ID" \
       --body "$(cat "$addendum")" --json >/dev/null
fi

echo "$ID $dispatch"

if [[ -f "$STATUS" ]] && grep -q '^Активные:' "$STATUS"; then
  tmp="$(mktemp)"
  awk -v add="$ID \`$dispatch\`" '/^Активные:/ { sub(/[[:space:]]*$/, ""); $0 = ($0 ~ /:$/ ? $0 " " add : $0 ", " add) } {print}' "$STATUS" >"$tmp"
  mv "$tmp" "$STATUS"
fi
