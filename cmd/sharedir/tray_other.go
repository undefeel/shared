//go:build !linux

package main

import (
	"context"
	"errors"

	"sharedir/internal/app"
)

const trayAvailable = false

func runWithTray(context.Context, context.CancelFunc, *app.Daemon) error {
	return errors.New("--tray есть только на Linux; на macOS используйте приложение SharedirBar")
}
