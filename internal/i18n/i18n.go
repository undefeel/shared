// Package i18n — переводы для CLI и Linux-трея. Базовый язык английский,
// русский включается локалью (LC_ALL, LC_MESSAGES, LANG начинаются с "ru").
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// Lang возвращает "ru" или "en" по переменным локали в порядке приоритета POSIX.
func Lang() string {
	for _, v := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if val := os.Getenv(v); val != "" {
			if strings.HasPrefix(val, "ru") {
				return "ru"
			}
			return "en"
		}
	}
	return "en"
}

var ru = map[string]string{
	// Состояния пира (коды из app.State*).
	"searching":   "поиск",
	"not_found":   "не найден в сети",
	"offline":     "нет связи",
	"syncing":     "синхронизация",
	"synced":      "синхронизировано",
	"retry":       "не удалось: %d, повтор",
	"paused":      "пауза",
	"peer_paused": "пир на паузе",

	// Типы файлов.
	"video":   "Видео",
	"images":  "Изображения",
	"docs":    "Документы",
	"audio":   "Аудио",
	"archive": "Архивы",
	"code":    "Код",
	"other":   "Прочее",

	// sharedir status.
	"Folder is blocked": "Папка остановлена",
	"Confirm with: sharedir folder-confirm --action reset|rebind --token %s": "Подтвердите: sharedir folder-confirm --action reset|rebind --token %s",
	"Last scan failed (%s): %s":                  "Последний скан не удался (%s): %s — локальные изменения не отправляются",
	"Sync is paused (sharedir resume)":           "⏸  синхронизация на паузе (sharedir resume)",
	"Folder: %s (%d files, %.1f MB)":             "Папка: %s (%d файлов, %.1f МБ)",
	"Paired devices:":                            "Сопряжённые устройства:",
	"none":                                       "нет",
	"Found, not paired (add: sharedir pair ID):": "Найдены, но не сопряжены (добавить: sharedir pair ID):",
	"(incoming)":                                 "(входящее)",
	"daemon is not running":                      "демон не запущен",

	// Ошибки управляющего API — их же переводит приложение macOS.
	"invalid device ID":            "некорректный ID устройства",
	"that is this device's own ID": "это ID этого же устройства",
	"no such paired device":        "такого устройства нет среди сопряжённых",

	// sharedir relay.
	"relay off": "relay выключен",
	"relay on: devices will connect through it when not on the same network": "relay включён: устройства в разных сетях будут соединяться через него",
	"relay is not configured": "relay не настроен",

	// Трей.
	"no paired devices":   "нет сопряжённых устройств",
	"syncing…":            "синхронизация…",
	"Open folder":         "Открыть папку",
	"Copy my ID":          "Скопировать мой ID",
	"Add device":          "Добавить устройство",
	"Nobody found":        "Никого не найдено",
	"incoming connection": "входящее подключение",
	"Quit":                "Выход",
	"%d files, %s":        "%d файлов, %s",
	"Compare this ID with the one shown on that device: %s": "Сверьте ID с тем, что показывает то устройство: %s",
}

// T переводит ключ (английскую строку или код) и подставляет аргументы.
func T(key string, args ...any) string {
	s := key
	if Lang() == "ru" {
		if v, ok := ru[key]; ok {
			s = v
		}
	} else if key == "retry" {
		s = "failed: %d, retrying"
	} else if strings.ToLower(key) == key && !strings.Contains(key, " ") {
		s = strings.ReplaceAll(key, "_", " ") // коды состояний читаемы и так
	}
	if len(args) > 0 && strings.Contains(s, "%") {
		return fmt.Sprintf(s, args...)
	}
	return s
}
