BINARIES := sway-title-animator sway-session
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=
GO_FILES := $(shell find . -name '*.go' -type f)

.PHONY: build install clean fmt fmt-check test race vet lint apparmor-check completion-check packaging-check process-boundary-check diff-check verify

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$$(gofmt -l $(GO_FILES))" || { \
		echo "Go files need formatting:"; \
		gofmt -l $(GO_FILES); \
		exit 1; \
	}

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	staticcheck ./...

apparmor-check:
	sh scripts/check-apparmor-policy.sh

completion-check:
	sh scripts/check-completions.sh

packaging-check:
	sh scripts/check-packaging.sh

process-boundary-check:
	sh scripts/check-process-boundary.sh

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-title-animator ./cmd/sway-title-animator
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-session ./cmd/sway-session

diff-check:
	git diff --check
	git diff --cached --check

verify: fmt-check test race vet lint apparmor-check completion-check packaging-check process-boundary-check build diff-check

install: build
	install -d $(PREFIX)/bin
	for binary in $(BINARIES); do install -m755 $$binary $(PREFIX)/bin/$$binary; done
	install -d $(PREFIX)/share/bash-completion/completions
	install -d $(PREFIX)/share/zsh/site-functions
	install -d $(PREFIX)/share/fish/vendor_completions.d
	install -d $(PREFIX)/share/doc/sway-title-animator/contrib/sway-session
	install -m644 contrib/completions/bash/sway-session $(PREFIX)/share/bash-completion/completions/sway-session
	install -m644 contrib/completions/zsh/_sway-session $(PREFIX)/share/zsh/site-functions/_sway-session
	install -m644 contrib/completions/fish/sway-session.fish $(PREFIX)/share/fish/vendor_completions.d/sway-session.fish
	install -m644 contrib/sway-session/config.toml $(PREFIX)/share/doc/sway-title-animator/contrib/sway-session/config.toml

clean:
	rm -f $(BINARIES)
