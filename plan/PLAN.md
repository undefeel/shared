# sharedir — план

## Архитектура и стек
Двусторонняя синхронизация одной папки между macOS и Linux (Omarchy).
- Go-демон: `cmd/sharedir`, `internal/{app,config,discovery,folder,identity,relay,transport,i18n,tray}`.
- Сеть: UDP-обнаружение в LAN (22101), взаимный TLS 1.3 (22100), ID устройства = SHA-256 сертификата; relay через свой VPS (`internal/relay`, `deploy/sharedir-relay.service`).
- macOS: меню-бар SwiftUI (`macos/SharedirBar`) + встроенный демон, связь через локальный control-сокет.
- Omarchy: виджет бара QML (`omarchy/sharedir`).

Приёмка: `make test`, `make test-macos`, `make test-omarchy`, `GOOS=linux go vet ./...`, (`go test -race ./...`), `go vet ./...`, `make build`, `make app` (Swift собирается на этой машине).

## Этапы
| № | Этап | Подплан | Статус |
|---|---|---|---|
| 01 | Ревью безопасности и багов | `01-review.md` | done |
| 02 | Исправления по итогам ревью | `02-fixes.md` | done |
| 03 | Хвосты этапа 02 | `03-tails.md` | in_progress |
