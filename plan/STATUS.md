# STATUS

Run: run_c60a6f0bca08 (этап 02)
Активные: 02-01 `ctx_50ea3a04e925`, 02-06 `ctx_5d00c9c8109a`

## Очередь
Волна 2 (реализация folder) — после проекта 02-01.

## Восстановление после обрыва
1. Прочитай этот файл и `plan/PLAN.md`.
2. `orca orchestration run-list --json` → `orca orchestration run-use --id <run_id>`.
3. `orca orchestration task-list --json` и `orca orchestration worker-list --run <run_id> --json`.
4. Потерянная почта: `orca orchestration inbox --terminal run:<run_id> --limit 50 --full`.
5. По каждому активному воркеру проверь коммиты в его ветке, затем продолжай с шага 4 «Порядка работы» в `CLAUDE.md`.
