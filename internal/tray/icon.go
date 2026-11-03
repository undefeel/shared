//go:build linux

package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

type iconState int

const (
	iconOK      iconState = iota // залитая папка
	iconSyncing                  // папка с «окошком»
	iconOffline                  // контур папки
)

// drawIcon рисует папку 64×64 цветом c. Отдельные файлы с картинками не
// нужны, а template-иконка на macOS сама подстроится под тему меню-бара.
func drawIcon(state iconState, c color.Color) []byte {
	const size = 64
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	inBody := func(x, y int) bool { return x >= 6 && x < 58 && y >= 18 && y < 54 }
	inTab := func(x, y int) bool { return x >= 6 && x < 28 && y >= 11 && y < 19 }
	const stroke = 5
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			on := inBody(x, y) || inTab(x, y)
			switch state {
			case iconOffline:
				inner := x >= 6+stroke && x < 58-stroke && y >= 18+stroke && y < 54-stroke
				on = on && !inner
			case iconSyncing:
				dx, dy := x-32, y-36
				on = on && dx*dx+dy*dy > 9*9
			}
			if on {
				img.Set(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

var (
	black = color.NRGBA{0, 0, 0, 255}
	light = color.NRGBA{0xd8, 0xde, 0xe9, 255} // читается на тёмном Waybar
)
