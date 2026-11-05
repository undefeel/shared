# STATUS

Run: run_af6138a06d4e (этап 04); предыдущий run_9a6f2bef2b61 — этапы 02–03, закрыт
Активные: 04-76 `ctx_cc56c765dda6`

## Очередь
Этап 04: волна A закрыта (замер в plan/04-perf.md). Следующие: волна B закрыта (замер в 04-perf.md); волна C: 04-75 → 04-76 → 04-77 → 04-78, параллельно 04-79 (профиль).
Ждут владельца также: Linux runtime-тесты на Omarchy, ручной smoke меню/виджета, проверка по docs/power-loss-check.md.

## Восстановление после обрыва
1. Прочитай этот файл и `plan/PLAN.md`.
2. `orca orchestration run-list --json` → `orca orchestration run-use --id <run_id>`.
3. `orca orchestration task-list --json` и `orca orchestration worker-list --run <run_id> --json`.
4. Потерянная почта: `orca orchestration inbox --terminal run:<run_id> --limit 50 --full`.
5. По каждому активному воркеру проверь коммиты в его ветке, затем продолжай с шага 4 «Порядка работы» в `CLAUDE.md`.

## Пауза: лимит Codex (решение владельца 2026-11-05 — ждать сброса)
Сброс 2026-11-07 10:10. После сброса: `orca orchestration send --to dispatch:ctx_cc56c765dda6 --subject "Resume" --body "Codex usage limit has reset. Continue 04-76 from where you stopped (implementer ctx_b501c88615d5 and fix-round-2 worker ctx_853d842ad32d hit the limit; uncommitted edits in daemon.go and ack_integration_test.go are in the worktree). Restart the implementer via lead.sh or finish yourself."`, затем `plan/tools/wait.sh` в фоне. Дальше 04-77 → 04-78 по очереди.
