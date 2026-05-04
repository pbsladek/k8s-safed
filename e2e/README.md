# E2E Test Conventions

The e2e suite runs only with the `e2e` build tag:

```bash
make e2e
make e2e-run TEST=TestDrain_NATS
```

The suite creates a k3d cluster, installs Helm chart workloads, builds the
local `kubectl-safed` binary, and runs real drain commands against Kubernetes.

Useful environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `SAFED_E2E_CLUSTER_NAME` | `safed-e2e` | k3d cluster name. |
| `SAFED_E2E_FLANNEL_BACKEND` | `host-gw` | k3s flannel backend. Set empty to use k3s default. |
| `K3S_IMAGE` | | Optional k3s image passed to k3d. |

The framework package is intentionally not hidden behind the `e2e` build tag
so editor tooling can load helper files such as `framework/helm.go`.
