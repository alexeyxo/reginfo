GO ?= go
GOLANGCI_LINT ?= golangci-lint
LINTER_VERSION ?= v2.14.0
BINARY_NAME ?= reginfo
BUILD_DIR ?= build

.DEFAULT_GOAL := help
.PHONY: help install-linter fmt lint test tests build run check

install-linter: ## Install the pinned golangci-lint version
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINTER_VERSION)

fmt: ## Format Go source files
	$(GO) fmt ./...

lint: ## Lint all packages
	$(GOLANGCI_LINT) run --config=./golangci.yml ./...

test: ## Run tests with race detection and coverage
	mkdir -p "$(BUILD_DIR)"
	$(GO) test -race -count=1 -coverprofile="$(BUILD_DIR)/coverage.out" ./...

tests: test ## Alias for make test

build: ## Build the application in build/reginfo
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/$(BINARY_NAME)" ./example/reginfo

run: ## Run the example application
	$(GO) run ./example/reginfo

check: lint test build ## Run lint, tests, and build

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "%-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
