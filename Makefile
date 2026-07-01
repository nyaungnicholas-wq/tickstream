GO      ?= go
BINDIR  := ./bin

.PHONY: help tidy vet lint test bench build run clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-8s %s\n", $$1, $$2}'

tidy: ## go mod tidy
	$(GO) mod tidy

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint
	golangci-lint run

test: ## unit tests with the race detector
	$(GO) test -race -buildvcs ./...

bench: ## micro-benchmarks (NEVER with -race; race timings are garbage)
	$(GO) test -run=^$$ -bench=. -benchmem -count=10 ./...

build: ## build the daemon
	$(GO) build -o $(BINDIR)/tickstreamd ./cmd/tickstreamd

run: build ## build and run the daemon
	$(BINDIR)/tickstreamd

clean: ## remove build artifacts
	rm -rf $(BINDIR)
