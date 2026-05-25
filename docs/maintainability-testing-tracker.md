# Maintainability And Testing Tracker

Progress tracker for the maintainability and testability implementation pass.

## Maintainability

- [x] Split `pkg/drain/drain.go` into focused files for rollout, eviction, pod
  waits, and workload coordination.
- [x] Add narrow Kubernetes operation seams where they make tests clearer.
- [x] Consolidate repeated e2e setup and expectation helpers.

## Unit And Golden Tests

- [x] Add direct tests for workload filtering and protected pod eviction.
- [x] Add concurrency tests for `WorkloadCoordinator`.
- [x] Add golden tests for text and JSON log output.
- [x] Add fuzz/property tests for workload filtering/config-style parsing.

## E2E And CI Support

- [x] Add a short PR-oriented e2e target.
- [x] Add a local e2e preflight target for mise/k3d/helm/kubectl readiness.
- [x] Improve failed e2e artifact layout by test name.
- [x] Add unit coverage reporting.
- [x] Add docs/example validation for `docs/examples`.

## Verification

- [x] `make test`
- [x] `make lint`
- [x] `git diff --check`
- [x] targeted e2e PR run (`make e2e-pr E2E_TIMEOUT=25m`)
- [x] full `make e2e`
