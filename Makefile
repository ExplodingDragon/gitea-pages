GO ?= go

.PHONY: build check test format tidy

build:
	$(GO) build -o bin/gitea-pages ./cmd/gitea-pages

check:
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) build ./...

test:
	$(GO) test ./...

format:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy
