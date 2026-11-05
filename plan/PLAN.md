# sharedir — план

## Архитектура и стек
Двусторонняя синхронизация одной папки между macOS и Linux (Omarchy).
- Go-демон: `cmd/sharedir`, `internal/{app,config,discovery,folder,identity,relay,transport,i18n,tray}`.
- Сеть: UDP-обнаружение в LAN (22101), взаимный TLS 1.3 (22100), протокол между пирами v2 (`docs/protocol-v2.md`), ID устройства = SHA-256 сертификата; relay через свой VPS (`internal/relay`, `deploy/sharedir-relay.service`).
- Хранение: индекс — gob-журнал `index.log` + снимок `index.snapshot` (групповой commit), не `index.json`.
- macOS: меню-бар SwiftUI (`macos/SharedirBar`) + встроенный демон, связь через локальный control-сокет.
- Omarchy: виджет бара QML (`omarchy/sharedir`).

Изменения этапа 04 (решения владельца 2026-11-04): индекс — append-only журнал + компактный снимок вместо полной перезаписи `index.json`; протокол между пирами v2 без обратной совместимости (обе машины обновляются вместе).

Приёмка: `make test`, `make test-macos`, `make test-omarchy`, `GOOS=linux go vet ./...`, (`go test -race ./...`), `go vet ./...`, `make build`, `make app` (Swift собирается на этой машине).

## Этапы
| № | Этап | Подплан | Статус |
|---|---|---|---|
| 01 | Ревью безопасности и багов | `01-review.md` | done |
| 02 | Исправления по итогам ревью | `02-fixes.md` | done |
| 03 | Хвосты этапа 02 | `03-tails.md` | done |
| 04 | Скорость синхронизации и износ SSD | `04-perf.md` | done |
| 05 | Чистка тестов | `05-tests.md` | in_progress |
