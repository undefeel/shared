#!/usr/bin/env bash
# Обновляет строку задачи в подплане: plan/tools/row.sh <ID> <status> [dispatch_id]
# Таблица: последние три колонки — статус | `task_id` | `dispatch_id`.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"
ID="$1"; ST="$2"; DS="${3:-}"
f=$(grep -l "^| $ID " "$ROOT"/plan/[0-9][0-9]-*.md | head -1)
[[ -n "$f" ]] || { echo "нет строки $ID" >&2; exit 2; }
awk -F'|' -v OFS='|' -v id="$ID" -v st="$ST" -v ds="$DS" '
  $2 == " " id " " { n=NF; $(n-3)=" " st " "; if (ds != "") $(n-1)=" `" ds "` " }
  { print }' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
grep "^| $ID " "$f"
