# 02 — исправления по итогам ревью

Источник находок: `plan/reviews/SUMMARY.md`.

Решения владельца (2026-11-04):
1. Если корень пропал или пуст при непустом индексе, синхронизация останавливается, ошибка видна в меню и виджете. Сброс — только по явному подтверждению.
2. Права: локальные права существующего файла сохраняются, синхронизируется только исполняемый бит.
3. Правка побеждает удаление (файл возвращается) — описать в README.

## Волна 1 — параллельно
| ID | Задача | Находки | Уровень | Исполнитель | Файлы | Зависимости | Статус | `task_id` | `dispatch_id` |
|---|---|---|---|---|---|---|---|---|---|
| 02-01 | Проект исправлений folder + разбиение на задачи | 01-02-01…11 | L3 | sol | `plan/design/folder.md` | — | done | `task_3e3493b9eff1` | `ctx_50ea3a04e925` |
| 02-02 | Relay: лимиты после авторизации, гонка accept, systemd | 01-01-06, -07 | L2 | luna | `internal/relay/server.go`, тесты, `deploy/sharedir-relay.service` | — | done | `task_a9dd43701411` | `ctx_aca933a7f1fa` |
| 02-03 | Discovery: лимит таблицы и частоты | 01-01-03 (UDP) | L2 | luna | `internal/discovery/*` | — | done | `task_a5e353dcfa14` | `ctx_2fa06cb0b7c8` |
| 02-04 | Transport + сервер демона: отзыв доступа, таймауты, лимиты индекса и загрузки | 01-01-01, -02, -03 (incoming), -04, -05 | L3 | sol | `internal/transport/*`, `internal/app/daemon.go` (сеть), `internal/app/relay.go` | — | done | `task_ca25c330076e` | `ctx_3f0f2affa738` |
| 02-05 | Приватный каталог control-сокета, проверка владельца | 01-03-01 | L2 | luna | `internal/config/*`, `internal/app/control*.go`, `macos/SharedirBar/ControlClient.swift` | — | done | `task_2a3f085b8dd9` | `ctx_63df2dd8b8c7` |
| 02-06 | Экранирование plist/unit, гонка activity | 01-03-02, 01-02-12 | L1 | antigravity | `internal/app/service*.go`, `internal/app/activity*.go` | — (в очереди: слот после первого освободившегося) | done | `task_a66d31b76476` | `ctx_5d00c9c8109a` |

02-04 дробить не стал: отзыв доступа, таймауты и лимиты — это одна обёртка listener и один набор callback'ов в `transport.go` и `daemon.go`. Если разделить, две задачи будут править одни и те же строки.

## Хвосты (не блокируют, решить в конце этапа)
- 02-03: при переполнении таблицы discovery (256) вытесняется самая старая запись — флуд поддельными ID может вытеснить сопряжённого пира (связь уйдёт на прописанный адрес или relay). Лучше не вытеснять сопряжённых.
- 02-04: каналы через relay приходят с IP VPS, поэтому лимит 16 на IP ограничивает все relay-соединения разом; `Index` читает до 512 MiB в память.

## Волна 2 — ядро folder (полный проект plan/design/folder.md, вариант B)

| ID | Задача | Уровень | Исполнитель | Зависимости | Статус | `task_id` | `dispatch_id` |
|---|---|---|---|---|---|---|---|
| 02-10 | примитивы безопасного дерева | L3 | sol | — | done | `task_9f2c03fa18ae` | `ctx_dbb7c8f0ac91` |
| 02-11 | no-replace и flush платформ | L3 | sol | 02-10 | done | `task_97de5a4673c1` | `ctx_d80e9ed00308` |
| 02-12 | глубокий снимок и save | L3 | sol | 02-11 | done | `task_8df04ac4b7c6` | `ctx_70c2a09ec7c0` |
| 02-13 | staging полного Scan | L2 | luna | 02-10, 02-12 | done | `task_4572aa79fc95` | `ctx_2a826fb7b265` |
| 02-14 | canonical names и коллизии | L2 | luna | 02-12, 02-13 | done | `task_ff16ffc13a96` | `ctx_3c0f5597b545` |
| 02-15 | binding, latch и миграция | L3 | sol | 02-14 | done | `task_b282941a5c41` | `ctx_1d56dcc9b099` |
| 02-16 | барьер корня в Scan | L2 | luna | 02-15 | done | `task_95fcc8bb5687` | `ctx_c46b97ebf731` |
| 02-17 | capture и уникальные conflicts | L3 | sol | 02-11, 02-15 | done | `task_efec4aa7296e` | `ctx_63d520c09566` |
| 02-18 | intent и recovery | L3 | sol | 02-12, 02-15, 02-17 | done | `task_f9e9cd0474af` | `ctx_d8045c5da3ce` |
| 02-19 | commit coordinator | L3 | sol | 02-16, 02-18 | done | `task_b28f3827a7bf` | `ctx_c338e098d63a` |
| 02-20 | повторное решение и применение | L3 | sol | 02-14, 02-16, 02-19 | done | `task_4f09f0705869` | `ctx_ec752c9d87dd` |
| 02-21 | Exec и локальные права | L2 | luna | 02-20 | done | `task_4fd2a3217c07` | `ctx_516f79518b7b` |
| 02-22 | проверка размера folder | L2 | luna | 02-21 | done | `task_d05e01a8ba91` | `ctx_ba338ad0bdce` |
| 02-23 | безопасная выдача /file | L3 | sol | 02-19, 02-22 | done | `task_a00a59d21e3f` | `ctx_fb7d634b252d` |
| 02-24 | интеграция остановки демона и control | L3 | sol | 02-16, 02-20, 02-23 | done | `task_b26f087a7d18` | `ctx_8919cfefb2f1` |
| 02-25 | watcher после rebind | L2 | luna | 02-24 | done | `task_2edf2066053f` | `ctx_b44e8eb6d5a7` |
| 02-26 | CLI подтверждения | L1 | antigravity | 02-24 | done | `task_b276f2e4ca2a` | `ctx_4fd6990a24df` |
| 02-27 | macOS меню | L2 | luna | 02-24 | done | `task_79b6ee2d8284` | `ctx_77640c0ab8b9` |
| 02-28 | Omarchy виджет | L2 | luna | 02-26 | done | `task_2e483d867673` | `ctx_3f216c07fbb9` |
| 02-29 | фиксация политики README | L1 | haiku | 02-21, 02-24, 02-26 | done | `task_56f08d93d87a` | `ctx_553058e207dd` |
| 02-30 | сквозная crash/concurrency приёмка | L3 | sol | 02-25, 02-26 | done | `task_d4e9297f1093` | `ctx_3897963ce394` |

Задачи идут почти строго по цепочке: следующая стартует только после мержа зависимостей (воркер ответвляется от ветки координатора). Параллельно возможны 02-16 ∥ 02-17 и после 02-24: 02-25 ∥ 02-26 ∥ 02-27 (затем 02-28, 02-29).
Linux-тесты здесь только компилируются; запуск на Omarchy — за владельцем перед закрытием этапа.


Ревью кода: L1–L2 — luna-low, L3 — luna high, только чтение.

Критерии готовности этапа: все находки из SUMMARY закрыты или явно отложены с причиной; `go vet ./...`, `go test -race ./...`, `make build`, `make app` зелёные.
