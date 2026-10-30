GO ?= go

.PHONY: build check test format format-check tidy

build:
	$(GO) build -o bin/gitea-pages ./cmd/gitea-pages

check:
	$(MAKE) format-check
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) build ./...

test:
	$(GO) test ./...

format:
	$(GO) fmt ./...

format-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -type f))" || { gofmt -l $$(find . -name '*.go' -type f); exit 1; }

tidy:
	$(GO) mod tidy
