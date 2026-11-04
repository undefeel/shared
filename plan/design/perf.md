# 04 — скорость синхронизации и износ SSD

Технический проект по `plan/04-perf.md`, состояние исходников на 2026-11-04. Реализация здесь не производится. Приоритет имеют решения владельца из входного плана и уточнение от 2026-11-04 в `plan/design/folder.md`: capture удаляется сразу после долговечного commit; постоянного recovery manifest нет. Старые абзацы того проекта про бесконечное хранение capture отменены этим уточнением.

## Решения владельца по проекту (2026-11-04) — важнее текста ниже

1. **R3: один аппаратный flush на группу (вопрос 1 закрыт).** Отказ от FULLFSYNC каждого файла принят владельцем осознанно, это ослабление решения этапа 02. Схема:
   - G1: каждый подготовленный incoming inode и подготовленный каталог — обычный `fsync` (Linux: как раньше, fsync каждого файла и каталога); затем **один** `F_FULLFSYNC` на каждое устройство, где лежат подготовленные файлы группы (группировать по `st_dev`; state и root на разных устройствах — по одному на каждое).
   - G2: intent — fsync + один FULLFSYNC (на устройстве state); G4: commit frame — fsync + один FULLFSYNC; compaction — как в R2.
   - Итого на macOS ≈ 3 FULLFSYNC на группу (+1 на каждое дополнительное устройство), а не F+2G.
   - R20 пересчитать: FULLFSYNC ≤ 3G + (число устройств − 1)·G + 12; file fsync ≤ F + 2G + 12; остальные формулы без изменений. Профиль «консервативный macOS» из R20 не реализуется.
   - `docs/power-loss-check.md` (04-57) и README (04-56) явно описывают эту гарантию: при отключении питания теряется не больше незавершённой группы; опубликованная группа долговечна, если накопитель соблюдает F_FULLFSYNC.
2. Вопросы 2 и 3 — приняты как в проекте (точное подавление собственных событий, неоднозначные проходят инкрементальную проверку; стартовые лимиты 30 суток / 256 MiB / 65536 блоков).
3. **Волны.** A — фундамент и сеть, основное ускорение: 04-10…04-32, 04-50, 04-52, 04-53, 04-54 (harness в волне A зависит только от 04-32 и 04-50; сценарии R16/R5 добавляются в волнах B/C). B — 04-33…04-41 (watcher, стабильность, Scan staging, инкрементальный скан, расписание, исключения, клоны). C — 04-42…04-49, 04-51, 04-55…04-57 (дельта, ACK/GC tombstone, сводки переподключений, crash suite, документация). После каждой волны — замер harness и отчёт владельцу.

## Общие инварианты

Сохраняются rooted fd/no-follow, NFC/Disk, no-replace, проверка содержимого, локальные rwx и передача Exec, edit побеждает конкурентный delete, durable latch пустого/сменившегося корня. Ошибка durability никогда не означает успешный apply. Recovery идёт до Scan, watcher и сетевой выдачи. Защита от произвольного локального писателя имеет ровно ограничения этапа 02: запись в заменённый inode через открытый fd после commit может потеряться. Новых зависимостей нет: gob, gzip/flate, sha256, crc32, binary из stdlib; платформенные операции через имеющийся x/sys.

Под «атомарностью группы» понимается публикация индекса целиком. Внешний процесс может видеть промежуточные изменения разных файлов дерева; межфайловая атомарность ФС не обещается. Одна группа — один intent и одна запись журнала индекса, но не один системный вызов fsync: каждый изменённый inode/каталог требует соответствующей обработки. Intent операций и append-only журнал индекса — разные сущности.

## R1 — групповой commit

Один coordinator на Folder собирает готовые операции всех runPeer: до 64 записей, 8 MiB подготовленных данных либо 25 ms от первой готовой операции, что раньше. При наличии следующей готовой пачки берёт её сразу; таймер не продлевается. Файл больше 8 MiB — отдельная группа. Очередь ограничена 128 записями и 32 MiB данных в staging (один большой файл допускается отдельно); backpressure останавливает download. Download/hash/clone вне scanMu. Один canonical target допускается в группе один раз; rename/conflict со связанными именами резервируют все имена, зависимости parent/child разводятся по группам. Группа не смешивает confirmed generation/binding.

Под scanMu → saveMu повторяются ctx, binding, root guard, generation и decide для каждой операции; mu удерживается только для глубоких копий и публикации. При устаревании операция возвращается в планирование до записи intent. После durable intent отмена ctx уже не прерывает commit посередине: coordinator завершает commit либо оставляет intent и блокирует Folder. При ошибке одного mutate нельзя зафиксировать успешное подмножество: вся группа остаётся незавершённой для recovery. Done, applied, notify и ACK выдаются только после durable записи всей группы.

### Последовательность записи, flush и recovery

Нумерация сопоставлена с текущими `commitLocked`/`cleanupCommit` в `commit.go`, `writeIntent`/`recoverIntent`/`finishNewIntent` в `journal.go` и шагами 1–5 проекта folder.

| Фаза | Действия и точка долговечности | Обрыв/ошибка и восстановление |
|---|---|---|
| G0, до текущего шага 1 | Download в уникальные 0600 tmp рядом с target; точные size/hash, chmod/mtime по fd. Снять Before, план After и ожидаемые identities. Пока дерево не меняется. | Нет intent: tmp можно убрать при запуске; индекс и пользовательские имена прежние. Не удалять файлы только по похожему имени без проверки служебного формата. |
| G1, шаг 1 | Синхронизировать каждый incoming inode; metadata-only тоже новый inode. Для консервативного режима macOS fsync + FULLFSYNC каждого файла; Linux fsync. Подготовленные каталоги синхронизировать от листа к корню. | Старое дерево остаётся авторитетным; отсутствует обязательство публиковать скачанное. |
| G2, шаг 2 `writeIntent` | Записать весь group-intent.tmp, fsync + FULLFSYNC на macOS, Close, rename → intent.json, fsync state-dir. Дерево нельзя менять до успешного возврата. | До rename старый индекс; после rename валидный intent означает незавершённую группу, даже если G2 не успел вернуть успех. Невалидный intent блокирует, а не угадывает. |
| G3a, capture, шаг 3 | По очереди capture каждого старого inode no-replace, повторная проверка захваченных байтов; fsync каждого затронутого родителя до установки target. Capture существует до любого разрушительного действия. | Для каждого элемента с capture восстановить его no-replace; занятое имя сохраняется, capture становится видимым conflict. Capture ещё не сделан — проверить прежний target; неоднозначность блокирует. |
| G3b, mutate, шаг 3 | Установить incoming no-replace / оставить target отсутствующим для delete; зарегистрировать все созданные родители. fsync изменённых каталогов, включая цепочки родителей, с дедупликацией fd identity внутри фазы. Изменение внешним процессом — conflict/blocked, никогда overwrite. | Без commit-записи группа не опубликована. Восстановить captures; установленный новый target не удалять вслепую: сохранить как conflict, если требуется освободить имя и доказана его identity. Новые файлы без Before оставить для последующего Scan; не вызывать `finishNewIntent` по одному элементу. |
| G4, вместо шага 4 `persistLocked` | Append одного целого Commit frame, содержащего все After/metadata и TxID. Проверить short write, fsync журнала + FULLFSYNC macOS. Новый сегмент предварительно создан и синхронизирован с каталогом. Только после flush обновить durableGeneration, files/clock/seq целиком под mu; затем Done/ACK. | Полный frame с верной цепочкой и checksum после рестарта считается committed, даже если вызывающий код не получил успех; проверяются targets. Частичный хвост не committed. Если flush вернул ошибку — текущий процесс blocked, публикации нет; решение принимает recovery по диску. |
| G5, шаг 5 `cleanupCommit` | Удалить все соответствующие capture (identity mismatch → promote conflict), затем intent; fsync родителей и state-dir. Не ждать retention. Перед следующей группой незавершённый cleanup должен закончиться, чтобы не перезаписать единственный intent. | При committed frame только cleanup; повторное удаление отсутствующего capture допустимо. При сбое cleanup intent восстанавливается как marker, как сейчас. Ошибка не отменяет уже durable commit. |

Recovery проверяет TxID, base/new generation, binding и **все** Before/After, а не только номер поколения. Для committed live targets проверяет hash/size/Exec; расхождение сохраняет данные и блокирует `recovery_ambiguous`. Незавершённая группа восстанавливается поэлементно идемпотентно; повторный обрыв recovery безопасен: не удаляется последний экземпляр данных, conflicts создаются no-replace. Если после rollback возникли видимые conflicts/новые target, полный Scan после завершения recovery вводит их обычными локальными версиями. Старый индекс не публикуется как доказательство состояния промежуточного дерева. Для однозначного повторения conflict имя заранее хранится в intent; при коллизии новое имя дописывается долговечно до promotion. Неизвестная identity, повреждённый frame в середине или потеря обеих копий — blocked. Не выполнять Scan поверх незавершённого recovery.

Затронуты: `internal/folder/commit.go`, `journal.go`, `apply.go`, новые `group.go`, `group_recovery.go` и тесты. Почему: дорогие границы амортизируются на группу, а сериализация решений сохраняется.

## R2 — журнал и снимок

Локальная schema=3. Каталог состояния доверенный, файлы 0600. `index.snapshot` и `index.log` имеют стабильные имена, `intent.json` остаётся JSON для диагностики и legacy recovery. Снимок содержит schema, store UUID/epoch, generation, clock, полный sorted files с Disk/Vector, binding, blocked с token, confirmedGeneration, peer-set epoch, ACK и GC floor. Не содержит lastScan, progress, статистику, hash-cache. Лог содержит только изменения и управляющие транзакции; удаление записи при GC отлично от tombstone.

Формат snapshot: magic `SHDSNP03` (8 байт), uint32 version, uint8 codec (0=raw,1=gzip), uint64 storedLen, uint64 plainLen, SHA-256 plain payload (32 байта), payload. Числа big-endian. Payload — самостоятельный gob конкретной структуры без interface/polymorphic types. Списки сортированы, Vector сериализован sorted парами. Декодер требует EOF, проверяет длины до аллокации, имена, дубликаты, размеры/векторы. Снимок ограничен 256 MiB распакованного payload; превышение — понятная остановка, не частичная загрузка.

Лог: заголовок magic `SHDLOG03`, version, store UUID и base snapshot generation. Каждый frame: uint32 storedLen, uint32 plainLen, uint8 codec, uint64 generation, 16-byte TxID, uint32 CRC32C всего заголовка frame и stored payload (кроме поля CRC), payload. Payload gob: previous generation, kind=Commit/Control/PeerAck/GC, операции с Before-digest и After/DeleteRecord, итоговые clock и изменившиеся управляющие поля. Frame максимум 16 MiB plain/stored, записи больше лимита разбиваются до intent (для Scan — транзакции метаданных без изменения дерева; полнота кандидата проверяется до первой публикации). Для полного Scan, требующего атомарного обновления больше frame limit, используется snapshot commit вместо частичной публикации; это редкий O(N) путь. Запрещено разбивать один уже подготовленный атомарный кандидат на наблюдаемые частичные изменения.

Каждый gob frame независим, без продолжения словаря предыдущего gob. gzip BestSpeed применяется при payload ≥4 KiB только если экономит ≥12.5%; CRC проверяется до распаковки, распаковка строго ограничена plainLen. Короткий последний frame отбрасывается до последней валидной границы с truncate+fsync **до** следующего append; полный frame с неверным CRC не считается обычным оборванным хвостом, Folder blocked. Последовательность generation без дыр и store UUID обязательны. В журналах нет сетевых Disk из пира.

### Сжатие журнала (compaction)

Порог: 8 MiB лога либо 4096 frames; дополнительно после GC при доле убранных записей ≥25%. В idle проверить порог, но не писать снимок по таймеру без изменений. Compaction сериализована scanMu/saveMu, между группами, после cleanup intent; удерживает только одну полную копию состояния.

1. Зафиксировать generation G, записать snapshot.tmp полностью, fsync + FULLFSYNC macOS, Close.
2. Rename → index.snapshot, fsync state-dir. Старый log пока не трогать. Обрыв до/после: либо прежний snapshot + полный log, либо новый snapshot G + прежний log; frames ≤G пропускаются только после проверки UUID/целостности.
3. Создать log.tmp с base=G, fsync + FULLFSYNC macOS, Close, rename → index.log, fsync state-dir. Обрыв до rename оставляет прежний log; после rename — пустой log и уже durable snapshot G. Нельзя сначала truncate старый log.
4. Удалить мусорные tmp. Обрыв на этом шаге влияет только на уборку. Старый snapshot не нужен для штатного replay; повреждение подтверждённого snapshot — ошибка, не автоматическое откатывание ACK/latch.

Snapshot commit большого Scan следует тому же порядку, с публикацией после шага 2; intent при изменении дерева должен ссылаться на TxID snapshot. В обычном потоке R1 всегда один log frame. Затронуты: `index.go`, новые `store.go`, `store_codec.go`, `store_compact.go`, `store_replay.go`, `journal.go`, `rootstate.go`.

## R3 — FULLFSYNC и BARRIERFSYNC

Нельзя считать F_BARRIERFSYNC эквивалентом аппаратного durable flush. Проект не выводит безопасность из предположения «FULLFSYNC журнала на системном диске сбросит данные внешнего APFS». State и Root могут находиться на разных устройствах. Консервативный обязательный режим сохраняет FULLFSYNC в G1 для каждого подготовленного inode и в G2 для intent; G4 — FULLFSYNC commit, compaction — FULLFSYNC snapshot и нового log. fsync каталогов во всех перечисленных фазах остаётся. Это частичное выполнение буквального R3, конфликт вынесен владельцу, гарантии не ослаблены.

Таблица допустимых замен/отказов:

| Место | Изменение | Оставшаяся гарантия |
|---|---|---|
| Повторный Sync в `download` и `commitLocked` одного неизменного incoming | Один Sync в G1 после последнего chmod/mtime вместо двух | Полные bytes+metadata durable до intent; флаг prepared не переносится через изменение inode/metadata. |
| Полная запись index.json на файл | Заменить на G4 frame, один FULLFSYNC на группу | Все After группы durable до notify; O(изменения), а не O(весь индекс). |
| Повторный flush одного каталога в одной фазе | Дедуп по dev/inode | Все directory entries этой фазы дошли до fsync; между capture и install дедуп запрещён. |
| lastScan, progress | Вообще без persist/flush | Только volatile диагностика; не участвует в recovery/решениях версий. |
| Дополнительный временный промежуточный файл, удаляемый до G1 | Без FULLFSYNC | Файл не опубликован, его потеря означает повтор download; не относится к incoming/capture/intent. |
| Необязательный ранний flush очереди tmp | fsync + BARRIERFSYNC разрешён только как подготовка, затем обязательный G1 FULLFSYNC | Только порядок/прогресс I/O до G1, никакого durable успеха. В базовой реализации этот вызов лучше вообще не делать. |

Замены G1/G2 на BARRIERFSYNC сейчас **не разрешены**. Потенциальная оптимизация — fsync всех подготовленных файлов одного устройства и один FULLFSYNC этого устройства на границе подготовки группы, плюс отдельный durable intent до mutate. Нужны подтверждённый платформенный контракт и power-loss тест на поддерживаемых ФС; до этого ветка не реализуется. На Linux fsync остаётся для каждого файла и каталога; деление на Full/Barrier отражается в hooks, но не эмулирует аппаратный flush неподдержанным ioctl. Файлы: `rootfs_darwin.go`, `rootfs_linux.go`, `index.go`, `commit.go`, `apply.go`, тесты платформ.

## R4 — служебные поля

lastScan/scanError/progress/reconnect counters остаются в памяти и Status. No-op Scan — ноль store writes/flush и без generation tick. Binding, latch, confirmedGeneration, vectors, clock, ACK и GC floor служебными в этом смысле не являются: они durable Control frames. Почему: рестарт не должен снимать остановку или забывать причинность. Файлы: `index.go`, `scan.go`, `rootstate.go`.

## R5 — tombstone GC и подтверждения

Хранить DeletedAt (локальное время первого durable tombstone) и идентификатор удаления = hash(name, Vector). Retention 30 суток после первого наблюдения, не от remote timestamp. GC разрешён только для точного tombstone, durably подтверждённого всеми **сопряжёнными** пирами текущего peer-set epoch; offline пир задерживает GC без срока. ACK означает durable применение удаления либо durable конкурентного edit с Vector, доминирующим tombstone; простой приём /index не ACK. Пакеты ACK сжимаются по максимальным векторам удаления на имя, но нельзя заменять их общим clock разных устройств.

После GC хранить компактный per-origin causal floor/retired epoch, запрещающий возврат старых записей. Если вектора имеют дыры, floor не повышать выше непрерывно подтверждённого префикса: sparse исключения остаются. GC не обязан экономить память до достижения безопасного floor. Peer с истёкшей/чужой store epoch проходит полный bootstrap, его старый индекс не импортируется как новые локальные edits. Новый pair стартует с текущего checkpoint, подтверждает baseline до участия в GC. Unpair — явное удаление из membership, не автоматический timeout; повторный pair новый epoch. Скачок часов не заменяет ACK, сомнительные/отрицательные интервалы откладывают GC. GC frame durable до compaction; restart не воскрешает tombstone и не забывает floor. Файлы: новые `tombstone.go`, `peerstate.go`, `transport/ack.go`, `app/daemon.go`, `index.go`, `store_codec.go`.

## R6 — собственные события

Не выключать watcher и не игнорировать путь на N секунд. Coordinator регистрирует ожидаемый fingerprint (generation, dev/inode, size, mtime, Exec, hash), по завершении G4 выдаёт его фильтру событий. Точные собственные события не запускают Scan; служебные tmp исключены всегда. Смешанные/неопределённые события остаются dirty, чужая запись с тем же именем/mtime не теряется: fingerprint с stat без hash не доказательство, при сомнении incremental hash. Событие удаления проверяет факт отсутствия и текущую запись. Поздние/coalesced события не сопоставляются лишь по времени. Гарантия R6 относится к доказанным собственным изменениям; неоднозначные требуют проверки. Файлы: новые `watch_events.go`, `group.go`, `scan_incremental.go`, оба `watch_*.go`.

## R7 — инкрементальный Scan

Watch API отдаёт batch относительных путей + FullRequired/RootChanged, generation. FSEvents MustScanSubDirs/dropped/history loss, inotify overflow, ошибка addTree/закрытие stream → полный Scan. Rename отмечает оба родителя; directory events сканируют затронутое поддерево, добавление дерева сначала устанавливает watches и затем проверяет содержимое. Неизвестное старое имя → полный обход. Ignore-file change → полный обход. Канонические коллизии проверяются против всей map. Ошибка ReadDir/stat/hash отменяет весь кандидат затронутого scope без удалений; отсутствие пути устанавливается повторно через pinned parent. Root empty latch проверяется полным подтверждающим обходом до публикации удаления последнего учитываемого файла. Файлы: `watch_darwin.go`, `watch_linux.go`, новые `watch_events.go`, `scan_incremental.go`, `app/daemon.go`.

## R8 — стабильность

N=1 s по умолчанию, две одинаковые size+mtime+inode+Exec пробы с интервалом N, затем stableHash с fstat до/после. Pending map bounded 10000; превышение переводит в full-rescan-needed. Таймер проб не спит под mutex. Изменение сбрасывает deadline; непрерывно пишущийся файл откладывается и виден как pending. В полном Scan нестабильность отменяет весь кандидат, чтобы не публиковать частичный обход. Incoming уже проверен download и не ждёт N секунд; последующая внешняя правка идёт обычным путём. Файлы: новый `stability.go`, `scan.go`, `scan_incremental.go`.

## R9 — расписание

Полный Scan на запуске после recovery, затем каждые 12 минут при здоровом watcher, каждые 60 секунд при отсутствии/ошибке watcher; overflow немедленно. Timer не сбрасывается бесконечным потоком событий. Full Scan принудительно проверяет hash даже при том же stat (выявляет восстановленный mtime); hash cache ускоряет событийный путь. Один выполняющийся full scan, последующие сигналы объединяются с сохранением dirty после старта. Файлы: `app/daemon.go`, новый `app/scan_schedule.go`, `folder/scan.go`, `scan_cache.go`.

## R10 — scanMu без обхода

Под короткой блокировкой снять immutable baseline/generation/ignore revision и lease rootFS; обход и hash — без scanMu/mu. Close/rebind ждёт leases вне блокировки commit, чтобы не получить deadlock. При возврате scanMu повторить guardRoot, generation, baseline revision, stat всех кандидатов и родителей, отсутствие удаляемых записей; конкурирующий commit/rebind отменяет весь кандидат и ставит retry. События с начала обхода остаются pending; финальная stat проверка не заменяет capture для операций Apply. Непрерывные внешние записи могут задержать Scan, это соответствует прежней гарантии. Файлы: `scan.go`, `rootfs.go`, `rootstate.go`, новые `scan_stage.go`, `scan_incremental.go`.

## Протокол v2

HTTP/2 поверх существующего mTLS 1.3 и relay без расшифровки на VPS. Старый v1 не поддерживается; endpoint без версии не является fallback. Включая LAN выполняется прикладной handshake после TLS: `POST /hello`, JSON `{protocol:2, storeEpoch, peerSetEpoch, features:["files","gzip","delta","ack"]}`; ответ той же формы с лимитами. Для каждого запроса заголовок `Sharedir-Protocol: 2`; сервер 426 с JSON `{code:"protocol_incompatible",expected:2,received:...}` при неверной/отсутствующей версии. 404 старого сервера на /hello клиент превращает в ту же понятную ошибку. Не считать успешный TLS доказательством совместимости. Peer identity берётся из сертификата, не из JSON.

`GET /index?epoch=...&since=...&wait=...` — JSON `{protocol,storeEpoch,cursor,reset,files}`; FileInfo без Disk/binding, с Vector/Exec и deletion ID. Epoch меняется при reset, не обычном рестарте; cursor durable generation. С начала можно полный baseline, затем изменения; compacted cursor → явный reset/baseline, а не молчаливый пустой ответ. `GET /file?name=&hash=` — одно точное содержимое; `POST /files` — пачка; `POST /delta` — блоки; `POST /ack` — durable подтверждения. У всех endpoints общий auth, blocked=503, paused/generation проверяются до выдачи и после long poll. Control API локальной машины сохраняет отдельный контракт.

### R11 — параллельные downloads

Config.DownloadConcurrency default=6, допустимо 1–8. Folder-wide semaphore общий для всех runPeer, а не 6 на каждый пир. Очередь справедлива между пирами, fd/body всегда закрываются при cancel; prepared results поступают coordinator. Пачка занимает один stream slot, её элементы ограничены общим staging budget. Нет goroutine на каждый файл. Файлы: `config/config.go`, `folder/apply.go`, новые `folder/download_pool.go`, `app/daemon.go`.

### R12 — /files

Запрос JSON до 256 KiB: `{protocol:2,items:[{id,name,hash,size}]}`, ≤64 уникальных id/имён, каждый файл ≤256 KiB, сумма ≤8 MiB. Content-Type ответа `application/x-sharedir-files;version=2`. Magic `SHDFIL02`, далее frames: uint32 headerLen (≤4096), JSON header `{id,status,name,hash,size,encoding,wireSize}`, затем ровно wireSize bytes. status=ok/not_found/version_changed/blocked; у ошибки body=0. Финальный frame `{end:true,count}` обязателен. size — plain bytes, wireSize ограничен 256 KiB+1 KiB; encoding identity/gzip. Headers и счётчики проверяются до выделения памяти; неизвестный/повторный id ошибка потока. Сервер bounded-buffer кодирует один мелкий файл, не всю пачку. Читатель учитывает отдельно wire/plain bytes, запрещает zip bomb и лишние bytes; проверяет SHA-256. Успешные завершённые frames сохраняются, обрыв повторяет только оставшиеся; ошибочный файл не портит остальные. Версия читается через OpenVersion по fd, никогда pathname из query напрямую. Файлы: новые `transport/files.go`, `files_client.go`, `batch_codec.go`, `folder/read.go`.

### R13 — retry без сброса cursor

Разделить fetched cursor и применённые записи: bounded pending map по (peer epoch,name,Vector,hash), а не один failed count. После получения index запомнить его cursor и не запрашивать повторно уже принятый индекс при ошибке одного тела. Retry 250 ms…30 s exponential+jitter по файлу, новый remote version заменяет pending; успешно durable применённое удаляется. ACK только после commit. Version_changed вызывает адресный запрос актуальной записи (`GET /index?name=...`), не since=0. При обрыве соединения cursor сохраняется в памяти; после рестарта допустим baseline для восстановления volatile pending, но без повторной загрузки совпавших файлов. Epoch/reset сервера — единственная штатная причина смены baseline. Pending overflow прекращает новые poll до освобождения места, не теряет работы. Файлы: `app/daemon.go`, новые `app/peer_queue.go`, `transport/transport.go`, `folder/apply.go`.

### R14 — сжатие

Через relay gzip BestSpeed для текстовых/сжимаемых данных, LAN default identity. Клиент явно передаёт `Accept-Encoding`, автоматическую распаковку net/http отключить, чтобы считать wire bytes и ограничения. Для /file и /delta поток gzip с decoded limit; /files кодирует каждый frame отдельно как выше. Не сжимать известные jpg/png/zip/gz/mp4, файлы <1 KiB; проба первых 32 KiB требует ≥12.5% экономии, затем поток без полного буфера. Content-Length не считается plain size. Итоговый hash всегда от plain. Relay конфигурация, сертификаты и лимиты idle/body не обходятся. Файлы: новый `transport/compress.go`, `files.go`, `files_client.go`, `transport.go`, `delta.go`.

### R15 — версия и Status

Handshake обязателен при каждом новом peer connection/address. В PeerStatus отдельный error code/message с expected/received, UI показывает несовместимость, никогда «синхронизировано». Ошибка постоянная до обновления/переподключения, retry handshake раз в 60 s, лог дедуплицирован. Файлы: `transport/transport.go`, новый `transport/protocol.go`, `app/daemon.go`, `macos/SharedirBar/Store.swift`, `MenuContent.swift`, `omarchy/sharedir/{Service.qml,Widget.qml,Strings.js}` и локализации.

## R16 — повторное использование содержимого

Индекс hash→список живых canonical names строится в памяти. Перед загрузкой проверить доступный local fd стабильным hash и Exec отдельно; совпадение target исключает передачу тела, но Vector/Exec проходит durable commit. Для нового target clone в уникальный tmp: APFS clonefile через rooted пути/fd API с проверенными родителями, Linux ioctl FICLONE; при недоступном безопасном clone API использовать копию из удерживаемого fd, не абсолютный небезопасный путь. Unsupported/cross-device → bounded copy, другие I/O ошибки не скрывать. Клон проверяется по hash и идёт G1; исходник не заменяется hardlink, изменение клона не меняет источник. Файлы: новые `clone.go`, `clone_darwin.go`, `clone_linux.go`, `apply.go`.

## R17 — дельта больших файлов

Порог 8 MiB, блок 1 MiB (последний короче), максимум 65536 блоков; больше — full stream. `POST /delta` JSON `{protocol:2,name,targetHash,targetSize,baseHash,baseSize,blockSize,blocks:[sha256...]}` до 5 MiB. Сервер OpenVersion целевой версии, вычисляет хеши блоков, выдаёт magic `SHDDEL02`, bounded JSON header и frames `{offset,length,hash,encoding,wireSize}` + тело только несовпавших блоков; end с target hash/size и числом frames. Все offsets кратны blockSize, длины/арифметика проверены; overlaps/дубликаты/пропуски изменённых блоков ошибка. Отсутствующий/изменённый target → version_changed; если изменения ≥70% — ответ full_required. Это fixed-block delta, вставка в начало может потребовать весь файл.

Получатель stableHash базового fd, clone в новый tmp; если clone не поддержан — полная локальная копия (экономия сети остаётся, снижение SSD-записи не обещается). Применяет только полученные блоки через WriteAt, truncate до targetSize, полный SHA-256 результата, stat базового fd до/после, G1…G5. Никогда patch живого inode. Изменившаяся база/неверный hash → отбросить tmp и повторить полный fetch; incoming не может затереть локальную правку, re-decide обязателен. Файлы: новые `transport/delta.go`, `delta_client.go`, `folder/delta.go`, clone helpers, `apply.go`.

## R18 — встроенные исключения

Добавить компоненты `node_modules`, `.venv`, `__pycache__`, `.next`, `dist`, временные имена `.sharedir-tmp-*`, `*.swp`, `*.swo`, `*~`, `.#*`, `#*#`, `~$*`, `*.crdownload`, `*.part`, `*.download`. `.git` остаётся включён. Правила едины для watcher/Scan/Apply; каталоги исключаются по компоненту, не substring. Встроенные исключения имеют приоритет, пользовательский ignore добавляет ограничения. Уже индексированные файлы, попавшие под новые исключения, не превращаются в tombstone; сохраняются как ignored records, не выдаются в активный exchange. Удалять старые записи можно только отдельной безопасной политикой, не при первом Scan. Файлы: новый `folder/ignore.go`, `scan.go`, `apply.go`, `watch_events.go`.

## R19 — логи

Bounded writer для файлового режима: 5 MiB × 3 файла, rename rotation, без fsync на каждую строку; systemd stderr остаётся под journald и не дублируется в файл. Повторы по (peer,error code) агрегировать, первое событие сразу, summary не чаще 5 минут с count и временем последнего; восстановление соединения сразу. Для непрерывных reconnect failures summary каждые 5 минут даже без wasConnected. Токены/сертификаты/relay secrets не логировать. Потеря диагностического хвоста при питании допустима и не влияет на commit. Файлы: новые `app/loglimit.go`, `app/reconnect_log.go`, `app/daemon.go`, `cmd/sharedir/main.go`.

## Конкурирующие пути

| Участники | Сериализация и повторная проверка |
|---|---|
| watcher / incremental / full Scan | Dirty scopes объединяются; один Scan builder, события после baseline сохраняются. Full отменяет несовместимый incremental кандидат. Own-event fingerprint не отменяет неизвестные внешние изменения. |
| Scan / downloads / несколько runPeer | Download только staging, общий semaphore. Scan lease не удерживает scanMu; coordinator решает все записи повторно под scanMu. Изменившийся baseline Scan отбрасывает целиком. |
| group commit / локальный процесс | pinned parents, stableHash, capture и no-replace; несовпадение после capture не overwrite. После G2 ошибка блокирует группу, до G2 допускается перепланировать отдельные операции. |
| pause / group | Pause останавливает приём новых задач, отменяет download; начавшийся после G2 commit дренируется. Ни pause, ни resume не снимают root latch. Не держать daemon.mu при ожидании drain. |
| rebind / confirm / leases | Сначала запрет новых leases и отмена поколения, drain coordinator/watch/download/scan; затем scanMu → saveMu для нового binding/latch и durable Control/snapshot. Старый tmp/событие/ACK не проходят generation check. Stale token=409. |
| restart / power loss | Проверка snapshot/log → legacy или group recovery → binding/latch → полный Scan → watcher/network. Не запускать GC до восстановления ACK/membership. Неопределённость оставляет понятный blocked Status. |

## Миграция

Под эксклюзивным lock домашнего каталога остановленного демона. Если schema=3 snapshot существует — валидировать и replay, `index.json` не fallback. Если snapshot отсутствует — прочитать schema=0/2 JSON, проверить все имена/дубликаты/Vector и **сначала восстановить legacy intent** существующим алгоритмом на старом JSON, включая `finishNewIntent`; не интерпретировать legacy intent как группу. Не удалять архив при ошибке.

Сохранить Schema=2 Binding (configured path/device/inode), Blocked code/message/token, ConfirmedGeneration, Generation, Clock, все Files и Disk без нормализации потерь. Проверить корень как прежде; миграция не подтверждает rebind и не снимает latch. Непустой schema=0 без binding остаётся blocked до явного действия; пустой bootstrap по политике 02. Legacy tombstone без DeletedAt получает now и неподтверждённые ACK, так что немедленный GC невозможен.

Скопировать исходный JSON в уникальный migration archive, fsync+FULLFSYNC, fsync state-dir. Создать snapshot schema=3 по R2 и пустой log. Наличие durable snapshot — точка переключения формата; обрыв после snapshot до log восстанавливается созданием отсутствующего пустого log с base snapshot generation (только при migration marker в snapshot). После готовности log durable завершить marker Control frame. Обрыв раньше snapshot — повторная миграция JSON, после snapshot — никогда не читать устаревший JSON как текущий индекс. Оставить JSON архивом для ручного rollback при остановленном демоне; downgrade автоматически запрещён. Reset архивирует весь store+floor и создаёт новый store epoch, rebind сохраняет causal history и меняет confirmed generation.

## Пороги приёмки R20

Fixture: два настоящих in-process Daemon с mTLS HTTP/2 loopback и временными state/root каталогами, 2092 детерминированных уникальных файла по 11 KiB (≈22.5 MiB), глубина до 4, 64 каталога, разные hash (исключить случайное ускорение R16), concurrency=6. Нет relay/искусственной задержки. Исходный полный Scan завершается до таймера, его записи отдельно; приёмник пуст, timer до последнего durable Done и совпадения hashes/Exec/Vector. Никаких mock fsync для основной цифры. Пять прогонов, медиана ≤30 s и каждый ≤45 s на локальных APFS/ext4 SSD; дополнительно ≥5× baseline текущего main в том же окружении. Это критерий цели, а не уже измеренный результат. VM/слабый диск публикует цифры отдельно; не подменять аппаратный flush no-op ради порога.

Байты индекса приёмника за прогон (все snapshot+log+Control+ACK writes, включая retry/short-write, кроме fixture setup) ≤4 MiB, включая один принудительный compaction в отдельном варианте ≤6 MiB. Intent bytes считать отдельно ≤4 MiB. Повторный no-op Scan: 0 store/intent bytes и 0 flush. Серия N=1000/2000/4000: bytes(4000) ≤2.3× bytes(2000)+256 KiB, исключает O(N²).

Пусть F=2092, G фактическое число групп, D число уникальных изменённых каталогов **по фазам групп**. Для консервативного macOS: FULLFSYNC ≤F+2G+12; file fsync ≤F+2G+12, directory fsync ≤3D+3G+32; общий вызов flush ≤2F+7G+3D+64. Linux без FULLFSYNC: ≤F+5G+3D+64. Двойной flush download+commit запрещён. Размер группы в тесте с готовой очередью ≥32 в среднем, G≤66; таймерный сценарий разреженных arrivals проверяется отдельно без этого требования. Заполнение групп контролируется отдельным детерминированным coordinator тестом; если реальный pipeline постоянно даёт меньшие группы, это провал производительности, а не повод увеличить бюджет. Очистка captures в first-sync отсутствует; update fixture отдельно проверяет дополнительные directory sync по формуле. Оптимизация G1 без F FULLFSYNC получает отдельный профиль только после решения вопроса R3.

Расширить per-Folder persistHooks (не глобальные переменные): write(kind,path,n,error), flush(kind,phase,dev,inode), phase(txID,phase), clockNow для детерминированных тестов. Обёртки считают фактически возвращённые Write n, включая intent/archive/compaction, и каждый вызов fsync/FULLFSYNC/BARRIERFSYNC раздельно; текущий syncFile скрывает два syscall, считать его одним нельзя. Hooks не меняют порядок production I/O, назначаются до concurrent use. Для app harness добавить в internal/folder узкий `PersistenceProbe` (наблюдение и fault injection поверх persistHooks, установка до использования Folder, без глобального состояния) и в app приватный constructor helper с Folder factory; обычный New использует folder.Open. Benchmark в package app вызывает helper, factory открывает Folder и устанавливает probe до возврата. Это внутренний Go API внутри internal/, не control endpoint и не пользовательская настройка; тестовые экспорты из `_test.go` импортируемому app недоступны, поэтому на них интеграционный harness не опирается. Существующие тесты folder продолжают использовать непосредственно persistHooks.

Дополнительно trace assert G1<G2<G3<G4<Done<G5; fault каждого Write/Close/rename/flush, truncation каждого byte frame, SIGKILL каждой фазы, повторный recovery, corrupt middle frame, ENOSPC, root/state на разных устройствах, writer с открытым fd, ACK lost/restart, cursor pending retry, pause/rebind с заполненной очередью под `-race`. SIGKILL не заменяет hard power-off. Финально `go vet ./...`, `go test -race ./...`, `make build`, `make app`; Linux runtime и VM/APFS power-loss выполняет владелец по обновлённой инструкции.

## Задачи реализации

Каждая строка — отдельная задача с точным ownership, ≤5 файлов, целевой diff 150–300 строк **с тестами**. Новые файлы названы явно; если бюджет превышается — дополнительное разбиение до реализации, не незаметное увеличение diff. Все неуказанные файлы только чтение. Общие файлы в строках образуют последовательную цепочку зависимостей; merge задачи после всех её зависимостей. Фундамент 04-10…04-19 раньше подключения apply; транспортные primitives можно делать параллельно, они не меняют folder. Уровень означает сложность ревью L1–L3.

| ID / цель | Точные файлы | Зависимости | Уровень | Конкретные тесты |
|---|---|---|---|---|
| 04-10 codec schema=3 | `internal/folder/store_codec.go`, `internal/folder/store_codec_test.go` | — | L3 | gob roundtrip binding/latch, duplicate name, oversized lengths, gzip bomb |
| 04-11 append/replay | `internal/folder/store.go`, `internal/folder/store_replay.go`, `internal/folder/store_test.go` | 04-10 | L3 | short write, torn tail, CRC middle, generation gap, repeated replay |
| 04-12 compaction | `internal/folder/store_compact.go`, `internal/folder/store_compact_test.go` | 04-11 | L3 | crash snapshot rename/log rename, full old log, no truncation before snapshot |
| 04-13 типы group-intent | `internal/folder/group_intent.go`, `internal/folder/group_intent_test.go` | 04-10 | L3 | duplicate target, invalid Disk, reserved conflict, binding mismatch |
| 04-14 recovery группы | `internal/folder/group_recovery.go`, `internal/folder/group_recovery_test.go` | 04-11, 04-13 | L3 | partial captures/install, repeated rollback, target occupied, committed cleanup |
| 04-15 hooks/flush contract | `internal/folder/index.go`, `internal/folder/rootfs_darwin.go`, `internal/folder/rootfs_linux.go`, `internal/folder/persist_hooks_test.go` | 04-12, 04-14 | L3 | count both syscalls, no-op hooks, failed FULLFSYNC, separate dev |
| 04-16 миграция helpers | `internal/folder/store_migrate.go`, `internal/folder/store_migrate_test.go` | 04-15 | L3 | schema0/2, durable latch/token, crash before/after snapshot, legacy intent |
| 04-17 подключить store Open/save | `internal/folder/index.go`, `internal/folder/journal.go`, `internal/folder/persist_test.go` | 04-16 | L3 | Open replay before network, unknown schema, old intent recovery, no JSON rewrite |
| 04-18 control persistence | `internal/folder/rootstate.go`, `internal/folder/rootstate_test.go` | 04-17 | L3 | reset new epoch, rebind same history, blocked survives restart, stale token |
| 04-19 commit группы | `internal/folder/commit.go`, `internal/folder/group_commit.go`, `internal/folder/group_commit_test.go` | 04-18 | L3 | G1…G5 trace, failing each flush, whole-group publish, cleanup marker |
| 04-20 очередь coordinator | `internal/folder/group.go`, `internal/folder/group_test.go` | 04-19 | L3 | count/bytes/time limit, duplicate target split, drain/cancel after intent |
| 04-21 staging apply extraction | `internal/folder/apply.go`, `internal/folder/apply_stage.go`, `internal/folder/apply_stage_test.go` | 04-20 | L3 | no network under scanMu, repeat decide, metadata-only, local race |
| 04-22 connect group apply | `internal/folder/apply.go`, `internal/folder/apply_group.go`, `internal/folder/apply_group_test.go` | 04-21 | L3 | 64 incoming one frame, mergeOnly durable, edit/delete, no early Done |
| 04-23 config concurrency | `internal/config/config.go`, `internal/config/config_test.go` | — | L2 | default6, 1/8 allowed, 0/9 invalid explicit |
| 04-24 download pool | `internal/folder/download_pool.go`, `internal/folder/download_pool_test.go`, `internal/folder/apply.go` | 04-22, 04-23 | L3 | two peers shared limit, cancel closes bodies, bounded staging |
| 04-25 handshake primitive | `internal/transport/protocol.go`, `internal/transport/protocol_test.go` | — | L2 | v1/missing=426, bad JSON, certified identity |
| 04-26 frame codec | `internal/transport/batch_codec.go`, `internal/transport/batch_codec_test.go` | 04-25 | L2 | truncated frame/end, duplicate id, wire/plain caps |
| 04-27 compression helper | `internal/transport/compress.go`, `internal/transport/compress_test.go` | 04-26 | L2 | text gzip, jpeg identity, bomb/trailing gzip, decoded hash |
| 04-28 /files server | `internal/transport/files.go`, `internal/transport/files_test.go` | 04-27 | L3 | version_changed mid-batch, blocked, symlink, bounded buffer |
| 04-29 /files client | `internal/transport/files_client.go`, `internal/transport/files_client_test.go` | 04-28 | L2 | keep good frames after disconnect, only remaining retry, cancel |
| 04-30 v2 routes/client | `internal/transport/transport.go`, `internal/transport/protocol_integration_test.go` | 04-29, 04-18 | L3 | /hello required, old server404, gzip single file, named index refresh |
| 04-31 retry queue primitive | `internal/app/peer_queue.go`, `internal/app/peer_queue_test.go` | 04-30 | L2 | failed one of64, backoff, newer Vector replaces, pending overflow |
| 04-32 daemon pipeline | `internal/app/daemon.go`, `internal/app/peer_pipeline_test.go` | 04-24, 04-31 | L3 | cursor unchanged on body fail, batches/pool wiring, pause drains, incompatible Status |
| 04-33 own events/types | `internal/folder/watch_events.go`, `internal/folder/watch_events_test.go` | 04-20 | L2 | exact self event suppress, external same mtime preserved, root event never suppress |
| 04-34 watcher paths | `internal/folder/watch_darwin.go`, `internal/folder/watch_linux.go`, `internal/folder/watch_test.go` | 04-33 | L2 | dropped/overflow full, rename parents, addTree error, root changed |
| 04-35 стабильность | `internal/folder/stability.go`, `internal/folder/stability_test.go` | — | L2 | fake clock1s, inode replaced, endless writer, pending bound |
| 04-36 Scan staging/lease | `internal/folder/scan.go`, `internal/folder/scan_stage.go`, `internal/folder/rootfs.go`, `internal/folder/rootstate.go`, `internal/folder/scan_stage_test.go` | 04-18, 04-35 | L3 | no scanMu during hash, rebind waits lease, stale baseline discarded, EACCES atomic |
| 04-37 incremental Scan | `internal/folder/scan_incremental.go`, `internal/folder/scan_incremental_test.go`, `internal/folder/scan_cache.go` | 04-34, 04-36 | L3 | rename/delete scope, collision with outside scope, last-file root latch |
| 04-38 schedule/watcher wiring | `internal/app/scan_schedule.go`, `internal/app/scan_schedule_test.go`, `internal/app/daemon.go` | 04-32, 04-37 | L2 | 12min/60s, overflow immediate, event storm no timer starvation |
| 04-39 исключения | `internal/folder/ignore.go`, `internal/folder/ignore_test.go`, `internal/folder/scan.go`, `internal/folder/apply.go`, `internal/folder/watch_events.go` | 04-24, 04-37 | L2 | .git included, builtins path components, old ignored no tombstone |
| 04-40 clone platforms | `internal/folder/clone.go`, `internal/folder/clone_darwin.go`, `internal/folder/clone_linux.go`, `internal/folder/clone_test.go` | 04-19 | L3 | CoW source unchanged, cross-device copy, nofollow, unsupported fallback |
| 04-41 hash reuse apply | `internal/folder/apply.go`, `internal/folder/reuse.go`, `internal/folder/reuse_test.go` | 04-39, 04-40 | L2 | target equal no fetch, candidate changed, Exec-only, permissions |
| 04-42 delta server | `internal/transport/delta.go`, `internal/transport/delta_test.go` | 04-30 | L3 | shrink/grow, threshold70%, bounds/overlap, stale target |
| 04-43 delta client | `internal/transport/delta_client.go`, `internal/transport/delta_client_test.go` | 04-42 | L3 | lost end, gzip bad block, offset overflow, full_required |
| 04-44 delta staging | `internal/folder/delta.go`, `internal/folder/delta_test.go` | 04-40, 04-43 | L3 | one changed block, base mutates, truncate, full resulting hash |
| 04-45 delta integration | `internal/folder/apply.go`, `internal/app/daemon.go`, `internal/app/delta_integration_test.go` | 04-38, 04-41, 04-44 | L3 | relay delta end-to-end, local edit during delta, full fallback |
| 04-46 ACK/floor model | `internal/folder/peerstate.go`, `internal/folder/peerstate_test.go`, `internal/folder/store_codec.go` | 04-18 | L3 | sparse vector holes, membership epoch, durable ACK replay |
| 04-47 tombstone collector | `internal/folder/tombstone.go`, `internal/folder/tombstone_test.go` | 04-46 | L3 | offline peer30d no GC, clock rollback, GC replay no resurrection |
| 04-48 ACK wire | `internal/transport/ack.go`, `internal/transport/ack_test.go`, `internal/transport/transport.go` | 04-30, 04-47 | L3 | certified peer, duplicate ACK, wrong epoch, ACK only exact deletion |
| 04-49 membership/ACK integration | `internal/app/daemon.go`, `internal/app/ack_integration_test.go`, `internal/folder/index.go` | 04-45, 04-48 | L3 | lost ACK restart, unpair/re-pair bootstrap, baseline before GC |
| 04-50 bounded log writer | `internal/app/loglimit.go`, `internal/app/loglimit_test.go`, `cmd/sharedir/main.go` | — | L2 | rotation5MiB×3, partial writes, journald no duplicate file |
| 04-51 reconnect summaries | `internal/app/reconnect_log.go`, `internal/app/reconnect_log_test.go`, `internal/app/daemon.go` | 04-49, 04-50 | L2 | never-connected summary5min, recovery immediate, secret redaction |
| 04-52 macOS error UI | `macos/SharedirBar/Store.swift`, `macos/SharedirBar/MenuContent.swift`, `macos/Resources/ru.lproj/Localizable.strings`, `macos/Resources/en.lproj/Localizable.strings` | 04-32 | L2 | v1 fixture, expected2 text, no false synced, light/dark snapshots |
| 04-53 QML error UI | `omarchy/sharedir/Service.qml`, `omarchy/sharedir/Widget.qml`, `omarchy/sharedir/Strings.js`, `omarchy/sharedir/tests/logic.test.mjs` | 04-32 | L2 | incompatible fixture, update message, paused/root error precedence |
| 04-54 perf harness | `internal/folder/persist_probe.go`, `internal/folder/persist_probe_test.go`, `internal/app/daemon.go`, `internal/app/perf_integration_test.go` | 04-39, 04-49, 04-51 | L3 | two app.Daemon 2092 files, real flush counters, 1000/2000/4000 scaling |
| 04-55 crash suite | `internal/folder/group_crash_test.go`, `internal/app/perf_race_test.go` | 04-54 | L3 | SIGKILL G0…G5, compaction kill, root/state different dev, concurrent confirm |
| 04-56 README/docs | `README.md`, `docs/protocol-v2.md`, `PLAN.md` | 04-51, 04-52, 04-53, 04-55 | L1 | commands match help, guarantees/limits/exclusions/v2, stack gob+log, no v1 fallback |
| 04-57 power loss guide | `docs/power-loss-check.md` | 04-55 | L1 | include group phases/log/snapshot/ACK, preserve ext4 VM and APFS checks, distinguish kill from power-off |

Разрешённые параллельные ветки: 04-10…24 (фундамент) и 04-25…31 (сеть); 04-23/35/50 независимы. После 04-20 — watch primitive, после 04-19 — clone; после 04-32 — обе UI отдельно. Все задачи с daemon.go упорядочены 32→38→45→49→51→54; с apply.go: 21→22→24→39→41→45. 04-36 и 04-39 последовательны через 37; 04-46 меняет codec только после его первичного создания. Перед стартом параллельных задач coordinator проверяет пересечение ownership, а не только логические зависимости. Полный DAG не допускает одновременные edits общих файлов.

## Вопросы владельцу

1. Буквальный R3 («FULLFSYNC только на commit группы и снимке») конфликтует с durable intent до изменения дерева, особенно когда state и root на разных устройствах. Здесь сохранены G1/G2 FULLFSYNC и ограничение R20 учитывает F входящих файлов; удаление этих flush не принято. Выбор владельца: оставить консервативную гарантию с дополнительными flush либо отдельно исследовать доказуемый group device flush; вариант ослабления durability требует отдельного явного решения, по умолчанию запрещён.
2. R6 нельзя гарантировать как абсолютное отсутствие скана любого собственного события при coalescing с внешней правкой. Здесь точные own-events подавляются, неоднозначные проходят incremental проверку. Если требуется ноль проверок вообще, это ослабит обнаружение локальных изменений; такой компромисс не принят.
3. Предложенные эксплуатационные параметры (30 суток GC, 256 MiB snapshot, 65536 delta blocks, пороги R20) являются стартовыми значениями проекта; изменение лимитов не должно обходить causal floor, latch или аппаратную долговечность. При исчерпании лимита показывается ошибка/задержка GC, не автоматический сброс истории.

Остальные вопросы поддержки ФС/ACL/локальных писателей из проекта folder остаются его ограничениями; этот этап не переоткрывает решения владельца и не расширяет обещания на неподдерживаемые тома.
