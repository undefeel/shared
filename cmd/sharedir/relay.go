package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"sharedir/internal/app"
	"sharedir/internal/config"
	"sharedir/internal/i18n"
	"sharedir/internal/identity"
	"sharedir/internal/relay"
)

const relayUsage = `sharedir relay — посредник для устройств в разных сетях

На сервере (VPS):
  relay init --addr ПУБЛИЧНЫЙ_IP[:ПОРТ]   создать ключ и токен, напечатать адрес relay
  relay serve [--listen :22200]           запустить relay
  relay url                               напечатать адрес relay ещё раз

На устройствах:
  relay use АДРЕС                         подключаться через relay (sharedir-relay://…)
  relay off                               перестать использовать relay
`

// relayState — то, что relay хранит на диске помимо ключа.
type relayState struct {
	Token string `json:"token"`
	Addr  string `json:"addr"`
}

func relayHome(home string) string { return filepath.Join(home, "relay") }

func loadRelayState(home string) (*relayState, *identity.Identity, error) {
	data, err := os.ReadFile(filepath.Join(relayHome(home), "relay.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, errors.New("relay is not initialized, run `sharedir relay init --addr IP`")
	}
	if err != nil {
		return nil, nil, err
	}
	var st relayState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, nil, err
	}
	id, err := identity.LoadOrCreate(relayHome(home))
	return &st, id, err
}

func cmdRelay(home string, args []string) error {
	if len(args) == 0 {
		fmt.Print(relayUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "init":
		fs := flag.NewFlagSet("relay init", flag.ExitOnError)
		addr := fs.String("addr", "", "публичный адрес relay: IP или IP:порт")
		parse(fs, args)
		if *addr == "" {
			return errors.New("usage: sharedir relay init --addr IP[:PORT]")
		}
		a := *addr
		if _, _, err := net.SplitHostPort(a); err != nil {
			a = net.JoinHostPort(a, relay.DefaultPort)
		}
		if err := os.MkdirAll(relayHome(home), 0o700); err != nil {
			return err
		}
		st := relayState{Token: relay.NewToken(), Addr: a}
		if old, _, err := loadRelayState(home); err == nil {
			st.Token = old.Token // токен не меняем: устройства уже настроены
		}
		data, _ := json.MarshalIndent(st, "", "  ")
		if err := os.WriteFile(filepath.Join(relayHome(home), "relay.json"), data, 0o600); err != nil {
			return err
		}
		return printRelayURL(home)

	case "url":
		return printRelayURL(home)

	case "serve":
		fs := flag.NewFlagSet("relay serve", flag.ExitOnError)
		listen := fs.String("listen", ":"+relay.DefaultPort, "адрес для входящих")
		parse(fs, args)
		st, id, err := loadRelayState(home)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		logger := log.New(os.Stderr, "", log.LstdFlags)
		logger.Printf("relay %s слушает %s", identity.Pretty(id.ID), *listen)
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			ln.Close()
		}()
		err = (&relay.Server{ID: id, Token: st.Token, Log: logger}).Serve(ln)
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err

	case "use", "off":
		url := ""
		if sub == "use" {
			if len(args) != 1 {
				return errors.New("usage: sharedir relay use sharedir-relay://ID:TOKEN@host:port")
			}
			ep, err := relay.ParseURL(args[0])
			if err != nil {
				return err
			}
			url = ep.URL()
		}
		cfg, err := config.Load(home)
		if err != nil {
			return err
		}
		cfg.Relay = url
		if err := cfg.Save(home); err != nil {
			return err
		}
		if err := app.NewCtl(home).Reload(); err != nil && !errors.Is(err, app.ErrNotRunning) {
			return err
		}
		if url == "" {
			fmt.Println(i18n.T("relay off"))
		} else {
			fmt.Println(i18n.T("relay on: devices will connect through it when not on the same network"))
		}
		return nil
	}
	fmt.Print(relayUsage)
	return fmt.Errorf("unknown relay command %q", sub)
}

func printRelayURL(home string) error {
	st, id, err := loadRelayState(home)
	if err != nil {
		return err
	}
	fmt.Println(relay.Endpoint{Addr: st.Addr, RelayID: id.ID, Token: st.Token}.URL())
	return nil
}
