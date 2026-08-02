GOFLAGS ?= -mod=readonly
export GOFLAGS

# Pinned tool versions. golangci-lint is deliberately a binary/`go run` install
# rather than a `tool` directive: it drags in a very large dependency graph that
# has no business in this module's graph. Pinning govulncheck matters too --
# `@latest` makes the gate non-reproducible, so a build can start failing
# because a tool moved rather than because the code changed.
GOLANGCI_LINT_VERSION  ?= v2.12.2
GOVULNCHECK_VERSION    ?= v1.6.0

# Prefer a golangci-lint already on PATH (fast, offline); fall back to the
# pinned module. Without the fallback `make lint` passes on a developer box and
# fails on a runner that has no golangci-lint installed -- which is exactly how
# this repo's GitHub CI was red from its first commit while every local run
# looked green. Both CIs call `make lint`, so the resolution belongs here, once.
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null)
ifeq ($(GOLANGCI_LINT),)
GOLANGCI_LINT = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
endif

GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

.DEFAULT_GOAL := help
.PHONY: help test lint vulncheck tidy

help: ## Print this help
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'
	@echo ""
	@echo "GOFLAGS: $(GOFLAGS)"

test: ## Run the unit tests with the race detector
	go test ./... -count=1 -race

lint: ## go vet, then golangci-lint over the whole module
	go vet ./...
	$(GOLANGCI_LINT) run

vulncheck: ## Check the dependency graph against the Go vulnerability database
	$(GOVULNCHECK) ./...

tidy: ## Tidy go.mod/go.sum (the one target allowed to write them)
	GOFLAGS=-mod=mod go mod tidy
