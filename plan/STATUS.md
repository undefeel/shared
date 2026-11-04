# STATUS

Run: run_af6138a06d4e (этап 04); предыдущий run_9a6f2bef2b61 — этапы 02–03, закрыт
Активные: 04-26 `ctx_e81281d83e45`, 04-14 `ctx_27f7d054fd36`, 04-26-review `ctx_0f71af4127bd`, 04-14-review `ctx_7a7c2f07602c`

## Очередь
Этап 04, волна A (27 задач, plan/04-perf.md). Работают 04-10, 04-23, 04-25, 04-50. Дальше по DAG: фундамент 04-11…04-22 цепочкой (параллельно 04-13 после 04-10), сеть 04-26…04-31, затем 04-24, 04-32, UI 04-52/04-53, harness 04-54. Ревью — саморевью воркера через plan/tools/self-review.sh (gemini medium), координатор только мержит.
Ждут владельца также: Linux runtime-тесты на Omarchy, ручной smoke меню/виджета, проверка по docs/power-loss-check.md.

## Восстановление после обрыва
1. Прочитай этот файл и `plan/PLAN.md`.
2. `orca orchestration run-list --json` → `orca orchestration run-use --id <run_id>`.
3. `orca orchestration task-list --json` и `orca orchestration worker-list --run <run_id> --json`.
4. Потерянная почта: `orca orchestration inbox --terminal run:<run_id> --limit 50 --full`.
5. По каждому активному воркеру проверь коммиты в его ветке, затем продолжай с шага 4 «Порядка работы» в `CLAUDE.md`.
