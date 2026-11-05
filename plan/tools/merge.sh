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
# Политика тестов (plan/TESTING.md): UI-логика и предупреждение о коде без тестов.
CHANGED="$(git diff --name-only HEAD^1 HEAD)"
if grep -q '^macos/' <<<"$CHANGED"; then make test-macos 2>&1 | tail -2; test "${PIPESTATUS[0]}" -eq 0; fi
if grep -q '^omarchy/' <<<"$CHANGED"; then make test-omarchy 2>&1 | grep -E '^ℹ (pass|fail)'; test "${PIPESTATUS[0]}" -eq 0; fi
if grep -E '\.go$' <<<"$CHANGED" | grep -qv '_test\.go$' && ! grep -q '_test\.go$' <<<"$CHANGED"; then
  echo "ВНИМАНИЕ: изменён Go-код без изменений в тестах — проверь обоснование в worker_done"
fi
plan/tools/row.sh "$ID" done >/dev/null
for ds in "$@"; do sed -i '' -E "s/(, )?$ID \`$ds\`//; s/^Активные: , /Активные: /" plan/STATUS.md; done
git add plan && git commit -qm "plan: $ID done

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push -q origin HEAD:main 2>/dev/null || git -c credential.helper="!gh auth git-credential" push -q https://github.com/undefeel/shared.git HEAD:main || echo "ВНИМАНИЕ: push не прошёл"
git -C "$MAIN_CO" merge -q --ff-only "$(git branch --show-current)"
git update-ref refs/remotes/origin/main HEAD  # база для новых worktree Orca
for ds in "$@"; do orca orchestration worker-release --dispatch "$ds" 2>&1 | tail -1; done
orca worktree rm --worktree "path:$WT" 2>&1 | tail -1
git branch -d "$BR"
echo "merged $ID: $(git log --oneline -1)"
