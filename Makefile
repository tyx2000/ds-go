PREFIX ?= $(HOME)/.local
BUILD_TIMESTAMP ?= $(shell TZ=Asia/Shanghai date +%Y%m%d.%H%M%S)

.PHONY: build test check install
build:
	go build -trimpath -ldflags "-X dsgo/internal/cli.buildTimestamp=$(BUILD_TIMESTAMP)" -o bin/dsgo ./cmd/dsgo

test:
	go test ./... -timeout=120s

check:
	go vet ./...
	go test -race ./... -timeout=120s

install: build
	install -d "$(PREFIX)/bin"
	install -m 755 bin/dsgo "$(PREFIX)/bin/dsgo"
