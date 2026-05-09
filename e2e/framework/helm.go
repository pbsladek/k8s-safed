package framework

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// HelmRelease defines a chart to install via helm.
type HelmRelease struct {
	// ReleaseName is the helm release name (e.g. "nats").
	ReleaseName string
	// Chart is the repo/chart reference (e.g. "nats/nats").
	Chart string
	// Version pins the chart version used by the e2e suite.
	Version string
	// Namespace to install into.
	Namespace string
	// ValuesFile is an optional Helm values override file.
	ValuesFile string
	// Timeout for helm install --wait. Defaults to 5 minutes.
	Timeout time.Duration
}

// HelmRepos maps a repo name to its URL. Call HelmSetupRepos once in TestMain.
var HelmRepos = map[string]string{
	"nats":                 "https://nats-io.github.io/k8s/helm/charts/",
	"grafana":              "https://grafana.github.io/helm-charts",
	"prometheus-community": "https://prometheus-community.github.io/helm-charts",
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

// HelmInstall installs or upgrades a release and waits for it to be ready.
func HelmInstall(ctx context.Context, kubeconfigPath string, r HelmRelease) error {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	args := []string{
		"upgrade", "--install", r.ReleaseName, r.Chart,
		"--kubeconfig", kubeconfigPath,
		"--namespace", r.Namespace,
		"--create-namespace",
		"--wait",
		"--timeout", timeout.String(),
	}
	if r.Version != "" {
		args = append(args, "--version", r.Version)
	}

	if r.ValuesFile != "" {
		args = append(args, "-f", r.ValuesFile)
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade --install %s (%s): %w", r.ReleaseName, r.Chart, err)
	}
	return nil
}

func helmValuesFile(name string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("resolve helm values file: runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "helm-values", name)
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
		Version:     "2.12.6",
		Namespace:   ns,
		Timeout:     8 * time.Minute,
		ValuesFile:  helmValuesFile("nats.yaml"),
	}
}

// GrafanaRelease returns the Grafana helm release config: a 3-replica
// Deployment spread one pod per node with default drain priority (100).
func GrafanaRelease(ns string) HelmRelease {
	return HelmRelease{
		ReleaseName: "grafana",
		Chart:       "grafana/grafana",
		Version:     "10.5.15",
		Namespace:   ns,
		Timeout:     8 * time.Minute,
		ValuesFile:  helmValuesFile("grafana.yaml"),
	}
}

// KubeStateMetricsRelease returns the kube-state-metrics release config:
// a lightweight single-replica Deployment.
func KubeStateMetricsRelease(ns string) HelmRelease {
	return HelmRelease{
		ReleaseName: "kube-state-metrics",
		Chart:       "prometheus-community/kube-state-metrics",
		Version:     "7.3.0",
		Namespace:   ns,
		Timeout:     5 * time.Minute,
		ValuesFile:  helmValuesFile("kube-state-metrics.yaml"),
	}
}
