# kubectl-safed

A `kubectl` krew plugin that drains Kubernetes nodes **without causing downtime**.

Instead of evicting pods directly — which can violate PodDisruptionBudgets and
interrupt traffic — `kubectl-safed` cordons the node and triggers rolling
restarts on every Deployment and StatefulSet that has pods there. The Kubernetes
scheduler places replacement pods on healthy nodes before the old ones are
terminated, giving you a zero-downtime drain.

---

## How it works

```
1. Validate   – confirm the target node exists
2. Discover   – find every Deployment and StatefulSet with pods on the node
                (Pod → ReplicaSet → Deployment ownership is fully resolved)
3. Filter     – apply --skip-workload / --only-workload before any cluster changes
4. Pre-flight – scan discovered workloads for downtime risks: single-replica
                Deployments, Recreate strategy, stateful services, PDBs with 0
                disruptions allowed. Use --preflight=strict to abort on any risk,
                --preflight=off to skip entirely.
5. Cordon     – mark the node unschedulable so no new pods are scheduled there
6. Restart    – patch each workload's pod template with a restartedAt annotation
                (identical to `kubectl rollout restart`)
7. Wait       – poll rollout status until all replicas are updated and ready;
                fail fast on ProgressDeadlineExceeded, CrashLoopBackOff,
                ImagePullBackOff, or ErrImagePull
8. Verify     – confirm all of that workload's pods have left the node before
                moving to the next one
9. Evict      – evict any remaining pods (DaemonSets, Jobs, standalones)
                controlled by --ignore-daemonsets, --force, --delete-emptydir-data
```

Workloads are processed **sequentially** by default. Use `--max-concurrency`
to run rolling restarts in batches or all at once.

Multiple nodes can be drained in sequence or in parallel. Use `--selector` to
target nodes by label, and `--node-concurrency` to drain several nodes at once.

---

## Differences from `kubectl drain`

| | `kubectl drain` | `kubectl-safed drain` |
|---|---|---|
| Mechanism | Pod eviction | Rolling restart → natural pod migration |
| Risk of downtime | High if PDB is misconfigured | Low — scheduler places new pods first |
| Respects RollingUpdate strategy | No | Yes |
| PDB awareness | Via eviction API | Via rollout controller |
| Verifies pods left the node | No | Yes, per workload |
| Progress visibility | Minimal | Aligned log columns, JSON output option |
| PDB-blocked eviction | Fails immediately | Retries with exponential backoff |
| CrashLoop / ImagePull detection | No | Fail-fast on first detected bad state |

---

## Requirements

### Kubernetes RBAC

The service account or user running `kubectl-safed` needs the following
permissions:

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

> `pods/delete` is only needed when using `--force-delete-standalone`.
> `poddisruptionbudgets/list` is only needed when `--preflight` is not `off`.
>
> When using `--emit-events`, also add:
> ```yaml
>   - apiGroups: [""]
>     resources: ["events"]
>     verbs: ["create"]
> ```

### kubectl

- kubectl 1.24+
- Kubernetes cluster 1.22+

---

## Installation

### Via krew (recommended)

Install [krew](https://krew.sigs.k8s.io/docs/user-guide/setup/install/) first,
then:

```bash
kubectl krew install --manifest-url \
  https://github.com/pbsladek/k8s-safed/releases/latest/download/plugin.yaml
```

Once the plugin is published to the official krew index:

```bash
kubectl krew install safed
```

### Manual installation

Download the binary for your platform from the
[releases page](https://github.com/pbsladek/k8s-safed/releases/latest), then
place it somewhere on your `PATH` as `kubectl-safed`:

```bash
# macOS ARM64 example
VERSION=$(curl -s https://api.github.com/repos/pbsladek/k8s-safed/releases/latest \
  | grep '"tag_name"' | cut -d'"' -f4)
curl -Lo kubectl-safed.tar.gz \
  "https://github.com/pbsladek/k8s-safed/releases/download/${VERSION}/kubectl-safed_darwin_arm64.tar.gz"
tar -xzf kubectl-safed.tar.gz
chmod +x kubectl-safed_darwin_arm64/kubectl-safed
sudo mv kubectl-safed_darwin_arm64/kubectl-safed /usr/local/bin/kubectl-safed

# Verify
kubectl safed --help
```

Available platforms:

| OS | Architecture | Archive |
|---|---|---|
| Linux | amd64 | `kubectl-safed_linux_amd64.tar.gz` |
| Linux | arm64 | `kubectl-safed_linux_arm64.tar.gz` |
| macOS | amd64 | `kubectl-safed_darwin_amd64.tar.gz` |
| macOS | arm64 (Apple Silicon) | `kubectl-safed_darwin_arm64.tar.gz` |
| Windows | amd64 | `kubectl-safed_windows_amd64.zip` |

### Build from source

```bash
git clone https://github.com/pbsladek/k8s-safed.git
cd k8s-safed
make build
sudo mv kubectl-safed /usr/local/bin/
```

---

## Usage

```
kubectl safed drain NODE [NODE...] [--selector SELECTOR] [flags]
```

### Examples

```bash
# Preview what would happen — no changes made
kubectl safed drain worker-1 -d

# Drain with default settings (DaemonSets skipped, pre-flight checks on)
kubectl safed drain worker-1

# Drain multiple nodes sequentially
kubectl safed drain worker-1 worker-2 worker-3

# Drain all nodes in a node pool (resolved via label selector)
kubectl safed drain --selector node-pool=spot

# Drain two nodes in parallel
kubectl safed drain worker-1 worker-2 --node-concurrency=2

# Abort if any downtime risk is detected during pre-flight
kubectl safed drain worker-1 --preflight=strict

# Skip pre-flight checks entirely
kubectl safed drain worker-1 --preflight=off

# Drain, waiting up to 10 minutes per workload rollout
kubectl safed drain worker-1 --rollout-timeout=10m

# Drain with a hard overall deadline
kubectl safed drain worker-1 -t 30m

# Restart 3 workloads at a time instead of sequentially
kubectl safed drain worker-1 --max-concurrency=3

# Restart all workloads at the same time
kubectl safed drain worker-1 --max-concurrency=0

# Drain a node and also evict DaemonSet pods
kubectl safed drain worker-1 --ignore-daemonsets=false

# Drain and evict pods that use emptyDir volumes (data will be lost)
kubectl safed drain worker-1 --delete-emptydir-data

# Drain and evict standalone and Job-owned pods
kubectl safed drain worker-1 -f

# Instantly remove standalone pods (no graceful shutdown)
kubectl safed drain worker-1 --force-delete-standalone

# Output structured JSON logs for ingestion by Loki / Datadog / ELK
kubectl safed drain worker-1 -o json

# Uncordon the node automatically if the drain fails
kubectl safed drain worker-1 --uncordon-on-failure

# Drain all workloads except one specific Deployment
kubectl safed drain worker-1 --skip-workload=Deployment/default/batch-processor

# Drain only specific workloads, leave the rest untouched
kubectl safed drain worker-1 --only-workload=Deployment/default/api --only-workload=StatefulSet/data/postgres

# Resume a previously interrupted drain (skips already-completed workloads)
kubectl safed drain worker-1 --resume

# Use a named drain profile from ~/.kube/safed.yaml
kubectl safed drain worker-1 --profile=prod

# Emit Kubernetes Events for audit trail (visible via kubectl describe node)
kubectl safed drain worker-1 --emit-events

# Use a specific kubeconfig context
kubectl safed drain worker-1 --context=prod-cluster
```

### Flags

#### Node targeting

| Flag | Short | Default | Description |
|---|---|---|---|
| `--selector` | `-l` | | Label selector to target nodes (e.g. `node-pool=spot`). Mutually exclusive with positional node names. |
| `--node-concurrency` | | `1` | Number of nodes to drain in parallel (`1` = sequential). Use with care on production clusters. |

#### Core behaviour

| Flag | Short | Default | Description |
|---|---|---|---|
| `--dry-run` | `-d` | `false` | Preview all actions without making any changes |
| `--preflight` | | `warn` | Pre-flight mode: `warn` (log risks, continue), `strict` (abort on any risk), `off` (skip all checks) |
| `--force` | `-f` | `false` | Evict standalone pods (no owner) and Job-owned pods |
| `--force-delete-standalone` | | `false` | Force-delete standalone pods with `gracePeriodSeconds=0` instead of evicting; implies `--force` |
| `--ignore-daemonsets` | | `true` | Skip eviction of DaemonSet-managed pods |
| `--delete-emptydir-data` | | `false` | Allow eviction of pods using emptyDir volumes (data loss) |
| `--grace-period` | | `-1` | Pod termination grace period in seconds; `-1` uses the pod's own default |
| `--max-concurrency` | | `1` | `1` = sequential, `0` = all at once, `N` = batches of N (per node, for workloads) |
| `--uncordon-on-failure` | | `false` | Uncordon the node if the drain fails (only if this run cordoned it) |
| `--skip-workload` | | | Exclude a workload from rolling restarts (`Kind/namespace/name`). Repeatable. Mutually exclusive with `--only-workload`. |
| `--only-workload` | | | Restrict rolling restarts to these workloads only (`Kind/namespace/name`). Repeatable. Mutually exclusive with `--skip-workload`. |
| `--emit-events` | | `false` | Emit Kubernetes Events to node and workload objects (requires `events/create` RBAC; visible via `kubectl describe`) |
| `--resume` | | `false` | Resume an interrupted drain, skipping workloads already recorded in the checkpoint file |
| `--checkpoint-path` | | | Override the checkpoint file path (default: `~/.kube/safed-checkpoints/<context>-<node>.json`) |
| `--profile` | | | Load flag defaults from a named profile in `~/.kube/safed.yaml` (see [Drain profiles](#drain-profiles)) |
| `--config` | | | Path to the safed config file (default: `~/.kube/safed.yaml`; env: `KUBECTL_SAFED_CONFIG`) |

#### Timeouts

| Flag | Short | Default | Description |
|---|---|---|---|
| `--timeout` | `-t` | `0` | Overall drain deadline per node; `0` = no limit |
| `--rollout-timeout` | | `5m` | Per-workload time to wait for rolling restart to converge; `0` = no per-workload limit |
| `--pod-vacate-timeout` | | `2m` | Per-workload time to wait for pods to leave the node after rollout |
| `--eviction-timeout` | | `5m` | Per-pod time to wait for a PDB-blocked eviction to succeed |
| `--pdb-retry-interval` | | `5s` | Base retry interval when eviction is blocked by a PDB (doubles each attempt, capped at 60s) |
| `--poll-interval` | | `5s` | Interval between status checks in all wait loops |

#### Output

| Flag | Short | Default | Description |
|---|---|---|---|
| `--log-format` | `-o` | `plain` | `plain` (human-readable, aligned columns) or `json` (one object per line) |

### Global flags (standard kubectl flags)

```
--kubeconfig, --context, --namespace, --server, --token,
--certificate-authority, --as, --as-group, ...
```

---

## Pre-flight checks

Before making any cluster changes (before the cordon), `kubectl-safed` scans
the discovered workloads and surfaces potential issues:

| Check | Risk level | Condition |
|---|---|---|
| Single-replica Deployment | **risk** | `spec.replicas == 1` — rolling restart briefly has 0 ready pods |
| Recreate strategy | **risk** | `strategy.type == Recreate` — all pods are terminated before new ones start |
| Single-replica StatefulSet | **risk** | `spec.replicas == 1` — rolling restart causes downtime |
| Multi-replica StatefulSet | note | Pods restart one at a time in reverse ordinal order |
| Known stateful service | note | Name matches a known stateful pattern (see below) |
| PDB with 0 disruptions | note | `status.disruptionsAllowed == 0` — eviction of remaining pods may be blocked |

**Risk-level** findings cause a non-zero exit under `--preflight=strict` and are
prefixed with `RISK:` in the log. Informational findings are prefixed with
`note:` and never abort the drain.

```
[safed] 15:04:05  worker-1                          info    Running pre-flight checks...
[safed] 15:04:05  Deployment/default/api            warn    RISK: single replica — rolling restart will briefly have 0 ready pods (downtime risk)
[safed] 15:04:05  StatefulSet/data/postgres         warn    note: detected known stateful service ("postgres") — verify replication health and data consistency before draining
[safed] 15:04:05  PodDisruptionBudget/data/my-pdb   warn    note: 0 disruptions currently allowed — eviction of remaining pods may be blocked until the PDB permits it
```

Stateful service detection matches these patterns (case-insensitive substring):

`postgres`, `postgresql`, `pgbouncer`, `pgpool`, `pgpool2`, `patroni`,
`mysql`, `mariadb`, `percona`, `vitess`,
`redis`, `keydb`,
`mongo`, `mongodb`,
`elasticsearch`, `opensearch`, `solr`,
`kafka`, `zookeeper`, `redpanda`,
`rabbitmq`, `nats`,
`etcd`, `cassandra`, `scylla`, `cockroach`, `clickhouse`, `yugabyte`,
`minio`, `vault`, `memcached`

Use `--preflight=strict` to abort the drain when any risk-level finding is
detected. Use `--preflight=off` to skip all checks.

---

## Log output

### Plain format (default)

```
[safed] 15:04:05  worker-1                          info    Validating "worker-1"
[safed] 15:04:05  worker-1                          info    Discovering managed workloads...
[safed] 15:04:05  worker-1                          info    Running pre-flight checks...
[safed] 15:04:05  Deployment/default/api            warn    RISK: single replica — rolling restart will briefly have 0 ready pods (downtime risk)
[safed] 15:04:05  worker-1                          info    Cordoning "worker-1"...
[safed] 15:04:05  worker-1                          done    Cordoned "worker-1"
[safed] 15:04:06  Deployment/default/api            start   Rolling restart [1/3]
[safed] 15:04:06  Deployment/default/api            poll    Waiting for rollout (targetGen=5)
[safed] 15:04:11  Deployment/default/api            poll    rollout updated=2/3 ready=1/3 available=1/3 unavail=1
[safed] 15:04:16  Deployment/default/api            poll    rollout updated=3/3 ready=3/3 available=3/3 unavail=0
[safed] 15:04:16  Deployment/default/api            done    Complete (10s)
```

Useful grep patterns:
```bash
grep "start"                   # all workloads that began restarting
grep "done"                    # all completions
grep "poll"                    # progress detail only
grep "warn"                    # all pre-flight findings
grep "RISK"                    # risk-level findings only
grep "Deployment/default/api"  # every line for one workload
grep "dryrun"                  # dry-run preview lines
grep "worker-1"                # all events for a specific node (useful in multi-node drains)
```

### JSON format (`--log-format=json`)

```json
{"ts":"2026-03-15T15:04:05Z","level":"info","subject":"worker-1","msg":"Validating \"worker-1\""}
{"ts":"2026-03-15T15:04:05Z","level":"warn","subject":"Deployment/default/api","msg":"RISK: single replica — rolling restart will briefly have 0 ready pods (downtime risk)"}
{"ts":"2026-03-15T15:04:06Z","level":"start","subject":"Deployment/default/api","msg":"Rolling restart [1/3]"}
{"ts":"2026-03-15T15:04:11Z","level":"poll","subject":"Deployment/default/api","msg":"rollout updated=2/3 ready=1/3 available=1/3 unavail=1"}
{"ts":"2026-03-15T15:04:16Z","level":"done","subject":"Deployment/default/api","msg":"Complete (10s)"}
```

Level values: `info`, `start`, `done`, `poll`, `dryrun`, `warn`

---

## What gets skipped

By default the following pods are **never** evicted or restarted:

| Pod type | Reason |
|---|---|
| Mirror pods (static pods) | Managed directly by kubelet; cannot be evicted via API |
| `Succeeded` / `Failed` pods | Already terminal; nothing to do |
| Already-terminating pods | `DeletionTimestamp` is set; kubelet is cleaning them up |
| DaemonSet pods | Run on every node by design (override with `--ignore-daemonsets=false`) |
| Pods with `emptyDir` | Eviction causes data loss (override with `--delete-emptydir-data`) |
| Standalone pods | No owner reference; require `--force` |
| Job-owned pods | Not rolling-restart managed; require `--force` |

---

## Edge case handling

### PodDisruptionBudget blocking eviction

When eviction of a remaining pod is blocked by a PDB (HTTP 429), the drain
retries with exponential backoff instead of failing immediately. Each blocked
attempt is logged:

```
[safed] 15:04:30  Pod/default/my-pod             poll    eviction blocked by PodDisruptionBudget (attempt 2), retrying in 10s
```

Retries continue until `--eviction-timeout` expires (default 5m). Tune with
`--pdb-retry-interval` (base backoff, doubles each attempt, cap 60s).

### CrashLoopBackOff / ImagePullBackOff during rollout

The rollout waiter actively inspects pod container states on every poll. If a
pod enters `CrashLoopBackOff` (after its first exit) or `ImagePullBackOff` /
`ErrImagePull`, the drain fails immediately with a clear error instead of
silently waiting for `--rollout-timeout` to expire.

This is especially important for StatefulSets, which have no
`ProgressDeadlineExceeded` equivalent.

### Standalone pods

With `--force`, standalone pods (no owner references) are evicted via the
eviction API. With `--force-delete-standalone`, they are immediately deleted
with `gracePeriodSeconds=0`, bypassing both PDB checks and graceful shutdown
hooks. Use this when you need them gone immediately and their shutdown behaviour
does not matter.

---

## Drain priority

By default workloads are restarted in discovery order. Add the
`kubectl.safed.io/drain-priority` annotation to control the sequence: lower
values restart first. Workloads without the annotation use a default priority
of `100`.

```yaml
# Restart the frontend first (priority 10), then the API (default 100),
# then the database last (priority 200).
apiVersion: apps/v1
kind: Deployment
metadata:
  name: frontend
  annotations:
    kubectl.safed.io/drain-priority: "10"
```

Within the same priority level, `--max-concurrency` still applies, so you can
run same-priority workloads concurrently while keeping critical tiers strictly
ordered.

---

## Workload filtering

`--skip-workload` and `--only-workload` accept `Kind/namespace/name` — the
same format used in the drain log output, so you can copy-paste directly.

```bash
# Skip one long-running batch job, drain everything else normally
kubectl safed drain worker-1 --skip-workload=Deployment/default/batch-processor

# Only restart these two workloads; leave everything else untouched
kubectl safed drain worker-1 \
  --only-workload=Deployment/default/api \
  --only-workload=StatefulSet/data/postgres
```

Skipped workloads are not rolling-restarted but still fall through to the
conventional eviction phase, so DaemonSet pods and standalones are handled
normally.

---

## Checkpoint & resume

When a drain is interrupted — by a timeout, a network blip, or Ctrl-C —
`--resume` picks up where the previous run left off:

```bash
# First attempt (interrupted after worker-1's api Deployment completed)
kubectl safed drain worker-1

# Resume: skips api, continues from the next workload
kubectl safed drain worker-1 --resume
```

Progress is written to `~/.kube/safed-checkpoints/<context>-<node>.json` after
each workload completes. The checkpoint is automatically deleted on a successful
drain. On failure it is left in place for the next `--resume` attempt.

For multi-node drains each node has its own checkpoint file, so resuming
applies independently per node.

Override the file path with `--checkpoint-path` if needed (e.g. for scripting
or when the home directory is not writable).

---

## Drain profiles

Save common flag combinations in `~/.kube/safed.yaml` and reference them with
`--profile`. This avoids retyping the same flags and reduces the chance of
operator error during maintenance windows.

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
  spot-scale-down:
    preflight: off
    ignore-daemonsets: false
    delete-emptydir-data: true
    node-concurrency: 5
```

```bash
kubectl safed drain worker-1 --profile=prod
```

CLI flags always override profile values, so you can use a profile as a
baseline and override specific settings for a one-off:

```bash
# Use prod profile but allow 3 concurrent workloads for this one drain
kubectl safed drain worker-1 --profile=prod --max-concurrency=3
```

The config file path can be overridden with `--config` or the
`KUBECTL_SAFED_CONFIG` environment variable.

---

## Development

### Unit tests

```bash
make check   # fmt + vet + unit tests (with race detector)
make test-v  # verbose unit tests
```

### End-to-end tests

E2E tests run the compiled `kubectl-safed` binary against a real multi-node
Kubernetes cluster created with [k3d](https://k3d.io). They require `k3d` and
`helm` in your `$PATH`.

```bash
# Run the full suite (creates a 3-node k3d cluster, installs NATS + Grafana via Helm)
make e2e

# Run a single test
make e2e-run TEST=TestDrain_NATS
```

The suite covers 18 scenarios including StatefulSet and Deployment rolling
restarts, multi-node drains, pre-flight checks, priority ordering, PDB-blocked
eviction, CrashLoopBackOff fail-fast, `--uncordon-on-failure`, workload
filtering, checkpoint/resume, and Kubernetes Event emission.

E2E tests also run automatically in CI on every push and pull request to `main`,
nightly at 03:00 UTC, and on `workflow_dispatch` via
`.github/workflows/e2e.yml`.

---

## Releasing a new version

Tag a commit and push — the release workflow handles the rest:

```bash
git tag v0.2.0
git push origin v0.2.0
```

The workflow will:
1. Build binaries for all platforms
2. Create a GitHub release with archives and `checksums.txt`
3. Generate and upload an updated `plugin.yaml` krew manifest
4. Commit the updated `plugin.yaml` back to `main`

---

## Publishing to the official krew index

Once the plugin is working and you want it discoverable via
`kubectl krew install safed`, follow these steps to enable automated
krew-index PRs on every release.

### One-time setup

**1. Fork krew-index**

Go to https://github.com/kubernetes-sigs/krew-index and click **Fork**.
Keep the default name (`krew-index`) under your account.

**2. Create a Personal Access Token**

Go to **GitHub → Settings → Developer settings → Personal access tokens**
and create a token (fine-grained or classic) with the **`repo`** scope
targeting your fork.

**3. Add the token as a repository secret**

In this repository go to **Settings → Secrets and variables → Actions → Secrets**
and add:

| Name | Value |
|---|---|
| `KREW_INDEX_PR_TOKEN` | The PAT you created above |

**4. Add your fork slug as a repository variable**

In **Settings → Secrets and variables → Actions → Variables** add:

| Name | Value |
|---|---|
| `KREW_INDEX_FORK` | `<your-github-username>/krew-index` |

**5. Uncomment the job in the release workflow**

In `.github/workflows/release.yml`, uncomment the `submit-to-krew-index`
job (the block starting with `# submit-to-krew-index:`).

### What happens on every release

The `submit-to-krew-index` job runs `scripts/submit-to-krew-index.sh` which:

1. Clones your krew-index fork and rebases it against upstream `master`
2. Copies the updated `plugin.yaml` to `plugins/safed.yaml` on a new branch
3. Pushes the branch to your fork
4. Opens a PR against `kubernetes-sigs/krew-index:master`

The krew maintainers review and merge the PR. After merge,
`kubectl krew install safed` works for everyone.

### Testing the script locally

```bash
KREW_GITHUB_TOKEN=<pat> \
KREW_INDEX_FORK=<your-username>/krew-index \
VERSION=v0.1.0 \
bash scripts/submit-to-krew-index.sh
```

---

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feat/my-feature`)
3. Make your changes and ensure `make check` passes
4. Open a pull request

---

## License

MIT — see [LICENSE](LICENSE).
