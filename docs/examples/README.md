---
layout: default
title: Examples
nav_order: 3
---

# kubectl-safed Examples

These examples are written as real operational patterns. Replace node names,
selectors, namespaces, and workload names with values from your cluster.

Before running a real drain, start with `--dry-run` and inspect the discovered
workloads:

```bash
kubectl safed drain worker-1 --dry-run
```

## 1. Planned Maintenance For One Production Node

Use strict pre-flight checks, a hard overall deadline, events for auditability,
and automatic uncordon on failure.

```bash
kubectl safed drain ip-10-0-12-34.ec2.internal \
  --preflight=strict \
  --rollout-timeout=10m \
  --pod-vacate-timeout=3m \
  --timeout=45m \
  --emit-events \
  --uncordon-on-failure
```

Use this when a single node needs kernel patching, disk replacement, or
container runtime maintenance.

## 2. Drain A Spot Node Pool In Batches

Drain all nodes with a label selector, two nodes at a time, while preserving
sequential workload rollout inside each node.

```bash
kubectl safed drain \
  --selector=nodepool=spot \
  --node-concurrency=2 \
  --preflight=warn \
  --rollout-timeout=8m \
  --timeout=30m
```

This pattern is useful for controlled spot replacement or node pool rotation.

## 3. Preview A Scale-Down Across A Node Group

Dry-run all nodes in a pool to see what would be restarted or evicted without
cordoning anything.

```bash
kubectl safed drain \
  --selector=karpenter.sh/nodepool=batch \
  --dry-run \
  --preflight=warn
```

Review pre-flight warnings and skipped/remaining pod output before allowing the
autoscaler or operator runbook to proceed.

## 4. Drain Only API Workloads On A Shared Node

Restrict rolling restarts to a known set of workloads and leave everything else
untouched by the managed restart phase.

```bash
kubectl safed drain worker-7 \
  --only-workload=Deployment/prod/api \
  --only-workload=Deployment/prod/api-worker \
  --only-workload=StatefulSet/prod/api-cache \
  --preflight=strict \
  --rollout-timeout=10m
```

Use this when a node hosts multiple application groups and a maintenance window
only covers one group.

## 5. Skip A Long-Running Batch Processor

Drain the node but do not rolling-restart a specific workload.

```bash
kubectl safed drain worker-3 \
  --skip-workload=Deployment/jobs/nightly-importer \
  --preflight=warn \
  --rollout-timeout=10m
```

The skipped workload is not patched for rolling restart. Any pods still left on
the node are still considered during the remaining-pod phase.

## 6. Resume After Laptop Disconnect Or Ctrl-C

First run:

```bash
kubectl safed drain worker-2 \
  --preflight=strict \
  --rollout-timeout=10m \
  --timeout=45m
```

If the process is interrupted, resume later:

```bash
kubectl safed drain worker-2 \
  --resume \
  --preflight=strict \
  --rollout-timeout=10m \
  --timeout=45m
```

Completed workloads are skipped from the checkpoint file. The checkpoint is
removed automatically once the resumed drain succeeds.

## 7. Use A Custom Checkpoint Path In Automation

Custom checkpoint paths are for single-node drains.

```bash
kubectl safed drain worker-9 \
  --checkpoint-path=/var/tmp/safed-worker-9.json \
  --preflight=strict \
  --rollout-timeout=10m

kubectl safed drain worker-9 \
  --resume \
  --checkpoint-path=/var/tmp/safed-worker-9.json \
  --preflight=strict \
  --rollout-timeout=10m
```

For multi-node drains, omit `--checkpoint-path` so each node uses its own
context-and-node-specific checkpoint file.

## 8. Evict Standalone And Job-Owned Pods Explicitly

Standalone pods and Job-owned pods are blocked by default because they are not
managed by rolling restarts. Use `--force` when the runbook explicitly allows
evicting them.

```bash
kubectl safed drain worker-4 \
  --force \
  --eviction-timeout=2m \
  --pdb-retry-interval=5s \
  --preflight=warn
```

Use this for batch or utility nodes where remaining unmanaged pods are expected
and safe to recreate.

## 9. Drain Pods That Use emptyDir

Pods using `emptyDir` are blocked by default because local data is lost on
eviction. Opt in only when the data is scratch/cache data.

```bash
kubectl safed drain worker-5 \
  --delete-emptydir-data \
  --preflight=strict \
  --rollout-timeout=10m
```

Do not use this for workloads that keep non-rebuildable state in `emptyDir`.

## 10. Produce Machine-Readable Logs For Incident Review

Send JSON logs to a file and emit Kubernetes Events.

```bash
kubectl safed drain worker-8 \
  --log-format=json \
  --emit-events \
  --preflight=strict \
  --rollout-timeout=10m \
  > safed-worker-8.ndjson

kubectl get events -A --sort-by=.lastTimestamp
```

This is useful for CI/CD jobs, incident timelines, or later import into log
aggregation systems.

## 11. Use Profiles For Repeatable Environments

Create `~/.kube/safed.yaml`:

```yaml
profiles:
  prod:
    preflight: strict
    rollout-timeout: 10m
    pod-vacate-timeout: 3m
    timeout: 45m
    max-concurrency: 1
    uncordon-on-failure: true
    emit-events: true
  staging:
    preflight: warn
    rollout-timeout: 5m
    max-concurrency: 3
    uncordon-on-failure: true
  spot-scale-down:
    preflight: off
    rollout-timeout: 6m
    node-concurrency: 5
    delete-emptydir-data: true
```

Then run:

```bash
kubectl safed drain worker-1 --profile=prod
kubectl safed drain --selector=nodepool=spot --profile=spot-scale-down
kubectl safed drain worker-1 --profile=prod --max-concurrency=2
```

CLI flags override profile values.

## 12. Drain DaemonSet Pods As Part Of Node Shutdown

DaemonSet pods are skipped by default. If your shutdown process requires
evicting them too, opt in:

```bash
kubectl safed drain worker-6 \
  --ignore-daemonsets=false \
  --force \
  --preflight=warn \
  --eviction-timeout=2m
```

Use this carefully; many DaemonSets are node agents that are expected to remain
until the node is terminated.

## Reusable Files

- [rbac.yaml](rbac.yaml): ClusterRole example for the tool.
- [profiles.yaml](profiles.yaml): profile examples for production, staging,
  and spot/node-pool workflows.
