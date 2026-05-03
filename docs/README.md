# kubectl-safed Documentation

`kubectl-safed` is a `kubectl` plugin for draining Kubernetes nodes by
cordoning the node, rolling-restarting managed workloads, waiting for
replacement pods to become healthy elsewhere, and then handling any remaining
pods according to explicit operator choices.

Start here:

- [User guide](user-guide.md): complete behavior, installation, command
  reference, RBAC, profiles, logging, checkpoints, and edge cases.
- [Examples](examples/README.md): real-world drain commands and reusable
  profile/RBAC examples.
- [Project README](../README.md): short project overview, release, and
  contribution notes.

## Core Model

`kubectl-safed drain` is intentionally different from `kubectl drain`.
Instead of evicting every pod immediately, it first finds Deployments and
StatefulSets with pods on the target node and triggers Kubernetes-native rolling
restarts for those workloads. This lets controllers create replacement pods on
healthy nodes before terminating old pods.

The normal sequence is:

1. Validate the target node exists.
2. Discover managed workloads with active pods on the node.
3. Apply `--skip-workload` or `--only-workload`.
4. Run pre-flight checks unless disabled.
5. Cordon the node.
6. Rolling-restart Deployments and StatefulSets.
7. Wait for rollout convergence.
8. Verify each workload has no active pods left on the node.
9. Evict or skip remaining unmanaged pods according to flags.
10. Save checkpoint progress during the drain and remove the checkpoint on
    success.

## When To Use It

Use `kubectl-safed` when you want node maintenance or scale-down behavior that
respects workload rollout semantics and gives clear progress output. It is a
good fit for planned maintenance, node pool replacement, spot/interruption
preparation, and production drains where direct eviction is too abrupt.

It cannot make unsafe workload designs safe. Single-replica workloads,
Deployments using `Recreate`, and applications that cannot tolerate their own
rolling restart can still see downtime. Use `--preflight=strict` for production
paths where these risks should abort the drain before cordoning.
