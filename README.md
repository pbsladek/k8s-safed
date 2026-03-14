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
2. Cordon     – mark the node unschedulable so no new pods are scheduled
3. Discover   – find every Deployment and StatefulSet with pods on the node
                (Pod → ReplicaSet → Deployment ownership is fully resolved)
4. Restart    – for each workload, patch the pod template with a
                restartedAt annotation (identical to `kubectl rollout restart`)
5. Wait       – poll rollout status until all replicas are updated and ready;
                fail fast if ProgressDeadlineExceeded is detected
6. Verify     – confirm all of that workload's pods have left the node
                before moving to the next one
7. Evict      – evict any remaining pods (DaemonSets, Jobs, standalones)
                controlled by --skip-daemon-sets, --force, --delete-emptydir-data
```

Rolling restarts are processed **one workload at a time**. Each workload must
fully migrate before the next one starts.

---

## Differences from `kubectl drain`

| | `kubectl drain` | `kubectl-safed drain` |
|---|---|---|
| Mechanism | Pod eviction | Rolling restart → natural pod migration |
| Risk of downtime | High if PDB is misconfigured | Low — scheduler places new pods first |
| Respects RollingUpdate strategy | No | Yes |
| PDB awareness | Via eviction API | Via rollout controller |
| Verifies pods left the node | No | Yes, per workload |
| Progress visibility | Minimal | Logs updated/ready/available counts |

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
    verbs: ["list", "get"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["get", "list", "patch"]
```

### Go / kubectl

- kubectl 1.24+
- Kubernetes cluster 1.22+

---

## Installation

### Via krew (recommended)

Install [krew](https://krew.sigs.k8s.io/docs/user-guide/setup/install/) first,
then:

```bash
# Install from the latest release manifest
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
go build -o kubectl-safed .
sudo mv kubectl-safed /usr/local/bin/
```

---

## Usage

```
kubectl safed drain NODE [flags]
```

### Examples

```bash
# Preview what would happen — no changes made
kubectl safed drain worker-1 --dry-run

# Drain with default settings (DaemonSets skipped)
kubectl safed drain worker-1

# Drain, waiting up to 10 minutes per workload rollout
kubectl safed drain worker-1 --rollout-timeout=10m

# Drain a node and also evict DaemonSet pods
kubectl safed drain worker-1 --skip-daemon-sets=false

# Drain and evict pods that use emptyDir volumes (data will be lost)
kubectl safed drain worker-1 --delete-emptydir-data

# Drain and evict standalone pods (no owner) and Job-owned pods
kubectl safed drain worker-1 --force

# Use a specific kubeconfig context
kubectl safed drain worker-1 --context=prod-cluster
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Preview all actions without making any changes |
| `--rollout-timeout` | `5m` | Maximum time to wait for each workload rollout to complete |
| `--skip-daemon-sets` | `true` | Skip eviction of DaemonSet-managed pods |
| `--delete-emptydir-data` | `false` | Allow eviction of pods using emptyDir volumes (data loss) |
| `--grace-period` | `-1` | Grace period in seconds for pod termination; -1 uses the pod's default |
| `--force` | `false` | Evict standalone pods (no owner) and Job-owned pods |
| `--timeout` | `0` | Overall drain timeout; 0 means no timeout |

### Global flags (standard kubectl flags)

```
--kubeconfig, --context, --namespace, --server, --token,
--certificate-authority, --as, --as-group, ...
```

---

## What gets skipped

By default the following pods are **never** evicted or restarted:

| Pod type | Reason |
|---|---|
| Mirror pods (static pods) | Managed directly by kubelet; cannot be evicted via API |
| `Succeeded` / `Failed` pods | Already terminal; nothing to do |
| Already-terminating pods | `DeletionTimestamp` is set; kubelet is cleaning them up |
| DaemonSet pods | Run on every node by design (override with `--skip-daemon-sets=false`) |
| Pods with `emptyDir` | Eviction causes data loss (override with `--delete-emptydir-data`) |
| Standalone pods | No owner reference; require `--force` |
| Job-owned pods | Not rolling-restart managed; require `--force` |

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
targeting your fork. This lets the release script push a branch and open
a PR on your behalf.

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

The krew maintainers review and merge the PR. After merge, `kubectl krew install safed`
works for everyone.

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
3. Make your changes and ensure `go build ./...` and `go vet ./...` pass
4. Open a pull request

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
