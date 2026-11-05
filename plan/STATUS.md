# STATUS

Run: run_8e7366dcb679 (этап 05); run_af6138a06d4e — этап 04, закрыт; run_9a6f2bef2b61 — этапы 02–03, закрыт
Активные: 05-02 `ctx_f3a42988277b`

## Очередь
Этап 04 закрыт (итог и хвосты — конец plan/04-perf.md). Этап 05 «Чистка тестов» (plan/05-tests.md) идёт: 05-01 и 05-02 параллельно.
Политика тестов: plan/TESTING.md (2026-11-05) — минимум тестов и прогонов; подключена к self-review.sh и merge.sh.
Ждут владельца также: Linux runtime-тесты на Omarchy, ручной smoke меню/виджета, проверка по docs/power-loss-check.md.

## Восстановление после обрыва
1. Прочитай этот файл и `plan/PLAN.md`.
2. `orca orchestration run-list --json` → `orca orchestration run-use --id <run_id>`.
3. `orca orchestration task-list --json` и `orca orchestration worker-list --run <run_id> --json`.
4. Потерянная почта: `orca orchestration inbox --terminal run:<run_id> --limit 50 --full`.
5. По каждому активному воркеру проверь коммиты в его ветке, затем продолжай с шага 4 «Порядка работы» в `CLAUDE.md`.
