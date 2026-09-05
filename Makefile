BINARIES := sway-session
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=
GO_FILES := $(shell find cmd internal -name '*.go' -type f)
DOC_ROOT := $(PREFIX)/share/doc/sway-session

.PHONY: build install clean fmt fmt-check test race vet lint apparmor-check codex-hook-check completion-check packaging-check standalone-check diff-check verify

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

codex-hook-check:
	sh scripts/check-codex-hook.sh

completion-check:
	sh scripts/check-completions.sh

packaging-check:
	sh scripts/check-packaging.sh

standalone-check:
	sh scripts/check-standalone-boundary.sh

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-session ./cmd/sway-session

diff-check:
	git diff --check
	git diff --cached --check

verify: fmt-check test race vet lint apparmor-check codex-hook-check completion-check packaging-check standalone-check build diff-check

install: build
	install -d $(PREFIX)/bin
	install -m755 sway-session $(PREFIX)/bin/sway-session
	install -d $(PREFIX)/lib/sway-session
	install -m755 contrib/codex/report-agent-session.sh $(PREFIX)/lib/sway-session/codex-report-agent-session
	install -d $(PREFIX)/share/bash-completion/completions
	install -d $(PREFIX)/share/zsh/site-functions
	install -d $(PREFIX)/share/fish/vendor_completions.d
	install -d $(DOC_ROOT)/contrib/sway-session
	install -d $(DOC_ROOT)/contrib/herdr
	install -d $(DOC_ROOT)/contrib/codex
	install -d $(DOC_ROOT)/contrib/apparmor
	install -d $(DOC_ROOT)/scripts
	install -d $(DOC_ROOT)/docs/adr
	install -d $(DOC_ROOT)/docs/assets
	install -m644 docs/assets/*.jpeg $(DOC_ROOT)/docs/assets/
	install -m644 docs/branding.md $(DOC_ROOT)/docs/branding.md
	install -m644 docs/agent-reporting.md $(DOC_ROOT)/docs/agent-reporting.md
	install -m644 README.md $(DOC_ROOT)/README.md
	install -m644 LICENSE $(DOC_ROOT)/LICENSE
	install -m644 docs/sway-session-plan.md $(DOC_ROOT)/docs/sway-session-plan.md
	install -m644 docs/sway-session-verification.md $(DOC_ROOT)/docs/sway-session-verification.md
	install -m644 docs/releasing.md $(DOC_ROOT)/docs/releasing.md
	install -m644 docs/workflow_conventions.md $(DOC_ROOT)/docs/workflow_conventions.md
	install -m644 docs/adr/0001-sqlite-session-runtime-state.md $(DOC_ROOT)/docs/adr/0001-sqlite-session-runtime-state.md
	install -m644 contrib/completions/bash/sway-session $(PREFIX)/share/bash-completion/completions/sway-session
	install -m644 contrib/completions/zsh/_sway-session $(PREFIX)/share/zsh/site-functions/_sway-session
	install -m644 contrib/completions/fish/sway-session.fish $(PREFIX)/share/fish/vendor_completions.d/sway-session.fish
	install -m644 contrib/sway/50-sway-session.conf $(DOC_ROOT)/50-sway-session.conf
	install -m644 contrib/sway-session/config.toml $(DOC_ROOT)/contrib/sway-session/config.toml
	install -m644 contrib/herdr/config.toml $(DOC_ROOT)/contrib/herdr/config.toml
	install -m644 contrib/codex/hooks.json $(DOC_ROOT)/contrib/codex/hooks.json
	install -m644 contrib/apparmor/codex-home-guard $(DOC_ROOT)/contrib/apparmor/codex-home-guard
	install -m755 scripts/verify-codex-boundary.sh $(DOC_ROOT)/scripts/verify-codex-boundary.sh

clean:
	rm -f $(BINARIES)
