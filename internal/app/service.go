package app

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	launchdLabel    = "dev.sharedir.agent"
	systemdUnitName = "sharedir.service"
)

func xmlEscape(s string) string {
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func escapeSystemdValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	return s
}

func launchdPlist(exe, home string) string {
	h, _ := os.UserHomeDir()
	logPath := filepath.Join(h, "Library/Logs/sharedir.log")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key><array><string>%s</string><string>run</string></array>
	<key>EnvironmentVariables</key><dict><key>SHAREDIR_HOME</key><string>%s</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	<key>ProcessType</key><string>Interactive</string>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(launchdLabel), xmlEscape(exe), xmlEscape(home), xmlEscape(logPath), xmlEscape(logPath))
}

// В ExecStart systemd раскрывает $VAR, поэтому там $ удваивается; в Environment — нет.
func systemdUnit(exe, home string) (string, error) {
	if strings.ContainsAny(exe, "\n\r") || strings.ContainsAny(home, "\n\r") {
		return "", fmt.Errorf("путь содержит перевод строки")
	}
	content := fmt.Sprintf(`[Unit]
Description=sharedir — синхронизация папки
After=graphical-session.target network-online.target
PartOf=graphical-session.target

[Service]
ExecStart="%s" run
Environment="SHAREDIR_HOME=%s"
Restart=on-failure
RestartSec=5

[Install]
WantedBy=graphical-session.target
`, strings.ReplaceAll(escapeSystemdValue(exe), "$", "$$"), escapeSystemdValue(home))
	return content, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// InstallService регистрирует `sharedir run` для автозапуска при входе:
// LaunchAgent на macOS, пользовательский юнит systemd на Linux.
func InstallService(home string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if strings.HasPrefix(exe, os.TempDir()) || strings.Contains(exe, "go-build") {
		return fmt.Errorf("бинарник лежит во временном каталоге (%s); скопируйте его, например, в ~/.local/bin", exe)
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(exe, home)
	case "linux":
		return installSystemd(exe, home)
	}
	return fmt.Errorf("автозапуск для %s не поддерживается", runtime.GOOS)
}

func UninstallService() error {
	h, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(h, "Library/LaunchAgents", launchdLabel+".plist")
		run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
		return os.Remove(p)
	case "linux":
		run("systemctl", "--user", "disable", "--now", systemdUnitName)
		return os.Remove(filepath.Join(h, ".config/systemd/user", systemdUnitName))
	}
	return nil
}

func installLaunchd(exe, home string) error {
	h, _ := os.UserHomeDir()
	plist := filepath.Join(h, "Library/LaunchAgents", launchdLabel+".plist")
	logPath := filepath.Join(h, "Library/Logs/sharedir.log")
	content := launchdPlist(exe, home)
	os.MkdirAll(filepath.Dir(plist), 0o755)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run() // если уже был
	if err := run("launchctl", "bootstrap", domain, plist); err != nil {
		return err
	}
	fmt.Printf("Установлено: %s\nЛог: %s\n", plist, logPath)
	return nil
}

func installSystemd(exe, home string) error {
	content, err := systemdUnit(exe, home)
	if err != nil {
		return err
	}
	h, _ := os.UserHomeDir()
	unit := filepath.Join(h, ".config/systemd/user", systemdUnitName)
	// Привязка к graphical-session.target: трею нужна сессия Wayland и DBus.
	// В Omarchy (Hyprland через uwsm) этот таргет поднимается при входе.
	os.MkdirAll(filepath.Dir(unit), 0o755)
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "enable", "--now", systemdUnitName); err != nil {
		return err
	}
	fmt.Printf("Установлено: %s\nЛог: journalctl --user -u sharedir -f\n", unit)
	return nil
}
