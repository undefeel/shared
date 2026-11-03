// sharedir — двусторонняя синхронизация папки между машинами в локальной сети.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"sharedir/internal/app"
	"sharedir/internal/config"
	"sharedir/internal/identity"
	"sharedir/internal/tray"
)

func init() {
	// На macOS UI трея обязан жить в главном потоке.
	runtime.LockOSThread()
}

const usage = `sharedir — синхронизация папки между машинами в локальной сети

Команды:
  init [--folder ПУТЬ] [--name ИМЯ]   создать конфиг и ключ устройства
  run [--no-tray]                     запустить демон (с иконкой в трее)
  status                              состояние синхронизации и найденные устройства
  id                                  показать ID этого устройства
  pair ID [--name ИМЯ] [--addr IP[:ПОРТ]]  доверять устройству (делается на обеих машинах)
  unpair ID                           перестать доверять устройству
  install                             автозапуск при входе (launchd / systemd --user)
  uninstall                           убрать автозапуск

Каталог настроек: %s (переопределяется SHAREDIR_HOME)
`

func main() {
	home, err := config.Home()
	if err != nil {
		fatal(err)
	}
	if len(os.Args) < 2 {
		fmt.Printf(usage, home)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "init":
		err = cmdInit(home, args)
	case "run":
		err = cmdRun(home, args)
	case "status":
		err = cmdStatus(home)
	case "id":
		err = cmdID(home)
	case "pair":
		err = cmdPair(home, args)
	case "unpair":
		err = cmdUnpair(home, args)
	case "install":
		err = app.InstallService(home)
	case "uninstall":
		err = app.UninstallService()
	case "help", "-h", "--help":
		fmt.Printf(usage, home)
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", cmd)
		fmt.Printf(usage, home)
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ошибка:", err)
	os.Exit(1)
}

// parse разбирает флаги, допуская их после позиционных аргументов
// (`pair ID --addr x`), чего стандартный flag не умеет.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdInit(home string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	folder := fs.String("folder", "~/Sharedir", "синхронизируемая папка")
	host, _ := os.Hostname()
	name := fs.String("name", strings.TrimSuffix(host, ".local"), "имя устройства")
	parse(fs, args)

	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(config.Path(home)); err == nil {
		return fmt.Errorf("конфиг уже существует: %s", config.Path(home))
	}
	abs, err := filepath.Abs(config.ExpandHome(*folder))
	if err != nil {
		return err
	}
	cfg := &config.Config{
		Name: *name, Folder: abs, Port: config.DefaultPort,
		Discovery: true, DiscoveryPort: config.DefaultDiscoveryPort, Peers: []config.Peer{},
	}
	if err := cfg.Save(home); err != nil {
		return err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	fmt.Printf("Готово.\n  Конфиг: %s\n  Папка:  %s\n  ID:     %s\n\n", config.Path(home), abs, identity.Pretty(id.ID))
	fmt.Println("Дальше: `sharedir run`, затем сопрягите устройства через трей или `sharedir pair`.")
	return nil
}

func cmdRun(home string, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	noTray := fs.Bool("no-tray", false, "без иконки в трее")
	parse(fs, args)

	logger := log.New(os.Stderr, "", log.LstdFlags)
	log.SetOutput(os.Stderr)
	d, err := app.New(home, logger)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *noTray {
		return d.Run(ctx)
	}
	errc := make(chan error, 1)
	go func() {
		errc <- d.Run(ctx)
		tray.Quit()
	}()
	go func() { <-ctx.Done(); tray.Quit() }()
	tray.Run(d, cancel)
	cancel()
	select {
	case err := <-errc:
		return err
	case <-time.After(3 * time.Second):
		return nil
	}
}

func cmdStatus(home string) error {
	st, err := app.NewCtl(home).Status()
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", st.Name, identity.Pretty(st.ID))
	fmt.Printf("Папка: %s (%d файлов, %.1f МБ)\n", st.Folder, st.Files, float64(st.Bytes)/(1<<20))
	fmt.Println("\nСопряжённые устройства:")
	if len(st.Peers) == 0 {
		fmt.Println("  нет")
	}
	for _, p := range st.Peers {
		mark := "○"
		if p.Connected {
			mark = "●"
		}
		line := fmt.Sprintf("  %s %-16s %-22s %s", mark, p.Name, p.Addr, p.State)
		if !p.LastSync.IsZero() {
			line += ", " + p.LastSync.Format("15:04:05")
		}
		fmt.Println(line)
	}
	if len(st.Candidates) > 0 {
		fmt.Println("\nНайдены, но не сопряжены (добавить: sharedir pair ID):")
		for _, c := range st.Candidates {
			name := c.Name
			if name == "" {
				name = "(входящее)"
			}
			fmt.Printf("  %-16s %-22s %s\n", name, c.Addr, identity.Pretty(c.ID))
		}
	}
	return nil
}

func cmdID(home string) error {
	if _, err := os.Stat(config.Path(home)); err != nil {
		return errors.New("сначала выполните `sharedir init`")
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return err
	}
	fmt.Println(identity.Pretty(id.ID))
	return nil
}

func cmdPair(home string, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	name := fs.String("name", "", "имя устройства")
	addr := fs.String("addr", "", "адрес IP[:порт], если устройство не находится автоматически")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return errors.New("использование: sharedir pair ID [--name ИМЯ] [--addr IP[:ПОРТ]]")
	}
	id, err := identity.Normalize(pos[0])
	if err != nil {
		return err
	}
	err = app.NewCtl(home).Pair(id, *name, *addr)
	if errors.Is(err, app.ErrNotRunning) {
		err = app.PairOffline(home, id, *name, *addr)
	}
	if err == nil {
		fmt.Printf("Устройство %s добавлено. Не забудьте сопрячь и с той стороны.\n", identity.Pretty(id))
	}
	return err
}

func cmdUnpair(home string, args []string) error {
	if len(args) != 1 {
		return errors.New("использование: sharedir unpair ID")
	}
	err := app.NewCtl(home).Unpair(args[0])
	if errors.Is(err, app.ErrNotRunning) {
		var cfg *config.Config
		if cfg, err = config.Load(home); err == nil {
			id, nerr := identity.Normalize(args[0])
			if nerr != nil {
				return nerr
			}
			if !cfg.RemovePeer(id) {
				return errors.New("такого устройства нет среди сопряжённых")
			}
			err = cfg.Save(home)
		}
	}
	return err
}
