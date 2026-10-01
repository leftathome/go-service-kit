GOFLAGS ?= -mod=readonly
export GOFLAGS

# Pinned tool versions. golangci-lint is deliberately a binary install rather
# than a `tool` directive: it drags in a very large dependency graph that
# has no business in this module's graph. Pinning govulncheck matters too --
# `@latest` makes the gate non-reproducible, so a build can start failing
# because a tool moved rather than because the code changed.
GOLANGCI_LINT_VERSION  ?= v2.12.2
GOVULNCHECK_VERSION    ?= v1.6.0

# DOWNLOADS ARE RETRIED, VERDICTS ARE NOT (quark-q5a). The gates used to
# `go run <tool>@<version>`, which fetches the tool from proxy.golang.org inside
# the very command whose exit status is the verdict. On the homelab runners that
# fetch intermittently dies ("net/http: TLS handshake timeout"), and a download
# blip read as a red vulncheck on a merge-ready MR, needing a manual retry.
#
# So the two are now separate steps. `go install` puts the pinned tool in
# $(TOOLS_BIN) through scripts/retry.sh (bounded attempts, backoff, a time
# budget), `mod-download` fetches the module graph the same way, and THEN the
# gate runs the installed binary exactly once, unwrapped. A lint finding, a
# failing test or a real vulnerability still fails on the first run and is never
# re-rolled. scripts/retry_test.go drives these targets with a fake toolchain
# and fails if a verdict is ever retried.
RETRY     ?= sh scripts/retry.sh
TOOLS_BIN ?= $(CURDIR)/bin/tools

# Prefer a golangci-lint already on PATH (fast, offline); fall back to the
# pinned module. Without the fallback `make lint` passes on a developer box and
# fails on a runner that has no golangci-lint installed -- which is exactly how
# this repo's GitHub CI was red from its first commit while every local run
# looked green. Both CIs call `make lint`, so the resolution belongs here, once.
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null)
ifeq ($(GOLANGCI_LINT),)
GOLANGCI_LINT         = $(TOOLS_BIN)/golangci-lint
GOLANGCI_LINT_INSTALL = install-golangci-lint
endif

# Same shape: set GOVULNCHECK to use a binary of your own and skip the install.
ifeq ($(GOVULNCHECK),)
GOVULNCHECK         = $(TOOLS_BIN)/govulncheck
GOVULNCHECK_INSTALL = install-govulncheck
endif

.DEFAULT_GOAL := help
.PHONY: help test lint vulncheck tidy
.PHONY: mod-download install-golangci-lint install-govulncheck

help: ## Print this help
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'
	@echo ""
	@echo "GOFLAGS: $(GOFLAGS)"

test: mod-download ## Run the unit tests with the race detector
	go test ./... -count=1 -race

lint: mod-download $(GOLANGCI_LINT_INSTALL) ## go vet, then golangci-lint over the whole module
	go vet ./...
	$(GOLANGCI_LINT) run

vulncheck: mod-download $(GOVULNCHECK_INSTALL) ## Check the dependency graph against the Go vulnerability database
	$(GOVULNCHECK) ./...

# The download steps. Everything that talks to the module proxy on behalf of a
# gate lives in these three recipes, behind $(RETRY), and nothing else does.
#
# `go list -deps -test`, not `go mod download`: it fetches exactly the modules
# the build and the tests import, so with a warm cache it needs no network at
# all. `go mod download` also wants modules nothing here compiles, and so fails
# offline on a tree that builds and tests fine.
mod-download: ## Fetch the modules the build and tests need, retrying a failed download
	$(RETRY) go list -deps -test ./... >/dev/null

install-golangci-lint:
	GOBIN='$(TOOLS_BIN)' $(RETRY) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

install-govulncheck:
	GOBIN='$(TOOLS_BIN)' $(RETRY) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

tidy: ## Tidy go.mod/go.sum (the one target allowed to write them)
	GOFLAGS=-mod=mod go mod tidy
