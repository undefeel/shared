.PHONY: build mac app linux test test-omarchy clean omarchy

build: mac linux

# macOS собирается нативно: трею и FSEvents нужен cgo.
mac:
	go build -trimpath -o bin/sharedir ./cmd/sharedir

# macOS-приложение меню-бара с демоном внутри: bin/sharedir.app
app:
	./macos/build.sh

# Linux — без cgo (трей ходит в DBus напрямую), можно собирать прямо на маке.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/sharedir-linux-amd64 ./cmd/sharedir
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/sharedir-linux-arm64 ./cmd/sharedir

test:
	go test -race ./...

test-omarchy:
	node --test omarchy/sharedir/tests/logic.test.mjs

clean:
	rm -rf bin

# Плагин бара Omarchy 4: копия (не симлинк — оболочка не следит за файлами вне ~/.config).
omarchy:
	rm -rf $(HOME)/.config/omarchy/plugins/sharedir
	cp -R omarchy/sharedir $(HOME)/.config/omarchy/plugins/sharedir
