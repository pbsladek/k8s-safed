# Review Fix Tracker

This file tracks the implementation pass for the maintainability, testability,
and architecture review findings.

## Phase 1: Drain Correctness

- [x] Make `--skip-workload` / `--only-workload` protect filtered managed pods
  from conventional eviction.
- [x] Wait for workload pods and conventionally evicted pods to be deleted before
  reporting success.
- [x] Suppress event creation during dry-run.
- [x] Bound best-effort event creation with a short timeout.
- [x] Strengthen checkpoint resume validation.

## Phase 2: Multi-Node And Ownership

- [x] Coordinate parallel multi-node drains so shared workloads are restarted
  once.
- [x] Add warnings or safeguards for unbounded concurrency modes.
- [x] Make workload owner resolution validate controller ownership and UID.
- [x] Make workload finder caches request-scoped.

## Phase 3: Maintainability

- [x] Centralize transient Kubernetes API error classification.
- [x] Reduce config/profile wiring drift with table-driven coverage.
- [x] Make zero-value timeout semantics explicit in docs/tests.
- [x] Scope preflight PDB warnings to related workloads.
- [x] Update Krew plugin metadata to use canonical flags.

## Phase 4: Testability

- [x] Replace e2e skip-based core coverage with deterministic workloads or hard
  failures.
- [x] Add bounded e2e cleanup helpers.
- [x] Add unit tests for PDB eviction retry behavior.
- [x] Strengthen event lifecycle e2e assertions.
- [x] Add focused rollout selector false-positive unit tests.
- [x] Add command orchestration unit coverage.
- [x] Replace no-op concurrency tests with tests that exercise workload
  concurrency.
