BINARIES := sway-title-animator sway-session
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=
GO_FILES := $(shell find . -name '*.go' -type f)

.PHONY: build install clean fmt fmt-check test race vet lint apparmor-check packaging-check diff-check verify

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

packaging-check:
	sh scripts/check-packaging.sh

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-title-animator ./cmd/sway-title-animator
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o sway-session ./cmd/sway-session

diff-check:
	git diff --check
	git diff --cached --check

verify: fmt-check test race vet lint apparmor-check packaging-check build diff-check

install: build
	install -d $(PREFIX)/bin
	for binary in $(BINARIES); do install -m755 $$binary $(PREFIX)/bin/$$binary; done

clean:
	rm -f $(BINARIES)
