//go:build e2e

package framework

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// HelmRelease defines a chart to install via helm.
type HelmRelease struct {
	// ReleaseName is the helm release name (e.g. "nats").
	ReleaseName string
	// Chart is the repo/chart reference (e.g. "nats/nats").
	Chart string
	// Namespace to install into.
	Namespace string
	// ValuesYAML is an optional YAML values override written to a temp file.
	ValuesYAML string
	// Timeout for helm install --wait. Defaults to 5 minutes.
	Timeout time.Duration
}

// HelmRepos maps a repo name to its URL. Call HelmSetupRepos once in TestMain.
var HelmRepos = map[string]string{
	"nats":                   "https://nats-io.github.io/k8s/helm/charts/",
	"grafana":                "https://grafana.github.io/helm-charts",
	"prometheus-community":   "https://prometheus-community.github.io/helm-charts",
}

// HelmSetupRepos adds required helm repos and updates them.
func HelmSetupRepos(ctx context.Context) error {
	for name, url := range HelmRepos {
		cmd := exec.CommandContext(ctx, "helm", "repo", "add", name, url,
			"--force-update")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("helm repo add %s: %w\n%s", name, err, out)
		}
	}
	cmd := exec.CommandContext(ctx, "helm", "repo", "update")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm repo update: %w", err)
	}
	return nil
}

// HelmInstall installs a release and waits for it to be ready.
func HelmInstall(ctx context.Context, kubeconfigPath string, r HelmRelease) error {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	args := []string{
		"install", r.ReleaseName, r.Chart,
		"--kubeconfig", kubeconfigPath,
		"--namespace", r.Namespace,
		"--create-namespace",
		"--wait",
		"--timeout", timeout.String(),
	}

	// Write values to a temp file if provided.
	if r.ValuesYAML != "" {
		f, err := os.CreateTemp("", "safed-e2e-values-*.yaml")
		if err != nil {
			return fmt.Errorf("create values file: %w", err)
		}
		if _, err := f.WriteString(r.ValuesYAML); err != nil {
			f.Close()
			os.Remove(f.Name())
			return fmt.Errorf("write values file: %w", err)
		}
		f.Close()
		defer os.Remove(f.Name())
		args = append(args, "-f", f.Name())
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm install %s (%s): %w", r.ReleaseName, r.Chart, err)
	}
	return nil
}

// HelmUninstall removes a helm release. Missing releases are ignored.
func HelmUninstall(ctx context.Context, kubeconfigPath, releaseName, namespace string) error {
	cmd := exec.CommandContext(ctx, "helm", "uninstall", releaseName,
		"--kubeconfig", kubeconfigPath,
		"--namespace", namespace,
		"--ignore-not-found",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm uninstall %s: %w\n%s", releaseName, err, out)
	}
	return nil
}

// --------------------------------------------------------------------------
// Release definitions
// --------------------------------------------------------------------------

// NATSRelease returns the NATS helm release config: a 3-replica cluster
// (StatefulSet) spread one pod per node with drain priority 10.
func NATSRelease(ns string) HelmRelease {
	return HelmRelease{
		ReleaseName: "nats",
		Chart:       "nats/nats",
		Namespace:   ns,
		Timeout:     5 * time.Minute,
		ValuesYAML: `
config:
  cluster:
    enabled: true
    replicas: 3
statefulSet:
  merge:
    metadata:
      annotations:
        kubectl.safed.io/drain-priority: "10"
  topologySpreadConstraints:
    kubernetes.io/hostname:
      maxSkew: 1
      whenUnsatisfiable: ScheduleAnyway
container:
  merge:
    resources:
      requests:
        cpu: 50m
        memory: 64Mi
      limits:
        memory: 128Mi
natsBox:
  enabled: false
`,
	}
}

// GrafanaRelease returns the Grafana helm release config: a 3-replica
// Deployment spread one pod per node with default drain priority (100).
func GrafanaRelease(ns string) HelmRelease {
	return HelmRelease{
		ReleaseName: "grafana",
		Chart:       "grafana/grafana",
		Namespace:   ns,
		Timeout:     5 * time.Minute,
		ValuesYAML: `
replicas: 3
annotations:
  kubectl.safed.io/drain-priority: "100"
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: grafana
        app.kubernetes.io/instance: grafana
resources:
  requests:
    cpu: 50m
    memory: 128Mi
  limits:
    memory: 256Mi
persistence:
  enabled: false
adminPassword: safed-e2e
`,
	}
}

// KubeStateMetricsRelease returns the kube-state-metrics release config:
// a lightweight single-replica Deployment.
func KubeStateMetricsRelease(ns string) HelmRelease {
	return HelmRelease{
		ReleaseName: "kube-state-metrics",
		Chart:       "prometheus-community/kube-state-metrics",
		Namespace:   ns,
		Timeout:     3 * time.Minute,
		ValuesYAML: `
resources:
  requests:
    cpu: 10m
    memory: 32Mi
  limits:
    memory: 64Mi
`,
	}
}
