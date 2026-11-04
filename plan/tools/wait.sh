#!/usr/bin/env bash
# Ждёт почту воркеров. Батчи только из heartbeat подтверждает сам и ждёт дальше.
# Использование: plan/tools/wait.sh [timeout_ms]   (по умолчанию 540000 — под лимит Bash в 10 минут)
set -euo pipefail
TIMEOUT="${1:-540000}"
deadline=$(( $(date +%s) + TIMEOUT / 1000 ))

while :; do
  left=$(( (deadline - $(date +%s)) * 1000 ))
  (( left > 0 )) || { echo "wait.sh: таймаут, почты нет"; exit 0; }
  out="$(orca orchestration check --wait --types "worker_done,escalation,question" \
           --timeout-ms "$left" --json 2>/dev/null)" || { echo "$out"; exit 1; }
  n="$(jq '.result.messages | length' <<<"$out")"
  (( n > 0 )) || { echo "wait.sh: таймаут, почты нет"; exit 0; }
  other="$(jq '[.result.messages[] | select(.type != "heartbeat")] | length' <<<"$out")"
  if (( other == 0 )); then
    did="$(jq -r '.result.deliveryId // .result.delivery_id // empty' <<<"$out")"
    [[ -n "$did" ]] && orca orchestration check --ack "$did" --json >/dev/null 2>&1 || true
    continue
  fi
  jq -r '.result.messages[] | select(.type != "heartbeat")
    | "── \(.id) [\(.type)] \(.subject // "")\n\(.body // "")\npayload: \(.payload // {} | tojson)\n"' <<<"$out"
  exit 0
done
