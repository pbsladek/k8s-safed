BINARY     := kubectl-safed
MODULE     := github.com/pbsladek/k8s-safed
GOFLAGS    := -trimpath
LDFLAGS    := -s -w

# Respect GOBIN / PATH install location; default to /usr/local/bin.
INSTALL_DIR ?= /usr/local/bin

.PHONY: all build test vet lint fmt check install clean release snapshot help e2e e2e-run

all: check build ## Run checks then build (default)

## ── Build ────────────────────────────────────────────────────────────────────

build: ## Build the binary for the current platform
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build ## Build and install to INSTALL_DIR (default /usr/local/bin)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

## ── Quality ──────────────────────────────────────────────────────────────────

test: ## Run all tests with race detector
	go test -race ./...

test-v: ## Run all tests verbose
	go test -race -v ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go source files
	go fmt ./...

lint: ## Run golangci-lint (requires golangci-lint to be installed)
	golangci-lint run ./...

check: fmt vet test lint ## Format, vet, test, and lint

## ── E2E tests ─────────────────────────────────────────────────────────────────

e2e: ## Run e2e tests against a real k3d cluster (requires k3d in PATH)
	go test -v -tags=e2e -count=1 -timeout=35m ./e2e/...

e2e-run: ## Run a single e2e test by name: make e2e-run TEST=TestDrain_Basic
	go test -v -tags=e2e -count=1 -timeout=35m -run $(TEST) ./e2e/...

## ── Dependencies ─────────────────────────────────────────────────────────────

deps: ## Download and verify modules
	go mod download
	go mod verify

tidy: ## Tidy go.mod and go.sum
	go mod tidy

## ── Release ──────────────────────────────────────────────────────────────────

release: ## Merge the open release-please PR to trigger a release
	@PR=$$(gh pr list --label "autorelease: pending" --json number --jq '.[0].number'); \
	if [ -z "$$PR" ]; then echo "No release-please PR found"; exit 1; fi; \
	gh pr merge "$$PR" --squash --auto

snapshot: ## Build a local multi-arch snapshot via GoReleaser (no publish)
	goreleaser release --snapshot --clean

releaser-check: ## Validate the .goreleaser.yaml config
	goreleaser check

## ── Housekeeping ─────────────────────────────────────────────────────────────

clean: ## Remove build artefacts
	rm -f $(BINARY)
	rm -rf dist/

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
