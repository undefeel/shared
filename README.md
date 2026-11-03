# sharedir

Двусторонняя синхронизация одной папки между macOS и Linux (Omarchy) в локальной сети.
У каждой машины своя полная копия, изменения расходятся в обе стороны за ~1 секунду.
Без сети всё работает локально и догоняется при следующей встрече.

- **Обнаружение:** UDP-броадкаст в LAN (порт 22101); для сетей без броадкаста адрес пира прописывается вручную.
- **Безопасность:** взаимный TLS 1.3. ID устройства — это SHA-256 его сертификата, поэтому
  подключиться и что-либо прочитать может только устройство, которое вы явно сопрягли.
- **Конфликты:** если файл поменяли на обеих машинах, побеждает более поздняя правка,
  а другая сохраняется рядом как `имя.sync-conflict-ДАТА-ВРЕМЯ-УСТРОЙСТВО.ext`.
- **macOS:** нативное приложение меню-бара (SwiftUI `MenuBarExtra`) с демоном внутри: статус, прогресс,
  пауза, сопряжение со сверкой ID, автозапуск при входе.
- **Языки:** английский (базовый) и русский — по языку системы.

## Сборка

```bash
make app        # bin/sharedir.app — приложение меню-бара macOS (нужны только Command Line Tools)
make build      # bin/sharedir (macOS) и bin/sharedir-linux-{amd64,arm64}
```

## Установка

### macOS

```bash
make app && cp -R bin/sharedir.app /Applications/
open /Applications/sharedir.app
```

В меню-баре появится иконка папки. При первом запуске выберите папку для синхронизации
(«Выбрать папку…») — приложение создаст конфиг и ключ и запустит демон. Включите
«Запускать при входе», чтобы sharedir стартовал вместе с системой.

Приложение само запускает встроенный демон и останавливает его при выходе. Если демон уже
работает отдельно (`sharedir install`), приложение просто подключается к нему.

При первом запуске macOS может спросить, разрешить ли входящие подключения, — разрешите.

Без приложения, только демон (например, на сервере):

```bash
mkdir -p ~/.local/bin && cp bin/sharedir ~/.local/bin/
~/.local/bin/sharedir init --folder ~/Sharedir
~/.local/bin/sharedir install     # LaunchAgent, лог: ~/Library/Logs/sharedir.log
```

### Omarchy

Скопируйте `bin/sharedir-linux-amd64` на машину (например, `scp`), затем:

```bash
install -Dm755 sharedir-linux-amd64 ~/.local/bin/sharedir
sharedir init --folder ~/Sharedir
sudo ufw allow 22100/tcp && sudo ufw allow 22101/udp   # в Omarchy ufw по умолчанию режет входящие
sharedir install     # systemd --user, лог: journalctl --user -u sharedir -f
```

Интерфейс для Omarchy 4 — виджет бара в [omarchy/sharedir](omarchy/sharedir) (проверен на 4.0.6):
иконка-статус, панель с переключателем паузы, устройствами, сопряжением и активностью.

```bash
make omarchy                                            # копия в ~/.config/omarchy/plugins/sharedir
omarchy plugin enable sharedir --section right --before omarchy.tray
```

Левый клик — панель, правый — пауза, средний — открыть папку. IPC: `omarchy-shell sharedir
open|toggle|pause|resume|status`. Если бинарник не в `~/.local/bin/sharedir`, задайте путь:
`omarchy bar set sharedir binary /путь/к/sharedir`. Копию, а не симлинк, — потому что оболочка
не перезагружает код по симлинку.

На Omarchy 3 (Waybar) и других окружениях с треем подойдёт `sharedir run --tray`.

## Сопряжение

Каждая машина должна доверять другой, поэтому сопряжение делается **на обеих**.

**Через приложение macOS:** в разделе «Найдено в сети» нажмите «Добавить…», сверьте ID с
экраном того устройства и нажмите «Сопрячь».

**Через терминал:**

```bash
sharedir id                 # на машине A — показать свой ID
sharedir status             # на машине B — найденные устройства с их ID
sharedir pair <ID-машины-A> # на машине B, и наоборот на A
```

## VPN и сети без броадкаста

Броадкаст не проходит через VPN-туннели. В таком случае укажите адрес вручную, достаточно
**на одной стороне**: вторая узнает обратный адрес, когда к ней подключатся.

```bash
sharedir pair <ID> --addr 10.0.0.5          # порт по умолчанию 22100
```

Про Happ/sing-box (VLESS Reality): в LAN он не мешает, локальные адреса идут мимо туннеля.
Но через сам Reality-сервер машины друг друга не видят: это прокси «клиент → сервер», а не общая сеть.
Для синхронизации вне дома — relay.

## Relay: синхронизация через интернет

Relay — посредник на сервере с публичным IP. Оба устройства подключаются к нему исходящим
соединением, relay склеивает их и пересылает байты. Внутри идёт тот же сквозной mTLS, что и в LAN:
relay не может ни прочитать файлы, ни подменить устройство — он видит только шифротекст и
метаданные (IP, время, объём). Без токена relay никого не обслуживает.

Порядок связи: адрес из LAN → прописанные адреса → relay. Дома трафик идёт напрямую; если вы
подключились через relay, а потом пир появился в LAN, связь сама переключится на прямую.

**Сервер (один раз):**

```bash
make linux && scp bin/sharedir-linux-amd64 root@VPS:/usr/local/bin/sharedir
scp deploy/sharedir-relay.service root@VPS:/etc/systemd/system/
ssh root@VPS
  systemctl daemon-reload
  systemd-run --wait --pipe -p DynamicUser=yes -p StateDirectory=sharedir-relay \
    -E SHAREDIR_HOME=/var/lib/sharedir-relay \
    /usr/local/bin/sharedir relay init --addr ПУБЛИЧНЫЙ_IP > /root/sharedir-relay-url.txt
  chmod 600 /root/sharedir-relay-url.txt
  systemctl enable --now sharedir-relay
  ufw allow 22200/tcp comment "sharedir relay"
```

`relay init` печатает адрес вида `sharedir-relay://ID:ТОКЕН@IP:22200`. Это секрет:
по нему можно пользоваться relay (но не читать чужие файлы).

**Устройства:**

```bash
sharedir relay use "$(ssh root@VPS cat /root/sharedir-relay-url.txt)"
sharedir status    # «relay … connected», у пира вне LAN — адрес relay
```

`sharedir relay off` отключает relay. В приложении macOS у пира, связанного через relay,
стоит пометка «через relay».

## Команды

| Команда | Что делает |
|---|---|
| `init [--folder ПУТЬ] [--name ИМЯ]` | создать конфиг и ключ |
| `run [--tray]` | запустить демон (обычно его запускает приложение или сервис); `--tray` — иконка SNI на Linux |
| `status [--json]` | пиры, их состояние и найденные устройства |
| `watch` | статус JSON-строкой при каждом изменении — для интерфейсов |
| `pause` / `resume` | поставить синхронизацию на паузу и продолжить |
| `id` | ID этого устройства |
| `pair ID [--name] [--addr]` / `unpair ID` | управление доверенными устройствами |
| `install` / `uninstall` | автозапуск при входе |
| `relay use АДРЕС` / `relay off` | связь через relay вне локальной сети |
| `relay init --addr IP` / `relay serve` | сам relay (на сервере) |

Настройки лежат в `~/Library/Application Support/sharedir` (macOS) или `~/.config/sharedir` (Linux):
`config.json`, ключ `key.pem`/`cert.pem`, индекс `index.json`. Каталог можно переопределить через `SHAREDIR_HOME`.

## Игнорирование

Служебные файлы (`.DS_Store`, `._*`, `Thumbs.db` и т.п.) не синхронизируются. Свои шаблоны
пишутся в `.sharedirignore` в корне папки, по одному на строку (glob, сопоставляется с именем
и с путём): `node_modules`, `*.tmp`, `build/*`.

## Для разработчиков интерфейсов

`sharedir status --json` и `sharedir watch` отдают тот же JSON, что и управляющий сокет
(`GET /status`). Состояния пиров — коды, а не текст: `searching`, `not_found`, `offline`, `syncing`,
`synced`, `retry`, `paused`, `peer_paused`; типы файлов — `video`, `images`, `docs`, `audio`,
`archive`, `code`, `other`. Ошибки API — по-английски; переводит их сам интерфейс.
Поле `relay` (`addr`, `connected`, `error`) — состояние подключения к relay; у пира, связанного
через relay, `addr` равен `"relay"`.

Переводы: CLI — [internal/i18n](internal/i18n/i18n.go) (язык из `LC_ALL`/`LC_MESSAGES`/`LANG`),
macOS — [macos/Resources/ru.lproj](macos/Resources/ru.lproj/Localizable.strings) (ключи — английские строки).

## Ограничения MVP

- Через relay скорость ограничена каналом сервера: трафик проходит через него дважды.
- Пустые каталоги и симлинки не синхронизируются.
- Файл передаётся целиком (без блочной дельты); при изменении большого файла уходит весь файл.
- Имена, различающиеся только регистром (`A.txt` и `a.txt`), на macOS столкнутся.
- Смена папки или порта в `config.json` требует перезапуска демона.
