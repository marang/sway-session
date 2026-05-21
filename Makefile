BINARY := sway-title-animator
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=

.PHONY: build install clean

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o $(BINARY) ./cmd/sway-title-animator

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
