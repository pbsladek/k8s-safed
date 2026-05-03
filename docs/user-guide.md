---
layout: default
title: User Guide
nav_order: 2
---

# kubectl-safed User Guide

## Overview

`kubectl-safed` drains Kubernetes nodes by using rolling restarts for managed
workloads instead of relying primarily on direct pod eviction. The plugin is
installed as `kubectl-safed`, so it is invoked as:

```bash
kubectl safed drain NODE [NODE...] [--selector SELECTOR] [flags]
```

It supports the standard `kubectl` global flags such as `--kubeconfig`,
`--context`, `--namespace`, `--server`, `--token`, `--as`, and `--as-group`.

## How Draining Works

For each target node, `kubectl-safed`:

1. Validates that the node exists and reads current node state.
2. Discovers Deployments and StatefulSets with non-terminal pods on the node.
3. Filters workloads with `--skip-workload` or `--only-workload`.
4. Runs pre-flight checks unless `--preflight=off`.
5. Cordons the node.
6. Patches each workload pod template with
   `kubectl.kubernetes.io/restartedAt`, equivalent to
   `kubectl rollout restart`.
7. Waits until each rollout is complete and healthy.
8. Verifies the workload no longer has active pods on the drained node.
9. Handles remaining pods with eviction, force deletion, or skipping depending
   on flags.
10. Emits optional Kubernetes Events and writes checkpoint progress for resume.

Workloads are processed sequentially by default. `--max-concurrency` changes
how many workload rollouts run at once per node. `--node-concurrency` changes
how many nodes drain at once.

## Installation

### krew

```bash
kubectl krew install --manifest-url \
  https://github.com/pbsladek/k8s-safed/releases/latest/download/plugin.yaml
```

After publication to the official krew index:

```bash
kubectl krew install safed
```

### Manual

Download the release archive for your platform, verify the checksum, and place
the `kubectl-safed` binary on your `PATH`.

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/pbsladek/k8s-safed/releases/latest \
  | grep '"tag_name"' | cut -d'"' -f4)
BASE="https://github.com/pbsladek/k8s-safed/releases/download/${VERSION}"

curl -fsSLO "${BASE}/kubectl-safed_darwin_arm64.tar.gz"
curl -fsSLO "${BASE}/checksums.txt"
grep "kubectl-safed_darwin_arm64.tar.gz" checksums.txt | shasum -a 256 --check
tar -xzf kubectl-safed_darwin_arm64.tar.gz
sudo install -m 0755 kubectl-safed_darwin_arm64/kubectl-safed /usr/local/bin/kubectl-safed
```

### Build From Source

```bash
git clone https://github.com/pbsladek/k8s-safed.git
cd k8s-safed
make build
sudo mv kubectl-safed /usr/local/bin/
```

## RBAC

The principal running `kubectl-safed` needs node patch access, workload read
and patch access, pod list/get/delete access, eviction access, and PDB list
access when pre-flight checks are enabled.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubectl-safed
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list", "get", "delete"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["get", "list", "patch"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["list"]
```

Notes:

- `pods/delete` is only needed for `--force-delete-standalone`.
- `poddisruptionbudgets/list` is only needed when `--preflight` is not `off`.
- `events/create` is only needed with `--emit-events`.

```yaml
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create"]
```

## Command Reference

### Node Targeting

| Flag | Default | Description |
|---|---:|---|
| `--selector`, `-l` | | Drain nodes matching a label selector. Mutually exclusive with positional node names. |
| `--node-concurrency` | `1` | Number of nodes to drain at once. `1` is sequential. `0` means all targeted nodes. |

### Core Behavior

| Flag | Default | Description |
|---|---:|---|
| `--dry-run`, `-d` | `false` | Preview actions without mutating the cluster. |
| `--preflight` | `warn` | `warn`, `strict`, or `off`. Strict aborts before cordon on risk-level findings. |
| `--force`, `-f` | `false` | Allow eviction of standalone and Job-owned pods. |
| `--force-delete-standalone` | `false` | Delete standalone pods with `gracePeriodSeconds=0`; implies `--force`. |
| `--ignore-daemonsets` | `true` | Skip DaemonSet-managed pods. Set to `false` to evict them. |
| `--delete-emptydir-data` | `false` | Allow eviction of pods using `emptyDir` volumes. |
| `--grace-period` | `-1` | Pod termination grace period in seconds. `-1` uses each pod default. |
| `--max-concurrency` | `1` | Workload rollouts per node. `1` sequential, `0` all workloads, `N` batches of `N`. |
| `--uncordon-on-failure` | `false` | Uncordon if this drain cordoned the node and then failed. |
| `--skip-workload` | | Exclude `Kind/namespace/name` from rolling restart. Repeatable. |
| `--only-workload` | | Restart only listed `Kind/namespace/name` workloads. Repeatable. |
| `--emit-events` | `false` | Emit Kubernetes Events on node and workload objects. |
| `--resume` | `false` | Skip workloads already completed in the checkpoint file. |
| `--checkpoint-path` | | Override checkpoint path for a single-node drain only. |
| `--profile` | | Load defaults from a named profile in the safed config file. |
| `--config` | `~/.kube/safed.yaml` | Config file path. Also controlled by `KUBECTL_SAFED_CONFIG`. |

`--skip-workload` and `--only-workload` are mutually exclusive.

### Timeouts

| Flag | Default | Description |
|---|---:|---|
| `--timeout` | `0` | Overall drain deadline per node. `0` means no overall deadline. |
| `--rollout-timeout` | `5m` | Per-workload rollout deadline. `0` disables the per-workload deadline. |
| `--pod-vacate-timeout` | `2m` | Per-workload deadline for pods to leave the node after rollout. |
| `--eviction-timeout` | `5m` | Per-pod deadline for PDB-blocked eviction retries. |
| `--pdb-retry-interval` | `5s` | Base retry interval for PDB-blocked evictions; doubles up to 60s. |
| `--poll-interval` | `5s` | Interval between status checks. |

### Output

| Flag | Default | Description |
|---|---:|---|
| `--log-format`, `-o` | `plain` | `plain` aligned logs or newline-delimited `json`. |

## Pre-Flight Checks

Pre-flight checks run after workload discovery and filtering, but before the
node is cordoned. They surface conditions that could cause downtime or require
operator attention.

| Check | Level | Meaning |
|---|---|---|
| Single-replica Deployment | risk | Rolling restart can briefly have zero ready pods. |
| Deployment `Recreate` strategy | risk | Existing pods terminate before replacements start. |
| Single-replica StatefulSet | risk | Rolling restart causes downtime. |
| Multi-replica StatefulSet | note | Pods restart one at a time in reverse ordinal order. |
| Known stateful service name | note | Name matches a database, queue, cache, or storage pattern. |
| PDB with zero disruptions allowed | note | Remaining pod eviction may be blocked. |

Risk-level findings are logged as `RISK:`. With `--preflight=strict`, any risk
causes a non-zero exit before the node is cordoned.

Known stateful-service matching is case-insensitive and includes names such as
`postgres`, `mysql`, `redis`, `mongo`, `elasticsearch`, `kafka`, `rabbitmq`,
`nats`, `etcd`, `cassandra`, `cockroach`, `minio`, `vault`, and `memcached`.

## Workload Ordering

Workloads are ordered by `kubectl.safed.io/drain-priority`. Lower numbers run
first. Workloads without the annotation default to priority `100`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: frontend
  annotations:
    kubectl.safed.io/drain-priority: "10"
```

Within the same priority, `--max-concurrency` controls whether workloads run
sequentially, in batches, or all at once.

## Workload Filtering

Use the exact log subject format: `Kind/namespace/name`.

```bash
kubectl safed drain worker-1 --skip-workload=Deployment/default/batch-processor

kubectl safed drain worker-1 \
  --only-workload=Deployment/default/api \
  --only-workload=StatefulSet/data/postgres
```

Filtered-out workloads are not rolling-restarted. Pods remaining on the node
still fall through to the remaining-pod handling phase.

## Remaining Pods

After managed workloads are restarted and cleared from the node, remaining pods
are handled as follows:

| Pod Type | Default Behavior | Override |
|---|---|---|
| Mirror/static pods | skipped | none |
| Terminal pods | skipped | none |
| Terminating pods | skipped | none |
| DaemonSet pods | skipped | `--ignore-daemonsets=false` |
| `emptyDir` pods | blocked | `--delete-emptydir-data` or `--force` |
| Standalone pods | blocked | `--force` or `--force-delete-standalone` |
| Job-owned pods | blocked | `--force` |

PDB-blocked eviction retries with exponential backoff until
`--eviction-timeout` expires.

## Checkpoints And Resume

Progress is written after each successful non-dry-run workload restart.
Default checkpoint path:

```text
~/.kube/safed-checkpoints/<context>-<node>.json
```

On success, the checkpoint is deleted. On failure or interruption, it remains
for a later resume:

```bash
kubectl safed drain worker-1 --resume
```

Checkpoint metadata includes the node name and kube context. Resume rejects a
checkpoint for a different node or context. In multi-node drains, omit
`--checkpoint-path`; each node uses its own default checkpoint. Custom
`--checkpoint-path` is only valid for a single-node drain.

Dry-runs do not write checkpoints.

## Profiles

Profiles live in `~/.kube/safed.yaml` by default. `--config` or
`KUBECTL_SAFED_CONFIG` can point to another file. CLI flags override profile
values.

```yaml
profiles:
  prod:
    preflight: strict
    rollout-timeout: 10m
    max-concurrency: 1
    uncordon-on-failure: true
    emit-events: true
  staging:
    preflight: warn
    rollout-timeout: 3m
    max-concurrency: 3
```

```bash
kubectl safed drain worker-1 --profile=prod
kubectl safed drain worker-1 --profile=prod --max-concurrency=2
```

## Logs And Events

Plain logs are aligned for terminal use:

```text
[safed] 15:04:05  worker-1                          info    Validating "worker-1"
[safed] 15:04:05  Deployment/default/api            start   Rolling restart [1/3]
[safed] 15:04:11  Deployment/default/api            poll    rollout updated=2/3 ready=1/3 available=1/3 unavail=1
[safed] 15:04:16  Deployment/default/api            done    Complete (10s)
```

JSON logs are one object per line:

```json
{"ts":"2026-03-15T15:04:05Z","level":"start","subject":"Deployment/default/api","msg":"Rolling restart [1/3]"}
```

Use `--emit-events` to create Kubernetes Events on the node and workload
objects. This requires `events/create`.

```bash
kubectl describe node worker-1
kubectl get events -A --sort-by=.lastTimestamp
```

## Failure Behavior

`kubectl-safed` fails fast on rollout states that are unlikely to recover
without a spec change:

- Deployment `ProgressDeadlineExceeded`
- `CrashLoopBackOff` after a container has terminated at least once
- `ImagePullBackOff`
- `ErrImagePull`

With `--uncordon-on-failure`, the node is uncordoned only if the current drain
performed the cordon. A node that was already cordoned before the drain is left
unchanged.

## Development

Common checks:

```bash
make check
make test-v
make e2e
make e2e-run TEST=TestDrain_NATS
```

The e2e suite creates a k3d cluster, installs NATS, Grafana, and
kube-state-metrics, then runs the compiled binary against real Kubernetes
objects.
