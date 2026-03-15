# CLAUDE.md — k8s-safed

## What this project is

`kubectl-safed` is a kubectl krew plugin for zero-downtime Kubernetes node
draining. Rather than evicting pods (which can violate PodDisruptionBudgets),
it cordons the node and triggers rolling restarts on every Deployment and
StatefulSet that has pods there. The Kubernetes scheduler naturally places new
pods on healthy nodes first.

## Build & run

```bash
make check          # fmt + vet + test (default CI gate)
make build          # compile to ./kubectl-safed
make install        # install to $GOPATH/bin
go run . drain --help
```

## Key decisions

- **`Patch` not `Update`** for cordon and rolling restarts — avoids
  `resourceVersion` conflicts with concurrent controllers.
- **`restartedAt` annotation** on the pod template triggers rolling restarts,
  identical to `kubectl rollout restart`.
- **Post-patch generation gate** — `restartDeployment` / `restartStatefulSet`
  return the `Generation` from the PATCH response (not a pre-patch snapshot).
  The rollout wait uses `ObservedGeneration >= targetGeneration` so a
  concurrent `kubectl rollout restart` between our GET and PATCH cannot cause
  false-completion detection.
- **`waitForPodsOffNode`** — after each rollout completes cluster-wide, we
  additionally verify that the specific workload's pods have left the draining
  node. Terminating pods (DeletionTimestamp set) are excluded from the count;
  kubelet handles their cleanup.
- **Separate `PodVacateTimeout`** — pod departure is bounded by
  `terminationGracePeriodSeconds`, not rollout convergence time. The two waits
  have independent, separately-tunable timeouts.
- **`evictWithPDBRetry`** — eviction retries with exponential backoff when
  blocked by a PDB (HTTP 429 / 503). Bounded by `EvictionTimeout` so a
  misconfigured PDB never hangs the drain indefinitely.
- **RS cache in Finder** — `pkg/workload/workload.go` caches ReplicaSet and
  workload lookups to avoid N API calls when N pods share one RS.
- **`policyv1.Eviction` via `EvictV1()`** — uses `policy/v1` (not the
  deprecated `v1beta1`) for evictions.
- **Single timeout context** — rollout wait functions pass the caller's `ctx`
  directly to `PollUntilContextTimeout` with `RolloutTimeout` as the per-call
  budget. No manual `context.WithTimeout` wrapper to avoid double-deadline
  skew. The global `--timeout` is applied once at the top of `Run`.

## Module & dependencies

```
module github.com/pbsladek/k8s-safed
go 1.26
k8s.io/api v0.35.2
k8s.io/apimachinery v0.35.2
k8s.io/client-go v0.35.2
k8s.io/cli-runtime v0.35.2
github.com/spf13/cobra v1.10.2
golang.org/x/sync v0.20.0
```

## Project layout

```
main.go                         entry point → cmd.Execute()
cmd/
  root.go                       root cobra command + kubeConfigFlags
  drain.go                      `kubectl safed drain NODE` subcommand + flags
pkg/
  k8s/client.go                 builds kubernetes.Interface from ConfigFlags
  workload/workload.go          Finder: single-pass pod→RS→Deployment resolver
                                  exports IsTerminalPod used by drain pkg
  drain/
    drain.go                    Drainer: full drain orchestration
    printer.go                  structured stdout/JSON output (LogFormat)
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

1. Apply `--timeout` deadline to the context (if set)
2. `GET` node — validate it exists, log kernel version + ready status
3. `PATCH` node `spec.unschedulable=true` — cordon (idempotent)
4. `LIST` pods on node → resolve owners via `workload.Finder.FindForNode`
5. For each workload (sequential, batch, or fully parallel per `--max-concurrency`):
   a. `PATCH` pod template annotation `kubectl.kubernetes.io/restartedAt`
   b. Capture `targetGeneration` from PATCH response
   c. Poll deployment/STS status until `ObservedGeneration >= targetGeneration`
      AND all replicas updated + ready
   d. Fail fast on: `ProgressDeadlineExceeded` (Deployment), pod in
      `CrashLoopBackOff` (after first exit), `ImagePullBackOff`, `ErrImagePull`
   e. Poll pods on node matching workload's label selector until none are
      active (excluding terminating + terminal) — bounded by `--pod-vacate-timeout`
6. `LIST` remaining pods → evict eligible ones with PDB-aware retry

## Rollout completion conditions

**Deployment** — all true:
- `ObservedGeneration >= targetGeneration`
- `UpdatedReplicas == *Spec.Replicas`
- `ReadyReplicas == *Spec.Replicas`
- `AvailableReplicas == *Spec.Replicas`
- `UnavailableReplicas == 0`
- No `Progressing` condition with `Status=False, Reason=ProgressDeadlineExceeded`
- No pod matching the workload selector in `CrashLoopBackOff` / `ImagePullBackOff` / `ErrImagePull`

**StatefulSet** — all true:
- `ObservedGeneration >= targetGeneration`
- `UpdateRevision != ""` (guard against empty-string match before controller runs)
- `UpdateRevision == CurrentRevision`
- `UpdatedReplicas == *Spec.Replicas`
- `CurrentReplicas == *Spec.Replicas` (controller has reconciled CurrentRevision)
- `ReadyReplicas == *Spec.Replicas`
- No pod in `CrashLoopBackOff` / `ImagePullBackOff` / `ErrImagePull` (primary fail-fast; no ProgressDeadlineExceeded equivalent)

## Concurrency modes (`--max-concurrency`)

| Value | Behaviour |
|---|---|
| `1` (default) | Sequential — one workload fully completes before the next starts |
| `N > 1` | Batches of N — workloads in a batch run concurrently via `errgroup`; first error cancels siblings |
| `0` | All workloads concurrently — equivalent to N = len(workloads) |

## Logging (`--log-format`)

| Format | Description |
|---|---|
| `plain` (default) | `[safed] HH:MM:SS  Subject(32char)  level   message` — aligned columns, grep-friendly |
| `json` | One JSON object per line: `{"ts":"RFC3339","level":"...","subject":"...","msg":"..."}` |

Level labels: `info`, `start`, `done`, `poll`, `dryrun`
Subjects: `node` for drain-level events; `Kind/namespace/name` for workloads; `Pod/namespace/name` for eviction.

Grep patterns:
```bash
grep "start"                   # all workloads that began restarting
grep "done"                    # all completions
grep "poll"                    # progress detail only
grep "Deployment/default/api"  # every line for one workload
grep "dryrun"                  # dry-run preview lines
```

## Tunable timeouts

| Option field | Flag | Default | Scope |
|---|---|---|---|
| `Timeout` | `--timeout` | `0` (none) | Entire drain |
| `RolloutTimeout` | `--rollout-timeout` | `5m` | Per-workload rollout convergence |
| `PodVacateTimeout` | `--pod-vacate-timeout` | `2m` | Per-workload pod departure from node |
| `EvictionTimeout` | `--eviction-timeout` | `5m` | Per-pod PDB-blocked eviction |
| `PDBRetryInterval` | `--pdb-retry-interval` | `5s` | Base backoff for PDB retries (doubles, capped 60s) |
| `PollInterval` | `--poll-interval` | `5s` | Interval between all status checks |

## Edge case handling

- **PDB-blocked eviction** — `evictWithPDBRetry` retries on `429`/`503` with
  exponential backoff, bounded by `EvictionTimeout`. Logs each blocked attempt.
- **CrashLoopBackOff** — detected via `LastTerminationState.Terminated != nil`
  (not `RestartCount > 0`, which misses the first crash before count increments).
- **ImagePullBackOff / ErrImagePull** — fail-fast immediately; image errors
  won't self-heal without a spec change.
- **Standalone pods** — require `--force` to evict. With `--force-delete-standalone`,
  they are force-deleted (`gracePeriodSeconds=0`) bypassing the eviction API entirely.
- **StatefulSet no ProgressDeadlineExceeded** — compensated by active pod-state
  polling on every tick.
- **Concurrent `kubectl rollout restart`** — using post-patch generation means
  a concurrent restart between our GET and PATCH cannot cause false completion.

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
- Printer methods (`out.Infof`, `out.Pollf`, `out.DryRun`, etc.) for all
  user-facing output — never `fmt.Println` directly in business logic.
- All wait loops use `wait.PollUntilContextTimeout` with `immediate=true` so
  fast rollouts don't pay a full poll-interval penalty.
- Scripts live in `scripts/` and are called by workflows with `run:` — no
  inline shell or Python in workflow YAML.
