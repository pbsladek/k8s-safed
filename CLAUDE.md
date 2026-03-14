# CLAUDE.md — k8s-safed

## What this project is

`kubectl-safed` is a kubectl krew plugin for zero-downtime Kubernetes node
draining. Rather than evicting pods (which can violate PodDisruptionBudgets),
it cordons the node and triggers rolling restarts on every Deployment and
StatefulSet that has pods there. The Kubernetes scheduler naturally places new
pods on healthy nodes first.

## Build & run

```bash
go build ./...          # compile
go vet ./...            # static analysis
go test ./...           # run tests (currently minimal stubs)
go run . drain --help   # smoke test the CLI
```

## Key decisions

- **`Patch` not `Update`** for cordon and rolling restarts — avoids
  `resourceVersion` conflicts with concurrent controllers.
- **`restartedAt` annotation** on the pod template to trigger rolling restarts,
  identical to `kubectl rollout restart`.
- **`preGeneration` gate** — the rollout wait captures the workload's generation
  before patching and only starts checking status once
  `ObservedGeneration > preGeneration`, preventing false-positive completion
  detection on a pre-existing ready state.
- **`waitForPodsOffNode`** — after each rollout completes cluster-wide, we
  additionally verify pods for that specific workload have left the draining
  node before moving to the next workload. Terminating pods are excluded from
  the count (DeletionTimestamp set = already handled by kubelet).
- **RS cache in Finder** — `pkg/workload/workload.go` caches ReplicaSet and
  workload lookups to avoid N API calls when N pods on the node share one RS.
- **`policyv1beta1.Eviction`** — client-go v0.31.0 `Pods.Evict()` takes
  `*policyv1beta1.Eviction`, not `*policyv1.Eviction`. Don't change this.

## Module & dependencies

```
module github.com/pbsladek/k8s-safed
go 1.26
k8s.io/api v0.31.0
k8s.io/apimachinery v0.31.0
k8s.io/client-go v0.31.0
k8s.io/cli-runtime v0.31.0   # for genericclioptions / kubeconfig flags
github.com/spf13/cobra v1.8.1
```

## Project layout

```
main.go                         entry point → cmd.Execute()
cmd/
  root.go                       root cobra command + kubeConfigFlags
  drain.go                      `kubectl safed drain NODE` subcommand
pkg/
  k8s/client.go                 builds kubernetes.Interface from ConfigFlags
  workload/workload.go          Finder: single-pass pod→RS→Deployment resolver
                                  exports IsTerminalPod used by drain pkg
  drain/
    drain.go                    Drainer: the full drain orchestration
    printer.go                  structured stdout output helpers
scripts/
  update-krew-manifest.py       rewrites plugin.yaml with SHA256s from dist/checksums.txt
  commit-manifest.sh            git add/commit/push plugin.yaml to main [skip ci]
  submit-to-krew-index.sh       opens a PR against kubernetes-sigs/krew-index
.github/workflows/
  ci.yml                        build + vet + test + goreleaser check on push/PR
  release.yml                   goreleaser → update manifest → upload → commit
.goreleaser.yaml                multi-arch build (linux/darwin/windows × amd64/arm64)
plugin.yaml                     krew manifest (auto-updated by release workflow)
```

## Drain sequence (Drainer.Run)

1. `GET` node — validate it exists, log kernel version + ready status
2. `PATCH` node `spec.unschedulable=true` — cordon (idempotent)
3. `LIST` pods on node → resolve owners via `workload.Finder.FindForNode`
4. For each workload:
   a. Capture `Generation` (preGen)
   b. `PATCH` pod template annotation `kubectl.kubernetes.io/restartedAt`
   c. Poll deployment/STS status until `ObservedGeneration > preGen` AND
      all replicas updated + ready (fail fast on `ProgressDeadlineExceeded`)
   d. Poll pods on node matching workload's label selector until none are
      active (excluding terminating + terminal)
5. `LIST` remaining pods → evict eligible ones

## Rollout completion conditions

**Deployment** — all true:
- `ObservedGeneration >= Generation`
- `UpdatedReplicas == *Spec.Replicas`
- `ReadyReplicas == *Spec.Replicas`
- `AvailableReplicas == *Spec.Replicas`
- `UnavailableReplicas == 0`
- No `Progressing` condition with `Status=False, Reason=ProgressDeadlineExceeded`

**StatefulSet** — all true:
- `ObservedGeneration > preGen` (int64, not a pointer in v0.31.0)
- `UpdateRevision != ""` (guard against empty-string match before controller runs)
- `UpdateRevision == CurrentRevision`
- `UpdatedReplicas == *Spec.Replicas`
- `ReadyReplicas == *Spec.Replicas`

## Release process

```bash
git tag v0.x.0
git push origin v0.x.0
```

This triggers `.github/workflows/release.yml` which:
1. Runs GoReleaser → builds 5 platform binaries, creates GitHub release
2. Runs `scripts/update-krew-manifest.py` → rewrites `plugin.yaml`
3. Uploads `plugin.yaml` to the GitHub release
4. Runs `scripts/commit-manifest.sh` → commits updated `plugin.yaml` to `main`

Krew-index submission is handled by `scripts/submit-to-krew-index.sh` in a
separate (currently commented-out) workflow job. See README for setup.

## Coding conventions

- No `Update` calls on resources that may be concurrently modified — always
  use `Patch` with `StrategicMergePatchType`.
- `Spec.Replicas` is `*int32` for both Deployment and StatefulSet; always
  nil-check and default to `1`.
- `workload.IsTerminalPod` is the single source of truth for "does this pod
  need action?" — use it in both `workload` and `drain` packages.
- Printer methods (`out.Infof`, `out.Step`, `out.DryRun`) for all user-facing
  output; never `fmt.Println` directly in business logic.
- Scripts live in `scripts/` and are called by workflows with `run:` — no
  inline shell or Python in workflow YAML.
