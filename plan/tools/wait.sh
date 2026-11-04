#!/usr/bin/env bash
# Ждёт почту воркеров. Батчи только из heartbeat подтверждает сам и ждёт дальше.
# Использование: plan/tools/wait.sh [timeout_ms]   (по умолчанию 540000 — под лимит Bash в 10 минут)
#
# Сторож лидов: каждые WATCH_SEC проверяет активных воркеров из строки «Активные:» в STATUS.md.
# Если у воркера в его worktree нет активных вложенных воркеров и файлы worktree не менялись
# IDLE_SEC секунд, будит его сообщением (не чаще раза в NUDGE_SEC на воркер).
# Причина: Orca не будит вложенного координатора (лида), когда отчитался его дочерний воркер.
set -euo pipefail
TIMEOUT="${1:-540000}"
WATCH_SEC="${WATCH_SEC:-300}"; IDLE_SEC="${IDLE_SEC:-600}"; NUDGE_SEC="${NUDGE_SEC:-1200}"
ROOT="$(git rev-parse --show-toplevel)"
STATE="$(git rev-parse --git-common-dir)/wait-nudges"; touch "$STATE"
deadline=$(( $(date +%s) + TIMEOUT / 1000 ))

watchdog() {
  local now workers ds wt newest last
  now=$(date +%s)
  workers="$(orca orchestration worker-list --json 2>/dev/null)" || return 0
  for ds in $(grep '^Активные:' "$ROOT/plan/STATUS.md" | grep -o 'ctx_[0-9a-f]*' | sort -u); do
    wt="$(jq -r --arg d "$ds" '.result.workers[]? | select(.dispatchId==$d) | (.workspace // "") | sub("^[^:]*::"; "")' <<<"$workers")"
    [[ -n "$wt" && -d "$wt" ]] || continue
    # дочерние воркеры в том же worktree ещё работают — лид ждёт, это нормально
    jq -e --arg d "$ds" --arg wt "$wt" 'any(.result.workers[]?; .dispatchId != $d and ((.workspace // "") | endswith($wt)) and .terminalState == "active")' <<<"$workers" >/dev/null && continue
    # сам воркер уже отчитался — не будить
    jq -e --arg d "$ds" 'any(.result.workers[]?; .dispatchId == $d and (.state == "succeeded" or .state == "failed"))' <<<"$workers" >/dev/null && continue
    newest="$(find "$wt" -path "$wt/.git" -prune -o -type f -newermt "-${IDLE_SEC} seconds" -print -quit 2>/dev/null)"
    [[ -z "$newest" ]] || continue
    [[ -n "$(git -C "$wt" log -1 --since="${IDLE_SEC} seconds ago" --format=%h 2>/dev/null)" ]] && continue
    last="$(awk -v d="$ds" '$1==d {print $2}' "$STATE" | tail -1)"
    (( now - ${last:-0} >= NUDGE_SEC )) || continue
    orca orchestration send --to "dispatch:$ds" --subject "Watchdog: are you stuck?" --body "No file changes and no running child workers in your worktree for ${IDLE_SEC}s. If you ended your turn while waiting for a child worker, its report is in your inbox (orca orchestration inbox --terminal dispatch:$ds --full): read it and continue your workflow. If the task is complete, send worker_done. If you are actively working, ignore this message." --json >/dev/null 2>&1 \
      && echo "$ds $now" >> "$STATE" && echo "wait.sh: сторож разбудил $ds (простой ≥ ${IDLE_SEC}s)"
  done
}

while :; do
  left=$(( (deadline - $(date +%s)) * 1000 ))
  (( left > 0 )) || { echo "wait.sh: таймаут, почты нет"; exit 0; }
  chunk=$(( left < WATCH_SEC * 1000 ? left : WATCH_SEC * 1000 ))
  out="$(orca orchestration check --wait --types "worker_done,escalation,question" \
           --timeout-ms "$chunk" --json 2>/dev/null)" || { echo "$out"; exit 1; }
  n="$(jq '.result.messages | length' <<<"$out")"
  if (( n == 0 )); then watchdog; continue; fi
  other="$(jq '[.result.messages[] | select(.type != "heartbeat")] | length' <<<"$out")"
  if (( other == 0 )); then
    did="$(jq -r '.result.deliveryId // .result.delivery_id // empty' <<<"$out")"
    [[ -n "$did" ]] && orca orchestration check --ack "$did" --json >/dev/null 2>&1 || true
    watchdog
    continue
  fi
  jq -r '.result.messages[] | select(.type != "heartbeat")
    | "── \(.id) [\(.type)] \(.subject // "")\n\(.body // "")\npayload: \(.payload // {} | tojson)\n"' <<<"$out"
  exit 0
done
