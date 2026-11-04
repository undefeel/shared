.pragma library

// Переводы плагина. Ключи — английские строки (базовый язык), %1, %2 —
// подстановки. Язык берётся из ~/.config/omarchy/language через I18n.

var ru = {
  // Шапка
  "Up to date": "Синхронизировано",
  "Syncing": "Синхронизация",
  "Offline": "Нет связи",
  "Paused": "На паузе",
  "No devices": "Нет устройств",
  "Not running": "Не запущен",
  "Folder stopped": "Папка остановлена",
  "Folder stopped: %1": "Папка остановлена: %1",
  "root_empty": "папка пуста",
  "root_unavailable": "папка недоступна",
  "root_changed": "папка изменилась",
  "commit_failed": "ошибка записи на диск",
  "The folder is empty. Choose carefully.": "Папка пуста. Внимательно выберите действие.",
  "Choose how to continue with this folder.": "Выберите, как продолжить работу с этой папкой.",
  "Start over (reset)": "Начать заново (сброс)",
  "Keep this folder (rebind)": "Считать папку прежней (перепривязать)",
  "Reset explanation": "Старый индекс будет архивирован. Файлы не удалятся; содержимое папки и устройств встретится как новое, возможны копии конфликтов.",
  "Rebind explanation": "Индекс сохранится. Файлы, которых нет в этой папке, будут удалены на устройствах.",
  "Rebind empty warning": "Папка пуста: после перепривязки все файлы будут удалены на устройствах.",
  "Confirm": "Подтвердить",
  "Folder reset confirmed": "Сброс папки подтверждён.",
  "Folder rebind confirmed": "Перепривязка папки подтверждена.",
  "Confirmation failed; folder remains stopped": "Не удалось подтвердить. Папка остаётся остановленной.",
  "Folder status changed. Wait for an update, then confirm again.": "Состояние папки изменилось. Дождитесь обновления и подтвердите снова.",
  "Pause syncing": "Поставить на паузу",
  "Resume syncing": "Продолжить",
  "Open folder": "Открыть папку",
  "Copy my ID": "Скопировать мой ID",

  // Строка статуса
  "%1 online · %2 in folder": "%1 на связи · %2 в папке",
  "Receiving %1 of %2 · %3 left": "Получение %1 из %2 · осталось %3",
  "No peers on the network. Make sure they are on.": "Пиры не найдены в сети. Проверьте, что они включены.",
  "Changes pile up locally and sync after you resume.": "Изменения копятся локально и уйдут после возобновления.",
  "Pair a device below, then do the same on it.": "Сопрягите устройство ниже — и сделайте то же на нём.",
  "The sharedir daemon isn't running.": "Демон sharedir не запущен.",
  "Start": "Запустить",

  // Вкладки и секции
  "Devices": "Устройства",
  "Activity": "Активность",
  "DEVICES": "УСТРОЙСТВА",
  "FOUND ON NETWORK": "НАЙДЕНО В СЕТИ",
  "FOLDER": "ПАПКА",
  "TRANSFERRED BY DAY": "ПЕРЕДАНО ПО ДНЯМ",
  "RECENT FILES": "НЕДАВНИЕ ФАЙЛЫ",
  "No changes yet": "Изменений пока нет",

  // Пиры
  "online": "на связи",
  "offline": "не в сети",
  "searching": "поиск",
  "Incompatible version": "Несовместимая версия",
  "Update the other machine to the same sharedir version": "Обновите sharedir на другом компьютере до той же версии",
  "paused": "пауза",
  "peer paused": "пауза у пира",
  "retrying": "повтор",
  "Up to date · %1": "Синхронизировано · %1",
  "Last seen %1 · changes will sync later": "Был %1 · изменения отправятся позже",
  "Not found on the network": "Не найден в сети",
  "Looking for the device…": "Поиск устройства…",
  "Sync is paused": "Синхронизация остановлена",
  "Paused on that device": "На паузе на том устройстве",
  "%1 files failed, retrying": "Не удалось файлов: %1, повтор",
  "direct": "напрямую",
  "via relay": "через relay",

  // Сопряжение
  "Incoming connection": "Входящее подключение",
  "Add": "Добавить",
  "Compare the ID with the screen of %1:": "Сверьте ID с экраном %1:",
  "that device": "того устройства",
  "Cancel": "Отмена",
  "Pair": "Сопрячь",

  // Типы файлов
  "video": "Видео", "images": "Изображения", "docs": "Документы", "audio": "Аудио",
  "archive": "Архивы", "code": "Код", "other": "Прочее",

  // Лента
  "from %1": "с %1", "to %1": "на %1", "deleted": "удалён", "conflict": "конфликт",
  "Today": "Сегодня",
  "just now": "только что",
  "%1 min ago": "%1 мин назад",
  "%1 h ago": "%1 ч назад"
}

var enKinds = { video: "Video", images: "Images", docs: "Documents", audio: "Audio", archive: "Archives", code: "Code", other: "Other" }

function lang(code) {
  return String(code || "en").indexOf("ru") === 0 ? "ru" : "en"
}

function t(language, key, args) {
  var s = (language === "ru" && ru[key] !== undefined) ? ru[key] : key
  if (args) for (var i = 0; i < args.length; i++) s = s.split("%" + (i + 1)).join(String(args[i]))
  return s
}

function kind(language, code) {
  return language === "ru" ? t(language, code) : (enKinds[code] || code)
}

function files(language, n) {
  if (language !== "ru") return n === 1 ? "1 file" : n + " files"
  var m10 = n % 10, m100 = n % 100
  if (m10 === 1 && m100 !== 11) return n + " файл"
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return n + " файла"
  return n + " файлов"
}

// Размеры в стиле Omarchy: коротко, без пробела — 2.3G, 512M.
function bytes(n) {
  n = Number(n || 0)
  var units = ["B", "K", "M", "G", "T"], i = 0
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++ }
  if (n === 0) return "0"
  return (i === 0 || n >= 100 ? Math.round(n) : n.toFixed(1).replace(/\.0$/, "")) + units[i]
}

function ago(language, iso) {
  if (!iso) return ""
  var sec = (Date.now() - Date.parse(iso)) / 1000
  if (sec < 60) return t(language, "just now")
  if (sec < 3600) return t(language, "%1 min ago", [Math.floor(sec / 60)])
  if (sec < 86400) return t(language, "%1 h ago", [Math.floor(sec / 3600)])
  return new Date(Date.parse(iso)).toLocaleDateString(language === "ru" ? "ru-RU" : "en-US")
}

function clock(iso) {
  var d = new Date(Date.parse(iso))
  return ("0" + d.getHours()).slice(-2) + ":" + ("0" + d.getMinutes()).slice(-2)
}

var ruDays = ["Вс", "Пн", "Вт", "Ср", "Чт", "Пт", "Сб"]
var enDays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]

function dayLabel(language, date, isToday) {
  if (isToday) return t(language, "Today")
  var d = new Date(date + "T12:00:00")
  return (language === "ru" ? ruDays : enDays)[d.getDay()]
}

// Короткое состояние пира справа от имени.
function peerBadge(language, peer) {
  if (peer.progress && peer.progress.bytes > 0)
    return Math.round(100 * peer.progress.done_bytes / peer.progress.bytes) + "%"
  switch (peer.state) {
  case "incompatible": return t(language, "Incompatible version")
  case "synced": return t(language, "online")
  case "syncing": return t(language, "Syncing").toLowerCase()
  case "searching": return t(language, "searching")
  case "paused": return t(language, "paused")
  case "peer_paused": return t(language, "peer paused")
  case "retry": return t(language, "retrying")
  default: return t(language, "offline")
  }
}

// Подпись под прогресс-баром пира.
function peerCaption(language, peer) {
  if (peer.state === "incompatible" || peer.error_code === "protocol_incompatible") return t(language, "Update the other machine to the same sharedir version")
  var p = peer.progress
  if (p) return t(language, "Receiving %1 of %2 · %3 left", [p.done_files, p.files, bytes(p.bytes - p.done_bytes)])
  switch (peer.state) {
  case "synced": return t(language, "Up to date · %1", [ago(language, peer.last_sync)])
  case "offline":
    return peer.last_sync ? t(language, "Last seen %1 · changes will sync later", [ago(language, peer.last_sync)]) : t(language, "Offline")
  case "not_found": return t(language, "Not found on the network")
  case "searching": return t(language, "Looking for the device…")
  case "paused": return t(language, "Sync is paused")
  case "peer_paused": return t(language, "Paused on that device")
  case "retry": return t(language, "%1 files failed, retrying", [peer.failed || 0])
  default: return ""
  }
}

// Как идёт связь с пиром: напрямую (с IP) или через relay. Пусто — нет связи.
function peerLink(language, peer) {
  if (!peer.connected || !peer.addr) return ""
  if (peer.addr === "relay") return t(language, "via relay")
  return t(language, "direct") + " · " + String(peer.addr).replace(/:\d+$/, "").replace(/^\[|\]$/g, "")
}

function prettyId(id) {
  return String(id || "").replace(/(.{4})(?=.)/g, "$1-")
}
