.DEFAULT_GOAL := help

help: ## Show commands
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'


.PHONY: build test lint all clean hel

all: lint test build  ## Run all steps: lint, test, and build

build:  ## Build the DAP server
	go build -o dap ./cmd/dap

test:  ## Run tests
	go test ./...

lint:  ## Run linters
	golangci-lint run ./...

clean:  ## Clean up build artifacts
	rm -f dap
