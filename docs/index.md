---
layout: default
title: Home
nav_order: 1
---

# kubectl-safed

`kubectl-safed` is a `kubectl` plugin for safer Kubernetes node drains. It
cordons target nodes, rolling-restarts managed workloads, waits for replacement
pods to become healthy elsewhere, and then handles remaining pods only through
explicit operator choices.

**[View on GitHub](https://github.com/pbsladek/k8s-safed)** |
**[Latest release](https://github.com/pbsladek/k8s-safed/releases/latest)**

---

## Documentation

- [User guide](user-guide.html): installation, RBAC, command reference,
  pre-flight checks, checkpoints, profiles, logs, and failure behavior.
- [Examples](examples/): real-world command patterns and reusable RBAC/profile
  manifests.
- [Project README](https://github.com/pbsladek/k8s-safed#readme): short
  project overview, release, and contribution notes.

## Core Model

`kubectl-safed drain` is intentionally different from `kubectl drain`.
Instead of evicting every pod immediately, it first finds Deployments and
StatefulSets with pods on the target node and triggers Kubernetes-native
rolling restarts for those workloads. This gives controllers a chance to create
replacement pods on healthy nodes before old pods are terminated.

The normal sequence is:

1. Validate the target node exists.
2. Discover managed workloads with active pods on the node.
3. Apply workload filters.
4. Run pre-flight checks unless disabled.
5. Cordon the node.
6. Rolling-restart Deployments and StatefulSets.
7. Wait for rollout convergence.
8. Verify each workload has no active pods left on the node.
9. Evict or skip remaining unmanaged pods according to flags.
10. Save checkpoint progress during the drain and remove the checkpoint on
    success.

## Common Commands

```bash
kubectl safed drain worker-1 --dry-run
kubectl safed drain worker-1 --preflight=strict --uncordon-on-failure
kubectl safed drain --selector=nodepool=spot --node-concurrency=2
kubectl safed drain worker-1 --resume
```

Start with the [user guide](user-guide.html) for the complete command surface.
