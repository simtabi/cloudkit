# The Go module lives in src/, so every target runs go with -C src. A bare
# `go test ./...` at the repository root finds no packages and passes.
GO      ?= go
GOLANGCI_LINT_VERSION ?= v2.15.0

.PHONY: help ci fmt fmt-check vet test lint tidy

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

ci: fmt-check vet test ## What CI runs: format check, vet, tests (policy tests included)

fmt: ## Format the module
	gofmt -w src

fmt-check: ## Fail if any file is not gofmt-clean
	@out="$$(gofmt -l src)"; if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) -C src vet ./...

test: ## Tests with the race detector
	$(GO) -C src test -race -count=1 ./...

lint: ## golangci-lint, built with this Go so it understands the stdlib it reads
	cd src && $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

tidy: ## go mod tidy
	$(GO) -C src mod tidy
