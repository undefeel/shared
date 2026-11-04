# STATUS

Run: run_af6138a06d4e (этап 04); предыдущий run_9a6f2bef2b61 — этапы 02–03, закрыт
Активные: 04-01 `ctx_6b241d62bd61`

## Очередь
Временно (владелец): код L1–L2 и их ревью — на gemini-medium, GPT экономить (см. plan/04-perf.md).
Этап 04: идёт 04-01 (проект, sol). После мержа — создать задачи 04-10… по plan/design/perf.md; вопросы владельцу из проекта — сначала ему.
Ждут владельца также: Linux runtime-тесты на Omarchy, ручной smoke меню/виджета, проверка по docs/power-loss-check.md.

## Восстановление после обрыва
1. Прочитай этот файл и `plan/PLAN.md`.
2. `orca orchestration run-list --json` → `orca orchestration run-use --id <run_id>`.
3. `orca orchestration task-list --json` и `orca orchestration worker-list --run <run_id> --json`.
4. Потерянная почта: `orca orchestration inbox --terminal run:<run_id> --limit 50 --full`.
5. По каждому активному воркеру проверь коммиты в его ветке, затем продолжай с шага 4 «Порядка работы» в `CLAUDE.md`.
