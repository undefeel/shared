//go:build linux

// Package tray — иконка StatusNotifierItem для Linux-окружений с треем
// (Waybar в Omarchy 3, KDE и т.п.). В Omarchy 4 вместо неё — плагин оболочки,
// на macOS — нативное приложение из macos/.
package tray

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"sharedir/internal/app"
	"sharedir/internal/i18n"
	"sharedir/internal/identity"
)

const (
	peerSlots      = 4
	candidateSlots = 5
)

// Run показывает трей и блокируется до выхода. На macOS должен вызываться из
// главного потока. onQuit вызывается, когда пользователь выбрал «Выход».
func Run(d *app.Daemon, onQuit func()) {
	systray.Run(func() { onReady(d) }, onQuit)
}

func Quit() { systray.Quit() }

type menu struct {
	header     *systray.MenuItem
	peers      [peerSlots]*systray.MenuItem
	candidates *systray.MenuItem
	candSlots  [candidateSlots]*systray.MenuItem
	idsMu      sync.Mutex
	candIDs    [candidateSlots]string
	noCands    *systray.MenuItem
	lastIcon   iconState
}

func onReady(d *app.Daemon) {
	m := &menu{lastIcon: -1}
	m.setIcon(iconOffline)
	systray.SetTooltip("sharedir")

	m.header = systray.AddMenuItem("sharedir", "")
	m.header.Disable()
	for i := range m.peers {
		m.peers[i] = systray.AddMenuItem("", "")
		m.peers[i].Disable()
		m.peers[i].Hide()
	}
	systray.AddSeparator()
	openItem := systray.AddMenuItem(i18n.T("Open folder"), "")
	copyItem := systray.AddMenuItem(i18n.T("Copy my ID"), identity.Pretty(d.ID.ID))
	m.candidates = systray.AddMenuItem(i18n.T("Add device"), "")
	m.noCands = m.candidates.AddSubMenuItem(i18n.T("Nobody found"), "")
	m.noCands.Disable()
	for i := range m.candSlots {
		m.candSlots[i] = m.candidates.AddSubMenuItem("", "")
		m.candSlots[i].Hide()
	}
	systray.AddSeparator()
	quitItem := systray.AddMenuItem(i18n.T("Quit"), "")

	go func() {
		for {
			select {
			case <-openItem.ClickedCh:
				openPath(d.Config().Folder)
			case <-copyItem.ClickedCh:
				copyText(identity.Pretty(d.ID.ID))
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
	for i := range m.candSlots {
		go func(i int) {
			for range m.candSlots[i].ClickedCh {
				m.idsMu.Lock()
				id := m.candIDs[i]
				m.idsMu.Unlock()
				if id != "" {
					if err := d.Pair(id, "", ""); err != nil {
						log.Printf("сопряжение: %v", err)
					}
				}
			}
		}(i)
	}

	updates := make(chan struct{}, 1)
	d.OnChange = func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
	go func() {
		tick := time.NewTicker(5 * time.Second)
		for {
			m.refresh(d.Status())
			select {
			case <-updates:
				time.Sleep(200 * time.Millisecond) // склеиваем частые обновления
			case <-tick.C:
			}
		}
	}()
}

func (m *menu) setIcon(s iconState) {
	if s == m.lastIcon {
		return
	}
	m.lastIcon = s
	systray.SetTemplateIcon(drawIcon(s, black), drawIcon(s, light))
}

func (m *menu) refresh(st app.Status) {
	icon, summary := iconOffline, i18n.T("no paired devices")
	if len(st.Peers) > 0 {
		summary = i18n.T("offline")
	}
	for _, p := range st.Peers {
		if p.Connected && icon == iconOffline {
			icon, summary = iconOK, i18n.T("synced")
		}
		if p.Syncing {
			icon, summary = iconSyncing, i18n.T("syncing…")
		}
	}
	m.setIcon(icon)
	m.header.SetTitle(fmt.Sprintf("%s — %s", st.Name, summary))
	systray.SetTooltip("sharedir: " + summary + "\n" + i18n.T("%d files, %s", st.Files, humanBytes(st.Bytes)))

	for i, item := range m.peers {
		if i >= len(st.Peers) {
			item.Hide()
			continue
		}
		p := st.Peers[i]
		dot := "○"
		if p.Connected {
			dot = "●"
		}
		title := fmt.Sprintf("%s %s — %s", dot, p.Name, i18n.T(p.State, p.Failed))
		if !p.LastSync.IsZero() && p.Connected {
			title += ", " + p.LastSync.Format("15:04")
		}
		item.SetTitle(title)
		item.Show()
	}

	if len(st.Candidates) == 0 {
		m.noCands.Show()
	} else {
		m.noCands.Hide()
	}
	m.idsMu.Lock()
	defer m.idsMu.Unlock()
	for i, item := range m.candSlots {
		if i >= len(st.Candidates) {
			m.candIDs[i] = ""
			item.Hide()
			continue
		}
		c := st.Candidates[i]
		name := c.Name
		if name == "" {
			name = i18n.T("incoming connection")
		}
		m.candIDs[i] = c.ID
		item.SetTitle(fmt.Sprintf("%s (%s)", name, identity.Pretty(c.ID)[:9]))
		item.SetTooltip(i18n.T("Compare this ID with the one shown on that device: %s", identity.Pretty(c.ID)))
		item.Show()
	}
}

func openPath(p string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	exec.Command(cmd, p).Start()
}

func copyText(s string) {
	cmd := exec.Command("wl-copy")
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("pbcopy")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	if cmd.Start() != nil {
		return
	}
	stdin.Write([]byte(s))
	stdin.Close()
	cmd.Wait()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cБ", float64(n)/float64(div), []rune("КМГТ")[exp])
}
