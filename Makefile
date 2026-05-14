BINARY := sway-title-animator
PREFIX ?= $(HOME)/.local

.PHONY: build install clean

build:
	CGO_ENABLED=0 go build -ldflags='-s -w' -o $(BINARY) ./cmd/sway-title-animator

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
