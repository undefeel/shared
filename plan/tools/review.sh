#!/usr/bin/env bash
# Ревью задачи (только чтение) в worktree её воркера: plan/tools/review.sh <ID> [luna|luna-low]
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
ID="$1"; EF=high; [[ "${2:-luna}" == luna-low ]] && EF=low
WT="$(dirname "$ROOT")/t$ID"; BR="undefeel/t$ID"
mkdir -p plan/specs/review; S="plan/specs/review/$ID.md"
cat > "$S" <<SPEC
# Ревью $ID (только чтение)

## Target
Ветка \`$BR\` в текущем worktree, коммиты: $(git log --format='%h' main.."$BR" | tr '\n' ' ').
Что требовалось — \`plan/specs/$ID.md\`; общий проект — \`plan/design/folder.md\` (раздел «Решение владельца» важнее остального).

## Change
Проверь diff \`git diff main...$BR\` на соответствие spec и проекту и на баги: выход за корень и следование симлинкам, утечки fd, порядок блокировок scanMu → saveMu → mu, гонки с watcher/Scan/несколькими runPeer, превращение временной ошибки в удаление или tombstone, потеря данных при сбое между шагами, совместимость формата \`/index\` и \`/file\`. Проверь, что тесты из spec есть и действительно проверяют заявленное (а не проходят тривиально).

## Constraints
Код не меняй, ничего не коммить, тесты повторно не запускай. Ответ — в теле \`worker_done\`: список \`[blocker|major|minor] файл:строка — суть — как исправить\` или «замечаний нет». blocker/major — только с конкретным путём исполнения.

## Ownership
Нет (только чтение).

## Observable acceptance
Отчёт в теле \`worker_done\`.
SPEC
orca orchestration worker-start --spec-file "$S" --task-title "Ревью $ID" --worktree "path:$WT" \
  --agent codex --model gpt-6-luna --effort "$EF" --json | jq -r '.result.dispatchId // .'
