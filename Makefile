BINARY := sway-title-animator
PREFIX ?= $(HOME)/.local
GO_BUILD_FLAGS := -trimpath -buildvcs=false
GO_LDFLAGS := -s -w -buildid=
GO_FILES := $(shell find . -name '*.go' -type f)

.PHONY: build install clean fmt fmt-check test race vet lint diff-check verify

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

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' -o $(BINARY) ./cmd/sway-title-animator

diff-check:
	git diff --check
	git diff --cached --check

verify: fmt-check test race vet lint build diff-check

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
