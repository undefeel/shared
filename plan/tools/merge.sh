#!/usr/bin/env bash
# Мерж задачи: plan/tools/merge.sh <ID> <dispatch> [ещё dispatch, например ревью...]
# merge --no-ff ветки undefeel/t<ID> → vet/test/build → коммит plan → push в main →
# ff основного checkout → release воркеров → удаление worktree и ветки.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"
ID="$1"; shift
BR="undefeel/t$ID"; WT="$(dirname "$ROOT")/t$ID"
MAIN_CO="/Users/kodoku/Work/sharedir"
git merge -q --no-ff "$BR" -m "merge $ID

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
go vet ./... && GOOS=linux go vet ./...
go test -race ./... 2>&1 | grep -v 'no test files' | tail -15
test "${PIPESTATUS[0]}" -eq 0
make build >/dev/null
plan/tools/row.sh "$ID" done >/dev/null
for ds in "$@"; do sed -i '' -E "s/(, )?$ID \`$ds\`//; s/^Активные: , /Активные: /" plan/STATUS.md; done
git add plan && git commit -qm "plan: $ID done

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push -q origin HEAD:main
git -C "$MAIN_CO" fetch -q origin && git -C "$MAIN_CO" merge -q --ff-only origin/main
for ds in "$@"; do orca orchestration worker-release --dispatch "$ds" 2>&1 | tail -1; done
orca worktree rm --worktree "path:$WT" 2>&1 | tail -1
git branch -d "$BR"
echo "merged $ID: $(git log --oneline -1)"
