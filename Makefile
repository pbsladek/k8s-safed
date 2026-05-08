BINARY     := kubectl-safed
MODULE     := github.com/pbsladek/k8s-safed
MISE_GO_ROOT := $(shell if command -v mise >/dev/null 2>&1 && [ -f .mise.toml ]; then mise where go 2>/dev/null; fi)
GO         ?= $(if $(MISE_GO_ROOT),$(MISE_GO_ROOT)/bin/go,go)
GO_ROOT    ?= $(shell $(GO) env GOROOT 2>/dev/null)
GO_ENV     := env "PATH=$(GO_ROOT)/bin:$(PATH)" "GOROOT=$(GO_ROOT)"
GOFLAGS    := -trimpath
LDFLAGS    := -s -w

# Respect GOBIN / PATH install location; default to /usr/local/bin.
INSTALL_DIR ?= /usr/local/bin

.PHONY: all build test vet lint fmt check install clean release snapshot help e2e e2e-full e2e-core e2e-smoke e2e-preflight e2e-config e2e-rbac e2e-failures e2e-eviction e2e-checkpoint e2e-observability e2e-nodes e2e-focused e2e-run

all: check build ## Run checks then build (default)

## ── Build ────────────────────────────────────────────────────────────────────

build: ## Build the binary for the current platform
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build ## Build and install to INSTALL_DIR (default /usr/local/bin)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

## ── Quality ──────────────────────────────────────────────────────────────────

test: ## Run all tests with race detector
	$(GO) test -race ./...

test-v: ## Run all tests verbose
	$(GO) test -race -v ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go source files
	$(GO) fmt ./...

lint: ## Run golangci-lint (requires golangci-lint to be installed)
	$(GO_ENV) golangci-lint run ./...

check: fmt vet test lint ## Format, vet, test, and lint

## ── E2E tests ─────────────────────────────────────────────────────────────────

E2E_TIMEOUT ?= 35m
E2E_FLAGS   ?= -v -tags=e2e -count=1 -timeout=$(E2E_TIMEOUT)
E2E_PKG     ?= ./e2e/...
E2E_ENV     ?= SAFED_E2E_GO=$(GO)

e2e: ## Run e2e tests against a real k3d cluster (requires k3d in PATH)
	$(MAKE) e2e-full

e2e-full: ## Run the complete e2e suite
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) $(E2E_PKG)

e2e-core: ## Run core e2e coverage suitable for PR validation
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(DryRun|NATS|Grafana|MultipleWorkloads|Preflight_StrictMode|ConfigDefaultsModeProfilePrecedence|CheckpointResume|CorruptCheckpointFailsBeforeCordon|RBACMissingNodePatchFailsBeforeMutation|PDBAllowedEviction)$$' $(E2E_PKG)

e2e-smoke: ## Run a minimal e2e smoke suite
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(DryRun|Preflight_StrictMode|RBACMissingNodePatchFailsBeforeMutation)$$' $(E2E_PKG)

e2e-preflight: ## Run preflight-focused e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_Preflight_' $(E2E_PKG)

e2e-config: ## Run config/mode/profile e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(ProfileConfigAndCLIOverride|ConfigDefaultsModeProfilePrecedence|ConfigEnvAndExplicitConfigPrecedence|ConfigValidationAndModeErrors|CustomStatefulPatternAndInvalidPriorityWarning|InvalidOptions)$$' $(E2E_PKG)

e2e-rbac: ## Run RBAC/permission e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_RBAC' $(E2E_PKG)

e2e-failures: ## Run failure-mode e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(CrashLoopAbort|ImagePullAbort|UncordonOnFailure|AlreadyCordonedFailureDoesNotUncordon)$$' $(E2E_PKG)

e2e-eviction: ## Run eviction/PDB/DaemonSet e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(DaemonSet|UnmanagedPodEvictionOptions|PDB|StaleReplicaSetOwnerPodIsSkippedAsWorkload|TerminatingPodIsSkippedDuringEviction)' $(E2E_PKG)

e2e-checkpoint: ## Run checkpoint/resume e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_.*Checkpoint|TestDrain_GlobalTimeoutKeepsCheckpointAndUncordons' $(E2E_PKG)

e2e-observability: ## Run events/logging e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(EmitEvents|EmitEventsFailure|MultiNamespace|JSONLogFormat|JSONLogFormatFailure)$$' $(E2E_PKG)

e2e-nodes: ## Run node selector and multi-node e2e tests
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(MultiNodeRejectsCheckpointPath|NodeSelector|NodeSelectorErrors|MultiNode|MultiNodePartialFailureUncordonsFailedNodeOnly)$$' $(E2E_PKG)

e2e-focused: ## Run recently added edge-case e2e coverage
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run 'TestDrain_(SkipWorkload|OnlyWorkload|ConfigEnvAndExplicitConfigPrecedence|CorruptCheckpointFailsBeforeCordon|CheckpointResumeRejectsContextMismatchBeforeCordon|AlreadyCordonedFailureDoesNotUncordon|MultiNodePartialFailureUncordonsFailedNodeOnly|EmitEvents|EmitEventsFailure|StaleReplicaSetOwnerPodIsSkippedAsWorkload|TerminatingPodIsSkippedDuringEviction|RBACMissingDeploymentPatchUncordonsAfterFailure|RBACMissingPodEvictionUncordonsAfterFailure|RBACMissingEventCreateIsBestEffort|RBACNamespacedPodListFailsBeforeMutation)$$' $(E2E_PKG)

e2e-run: ## Run a single e2e test by name: make e2e-run TEST=TestDrain_Basic
	$(E2E_ENV) $(GO) test $(E2E_FLAGS) -run $(TEST) $(E2E_PKG)

## ── Dependencies ─────────────────────────────────────────────────────────────

deps: ## Download and verify modules
	$(GO) mod download
	$(GO) mod verify

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

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
