# Maintainability And Testability Review Plan

This plan consolidates three focused reviews:

- maintainability and repo conventions
- test coverage gaps
- testability and deterministic testing

The goal is to improve confidence without turning every change into a slow k3d
run. Fast unit and docs checks should cover command/config semantics and drain
state-machine behavior; e2e should stay focused on Kubernetes contracts that
fake clients cannot prove.

## Phase 1: Drift Fixes And Fast Guardrails

- [x] Align workload filtering documentation and comments with current behavior:
  `--skip-workload` and `--only-workload` leave filtered managed workloads
  untouched by restart and conventional eviction.
- [x] Expand docs validation beyond `docs/examples/README.md`:
  - scan `README.md`, `docs/user-guide.md`, `docs/index.md`, and
    `docs/examples/README.md`
  - extract `kubectl safed drain` examples from markdown code blocks
  - validate documented flags against Cobra command definitions without
    executing Kubernetes calls
  - reject stale hidden/removed flag names unless intentionally documented as
    compatibility aliases
- [x] Add fast CLI argument/config unit tests:
  - missing node and selector is rejected
  - node args plus `--selector` is rejected
  - `--skip-workload` plus `--only-workload` is rejected
  - missing default config is ignored without a requested profile
  - missing config from `KUBECTL_SAFED_CONFIG` returns an error
- [x] Verify with:
  - `mise exec -- make test`
  - `mise exec -- make docs-examples-check`
  - `mise exec -- make lint`

## Phase 2: Kubernetes Semantics Coverage

- [x] Add preflight coverage for workloads that can accept a restart patch but
  not progress safely:
  - paused Deployments
  - StatefulSets with `OnDelete` update strategy
  - StatefulSets with rolling-update partition set
- [x] Add matching strict-mode e2e coverage proving these abort before cordon.
- [x] Add real DaemonSet eviction override e2e coverage for
  `--ignore-daemonsets=false` outside dry-run.
- [x] Add workload discovery unit coverage for:
  - pods on other nodes are ignored
  - owner references with nil or false `Controller` are ignored
  - stale owner UID/name mismatches do not resolve to active workloads
- [x] Verify with:
  - `mise exec -- make test`
  - compile-only e2e verification with `go test -c -tags=e2e`
  - `mise exec -- make e2e-pr E2E_TIMEOUT=25m`
  - `mise exec -- make e2e-preflight E2E_TIMEOUT=25m`
  - `mise exec -- make e2e-eviction E2E_TIMEOUT=25m`

## Phase 3: Deterministic Time And Output

- [x] Introduce a small clock/sleeper abstraction for drain internals.
- [x] Route these through the clock:
  - restart patch `restartedAt` timestamp
  - checkpoint completion timestamps
  - event names/timestamps
  - elapsed output
  - retry sleeps and PDB backoff
- [x] Add deterministic unit/golden tests for:
  - restart patch content
  - checkpoint JSON with fixed timestamps
  - event objects with stable names/timestamps
  - plain and JSON output with stable elapsed values
- [x] Verify with `mise exec -- make test` and `mise exec -- make lint`.

## Phase 4: Command And Option Maintainability

- [x] Add public option metadata so each option has one inventory entry for:
  - CLI flag name and default
  - config/profile key
  - validation rule
  - docs/help text
  - whether it is deprecated or hidden
- [x] Use that metadata to reduce drift in:
  - Cobra flag registration
  - command-surface tests
  - docs validation
- [x] Extract command orchestration out of `cmd/drain.go`:
  - config/default/mode/profile resolution
  - node selection
  - checkpoint path resolution
  - multi-node execution
- [x] Keep Cobra focused on parsing, help, and wiring.
- [x] Verify with command unit tests and docs validation before broader e2e.

## Phase 5: Drain State-Machine Testability

- [x] Split `Drainer.Run` orchestration behind explicit phase collaborators while
  preserving the public `drain.Options` API:
  - workload discovery
  - node cordon/uncordon
  - preflight
  - rollout/restart
  - remaining pod eviction
  - checkpoint store
  - event sink
- [x] Introduce a per-run state struct for data currently kept as mutable
  drainer fields, especially protected workloads and checkpoint state.
- [x] Add fast deterministic tests with simple fakes for:
  - successful phase ordering
  - preflight abort before cordon
  - bad checkpoint abort before cordon
  - rollout failure uncordons only when this run cordoned the node
  - checkpoint completion and cleanup
  - multi-node workload coordination/deduplication
- [x] Keep Kubernetes fake-client tests for API shape and reactors; use the new
  fakes for drain state-machine behavior.

## Phase 6: E2E Suite And CI Improvements

- [x] Replace Makefile-only long regex suite definitions with suite metadata or
  Go-level grouping so test renames cannot silently drop PR coverage.
- [x] Broaden PR e2e coverage or add a medium protected-branch tier including:
  - one successful real rollout
  - one StatefulSet drain
  - checkpoint resume
  - skip/only workload behavior
  - uncordon-on-failure
- [x] Add CI coverage reporting:
  - run `make test-coverage`
  - upload `coverage.out`
  - set an initial threshold after measuring baseline
- [x] Improve e2e wait helpers:
  - central polling helper with last-observation diagnostics
  - focused object snapshots on timeout
  - recent events for the failed subject
  - avoid fixed sleeps where an observed condition can be polled
- [x] Keep full k3d e2e reserved for Kubernetes contract validation, RBAC,
  scheduler/controller behavior, and smoke confidence after fast tests pass.

## Remaining Deep Refactors

- [x] Move command orchestration from `cmd/drain.go` into an internal
  application package.
- [x] Split `Drainer.Run` into explicit phase methods.
- [x] Replace mutable drainer run state with an explicit per-run state object.

## Notes From Review

- A direct `go test ./...` in this shell hit a Go toolchain cache mismatch
  between Go 1.26.2 and 1.26.3. The mise-backed path is the repo convention and
  passed with `mise exec -- make test`.
- The final deep-refactor pass was verified with the mise-backed fast tests,
  docs/examples check, lint, coverage threshold, e2e package compile, and
  `git diff --check`.
- Existing completed trackers remain in `docs/review-fix-tracker.md`,
  `docs/e2e-improvement-plan.md`, and
  `docs/maintainability-testing-tracker.md`.
