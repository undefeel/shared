package main

import (
	"context"
	"time"

	"sharedir/internal/app"
	"sharedir/internal/tray"
)

const trayAvailable = true

// runWithTray держит трей в главном потоке, а демон — в горутине.
func runWithTray(ctx context.Context, cancel context.CancelFunc, d *app.Daemon) error {
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
