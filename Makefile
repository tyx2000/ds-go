PREFIX ?= $(HOME)/.local

.PHONY: build test check install
build:
	go build -trimpath -o bin/dsgo ./cmd/dsgo

test:
	go test ./... -timeout=120s

check:
	go vet ./...
	go test -race ./... -timeout=120s

install: build
	install -d "$(PREFIX)/bin"
	install -m 755 bin/dsgo "$(PREFIX)/bin/dsgo"
