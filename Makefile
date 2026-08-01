GOFLAGS ?= -mod=readonly
export GOFLAGS

.PHONY: help test lint vulncheck tidy
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

test: ## Run tests with the race detector
	go test ./... -count=1 -race

lint: ## Run go vet and golangci-lint
	go vet ./...
	golangci-lint run

vulncheck: ## Scan dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy: ## Tidy go.mod
	go mod tidy
