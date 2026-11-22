.PHONY: help check test lint docs format build clean smoke tools

GOLANGCI_LINT_VERSION := 2.12.2
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH | cut -d: -f1)/bin
endif
GOLANGCI_LINT := $(GOBIN)/golangci-lint

help: ## List available targets
	@awk -F ':.*## ' '/^[a-z][a-z-]*:.*## / { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

check: lint docs test ## Run all quality gates

lint: ## Run pinned golangci-lint checks
	@"$(GOLANGCI_LINT)" version | grep -q 'version $(GOLANGCI_LINT_VERSION) ' || { echo 'Run make tools to install pinned golangci-lint' >&2; exit 1; }
	"$(GOLANGCI_LINT)" run ./...
	"$(GOLANGCI_LINT)" fmt --diff

docs: ## Lint Markdown
	npx --yes markdownlint-cli2@0.24.2 '**/*.md'

test: ## Run all tests with the race detector
	go test -race -count=1 ./...

smoke: ## Install the example and inspect isolated-home artifacts
	go test -race -count=1 -run '^TestInstalledSmoke$$' -v ./internal/smoke

format: ## Format Go source
	"$(GOLANGCI_LINT)" fmt

build: ## Build the example binary
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -o dist/punt-observe-smoke ./cmd/punt-observe-smoke

clean: ## Remove build artifacts
	rm -rf dist .tmp

tools: ## Install the pinned, checksum-verified golangci-lint release
	@set -eu; \
	mkdir -p .tmp "$(GOBIN)"; \
	work=$$(mktemp -d "$(CURDIR)/.tmp/lint.XXXXXX"); trap 'rm -rf "$$work"' EXIT; \
	base=golangci-lint-$(GOLANGCI_LINT_VERSION); \
	archive=$$base-$$(go env GOOS)-$$(go env GOARCH).tar.gz; \
	url=https://github.com/golangci/golangci-lint/releases/download/v$(GOLANGCI_LINT_VERSION); \
	cd "$$work"; \
	curl -fsSLO "$$url/$$archive"; curl -fsSLO "$$url/$$base-checksums.txt"; \
	grep -F "  $$archive" "$$base-checksums.txt" | shasum -a 256 -c -; \
	tar xzf "$$archive"; install -m 0755 "$${archive%.tar.gz}/golangci-lint" "$(GOLANGCI_LINT)"
