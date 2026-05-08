# E2E Test Conventions

The e2e suite runs only with the `e2e` build tag:

```bash
make e2e
make e2e-core
make e2e-rbac
make e2e-run TEST=TestDrain_NATS
```

The suite creates a k3d cluster, installs Helm chart workloads, builds the
local `kubectl-safed` binary, and runs real drain commands against Kubernetes.

Useful environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `SAFED_E2E_CLUSTER_NAME` | `safed-e2e` | k3d cluster name. |
| `SAFED_E2E_FLANNEL_BACKEND` | `host-gw` | k3s flannel backend. Set empty to use k3s default. |
| `SAFED_E2E_ARTIFACT_DIR` | `/tmp/safed-e2e-diagnostics` | Directory for per-test diagnostics artifacts. |
| `SAFED_E2E_GO` | `go` | Go binary used by the e2e harness when building `kubectl-safed`. Make sets this to the repo-pinned mise Go when available. |
| `SAFED_E2E_STREAM` | | Stream drain command output while tests run. |
| `K3S_IMAGE` | | Optional k3s image passed to k3d. |

The framework package is intentionally not hidden behind the `e2e` build tag
so editor tooling can load helper files such as `framework/helm.go`.

## Test Tiers

| Target | Scope |
|---|---|
| `make e2e-full` | Full real-cluster suite. This is also what `make e2e` runs. |
| `make e2e-core` | Main happy-path, preflight, config, failure, checkpoint, and RBAC coverage. |
| `make e2e-smoke` | Fastest confidence check for representative NATS/Grafana behavior. |
| `make e2e-preflight` | Preflight detection, validation, and dry-run edge cases. |
| `make e2e-config` | Config defaults, modes, profiles, env vars, and CLI precedence. |
| `make e2e-rbac` | Restricted-ServiceAccount permission failures and best-effort event emission. |
| `make e2e-failures` | CrashLoop, PDB, rollout-timeout, and interrupt/failure handling. |
| `make e2e-eviction` | Remaining-pod eviction policy, force-delete, and deletion races. |
| `make e2e-checkpoint` | Checkpoint creation, validation, resume, timeout, and corrupt-file handling. |
| `make e2e-observability` | JSON logs, event emission, skip output, and status formatting. |
| `make e2e-nodes` | Multi-node selection, deduplication, and selector behavior. |
| `make e2e-focused` | Recently added edge cases for config, checkpoint, RBAC, node concurrency, and deletion races. |

## Coverage Matrix

| Area | Covered scenarios |
|---|---|
| Workload restarts | Deployment, StatefulSet, NATS, Grafana, kube-state-metrics, priority ordering, concurrency, skip/only filters. |
| Preflight | Single replica, PDB warnings, StatefulSet risk, stateful name patterns, invalid options, strict/warn/off modes. |
| Eviction | PDB retry timeout, standalone pod force handling, emptyDir policy, force-delete standalone pods, stale owner references, terminating pods. |
| Checkpoint/resume | Completed workload skip, killed process resume, global timeout persistence, wrong-node validation, corrupt checkpoint rejection before cordon. |
| RBAC | Missing node patch, missing workload patch, missing pod eviction, missing events create as best-effort warning. |
| Config | Defaults, built-in modes, named profiles, CLI override, unknown fields, env config, explicit `--config` precedence. |
| Multi-node | Selector targeting, node de-duplication, parallel node drains, partial failure cleanup with `--node-concurrency`. |
| Failure handling | CrashLoop detection, rollout timeout, uncordon-on-failure, already-cordoned no-op behavior, dry-run no-mutation behavior. |
| Observability | Human output, JSON output, Kubernetes events, diagnostics logs and artifact files on setup/test failures. |
