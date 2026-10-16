BINARIES := sway-session
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
MODIFIED ?= $(shell if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then test -z "$$(git status --porcelain --untracked-files=normal)" && echo false || echo true; else echo false; fi)
GO_LDFLAGS += -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.modified=$(MODIFIED)
GO_LDFLAGS += -X github.com/marang/sway-session/internal/buildmetadata.Stamp=sway-session-build-v1|$(VERSION)|$(COMMIT)|$(MODIFIED)|end-sway-session-build-v1
GO_FILES := $(shell find cmd internal scripts -name '*.go' -type f)
DOC_ROOT := $(PREFIX)/share/doc/sway-session
VERIFY_RUN = $(if $(VERIFY_PROFILE_DIR),python3 -B scripts/verify-profile.py --step $@ --output "$(VERIFY_PROFILE_DIR)" --,)

.PHONY: build install clean fmt fmt-check test race vet lint apparmor-check completion-check packaging-check standalone-check diff-check verify verify-go verify-other lifecycle-check vm-harness-check vm-reboot-check release-gate-check

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@$(VERIFY_RUN) sh -c 'test -z "$$(gofmt -l $(GO_FILES))" || { \
		echo "Go files need formatting:"; \
		gofmt -l $(GO_FILES); \
		exit 1; \
	}'

test:
	$(VERIFY_RUN) go test -count=1 ./...

race:
	$(VERIFY_RUN) go test -race -count=1 ./...

vet:
	$(VERIFY_RUN) go vet ./...

lint:
	$(VERIFY_RUN) staticcheck ./...

apparmor-check:
	$(VERIFY_RUN) sh scripts/check-apparmor-policy.sh

completion-check:
	$(VERIFY_RUN) sh scripts/check-completions.sh

packaging-check:
	$(VERIFY_RUN) sh scripts/check-packaging.sh

standalone-check:
	$(VERIFY_RUN) sh scripts/check-standalone-boundary.sh

build:
	$(VERIFY_RUN) env CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-session ./cmd/sway-session

diff-check:
	$(VERIFY_RUN) sh -c 'git diff --check && git diff --cached --check'

lifecycle-check:
	sh scripts/verify-lifecycle.sh

vm-harness-check:
	$(VERIFY_RUN) python3 -B -m unittest discover -s scripts/vm-reboot -p 'test_*.py'

vm-reboot-check:
	python3 -B scripts/verify-vm-reboot.py $(VM_ARGS)

release-gate-check:
	$(VERIFY_RUN) python3 -B -m unittest discover -s scripts/release-gate -p 'test_*.py'

# Recursive serial lanes work on GNU Make 4.3 as well as 4.4. Named
# .NOTPARALLEL prerequisites only serialize individual targets on Make 4.4;
# Make 4.3 would serialize the entire gate, defeating CI's bounded overlap.
verify-go: fmt-check
	+$(MAKE) --no-print-directory --jobs=1 test race vet lint build

verify-other: fmt-check
	+$(MAKE) --no-print-directory --jobs=1 apparmor-check completion-check packaging-check standalone-check vm-harness-check release-gate-check diff-check

verify: verify-go verify-other

install: build
	install -d "$(PREFIX)/bin"
	install -m755 sway-session "$(PREFIX)/bin/sway-session"
	install -d "$(PREFIX)/share/bash-completion/completions"
	install -d "$(PREFIX)/share/zsh/site-functions"
	install -d "$(PREFIX)/share/fish/vendor_completions.d"
	install -d "$(DOC_ROOT)/contrib/sway-session"
	install -d "$(DOC_ROOT)/contrib/herdr"
	install -d "$(DOC_ROOT)/contrib/codex"
	install -d "$(DOC_ROOT)/contrib/apparmor"
	install -d "$(DOC_ROOT)/scripts"
	install -d "$(DOC_ROOT)/docs/adr"
	install -d "$(DOC_ROOT)/docs/research"
	install -d "$(DOC_ROOT)/docs/assets"
	install -m644 docs/assets/*.jpeg "$(DOC_ROOT)/docs/assets/"
	install -m644 docs/branding.md "$(DOC_ROOT)/docs/branding.md"
	install -m644 docs/agent-reporting.md "$(DOC_ROOT)/docs/agent-reporting.md"
	install -m644 docs/lifecycle-recovery.md "$(DOC_ROOT)/docs/lifecycle-recovery.md"
	install -m644 docs/named-work-environments.md "$(DOC_ROOT)/docs/named-work-environments.md"
	install -m644 docs/research/lab-374-herdr-remote-restoration.md "$(DOC_ROOT)/docs/research/lab-374-herdr-remote-restoration.md"
	install -m644 docs/research/lab-375-reach-focus-capability.md "$(DOC_ROOT)/docs/research/lab-375-reach-focus-capability.md"
	install -m644 docs/research/herdr-plugin-session-deletion.md "$(DOC_ROOT)/docs/research/herdr-plugin-session-deletion.md"
	install -m644 README.md "$(DOC_ROOT)/README.md"
	install -m644 LICENSE "$(DOC_ROOT)/LICENSE"
	install -m644 docs/sway-session-plan.md "$(DOC_ROOT)/docs/sway-session-plan.md"
	install -m644 docs/sway-session-verification.md "$(DOC_ROOT)/docs/sway-session-verification.md"
	install -m644 docs/releasing.md "$(DOC_ROOT)/docs/releasing.md"
	install -m644 docs/workflow_conventions.md "$(DOC_ROOT)/docs/workflow_conventions.md"
	install -m644 docs/adr/0001-sqlite-session-runtime-state.md "$(DOC_ROOT)/docs/adr/0001-sqlite-session-runtime-state.md"
	install -m644 docs/adr/0002-nonowning-work-environments.md "$(DOC_ROOT)/docs/adr/0002-nonowning-work-environments.md"
	install -m644 docs/adr/0003-explicit-remote-terminal-bindings.md "$(DOC_ROOT)/docs/adr/0003-explicit-remote-terminal-bindings.md"
	install -m644 contrib/completions/bash/sway-session "$(PREFIX)/share/bash-completion/completions/sway-session"
	install -m644 contrib/completions/zsh/_sway-session "$(PREFIX)/share/zsh/site-functions/_sway-session"
	install -m644 contrib/completions/fish/sway-session.fish "$(PREFIX)/share/fish/vendor_completions.d/sway-session.fish"
	install -m644 contrib/sway/50-sway-session.conf "$(DOC_ROOT)/50-sway-session.conf"
	install -m644 contrib/sway-session/config.toml "$(DOC_ROOT)/contrib/sway-session/config.toml"
	install -m644 contrib/herdr/config.toml "$(DOC_ROOT)/contrib/herdr/config.toml"
	install -m644 contrib/codex/hooks.json "$(DOC_ROOT)/contrib/codex/hooks.json"
	install -m644 contrib/apparmor/agent-home-guard "$(DOC_ROOT)/contrib/apparmor/agent-home-guard"
	install -m755 scripts/verify-codex-boundary.sh "$(DOC_ROOT)/scripts/verify-codex-boundary.sh"

clean:
	rm -f $(BINARIES)
