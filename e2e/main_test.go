//go:build e2e

// Package e2e contains end-to-end tests for kubectl-safed.
//
// Tests require k3d and helm to be installed and in $PATH. They create a real
// multi-node Kubernetes cluster, deploy NATS, Grafana, and kube-state-metrics
// via their official Helm charts, then run the compiled kubectl-safed binary
// against the cluster.
//
// Run all tests:
//
//	make e2e
//
// Run a single test:
//
//	make e2e-run TEST=TestDrain_NATS
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

var (
	testCluster *framework.Cluster
	testClient  kubernetes.Interface
	testBinary  *framework.Binary
	artifactDir string
)

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 33*time.Minute)
	defer cancel()

	artifactDir = os.Getenv("SAFED_E2E_ARTIFACT_DIR")
	if artifactDir == "" {
		artifactDir = filepath.Join(os.TempDir(), "safed-e2e-diagnostics")
	}
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] create artifact dir %s: %v\n", artifactDir, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[e2e] Diagnostics artifacts: %s\n", artifactDir)

	// ── Build binary ──────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Building kubectl-safed...")
	binPath, err := framework.BuildBinary(moduleRoot())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] build failed: %v\n", err)
		return 1
	}
	defer os.RemoveAll(filepath.Dir(binPath))

	// ── k3d cluster ───────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Creating k3d cluster (1 server + 2 agents)...")
	testCluster = framework.NewCluster(framework.ClusterName())
	if err := testCluster.Create(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] cluster create: %v\n", err)
		return 1
	}
	defer func() {
		fmt.Fprintln(os.Stderr, "[e2e] Destroying cluster...")
		_ = testCluster.Destroy(context.Background())
	}()

	// ── Kubernetes client ─────────────────────────────────────────────────────
	client, err := framework.NewClient(testCluster.KubeconfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] k8s client: %v\n", err)
		return 1
	}
	testClient = client

	testBinary = &framework.Binary{
		Path:           binPath,
		KubeconfigPath: testCluster.KubeconfigPath,
	}

	// ── Cluster baseline ──────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Waiting for cluster addons to be ready...")
	if err := retrySetup(ctx, "wait for cluster addons", 3, func() error {
		return framework.WaitForClusterAddons(ctx, client, 3*time.Minute)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] cluster addons not ready: %v\n", err)
		dumpSetupDiagnostics(testCluster.KubeconfigPath)
		return 1
	}

	// ── Namespace ─────────────────────────────────────────────────────────────
	if err := framework.EnsureNamespace(ctx, client, framework.E2ENamespace); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] create namespace: %v\n", err)
		return 1
	}

	// ── Helm repos ────────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Setting up Helm repos...")
	if err := retrySetup(ctx, "helm repo setup", 3, func() error {
		return framework.HelmSetupRepos(ctx)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] helm repo setup: %v\n", err)
		return 1
	}

	// ── Deploy core workloads via Helm ────────────────────────────────────────
	releases := []framework.HelmRelease{
		framework.NATSRelease(framework.E2ENamespace),
		framework.GrafanaRelease(framework.E2ENamespace),
		framework.KubeStateMetricsRelease(framework.E2ENamespace),
	}
	for _, r := range releases {
		fmt.Fprintf(os.Stderr, "[e2e] Installing %s (%s)...\n", r.ReleaseName, r.Chart)
		if err := retrySetup(ctx, "helm install "+r.ReleaseName, 2, func() error {
			return framework.HelmInstall(ctx, testCluster.KubeconfigPath, r)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[e2e] helm install %s: %v\n", r.ReleaseName, err)
			dumpSetupDiagnostics(testCluster.KubeconfigPath)
			return 1
		}
	}

	// ── Wait for all workloads to settle ──────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Waiting for workloads to be ready...")
	if err := retrySetup(ctx, "wait for core workloads", 3, func() error {
		return framework.WaitForCoreWorkloads(ctx, client, framework.E2ENamespace, 5*time.Minute)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] workloads not ready: %v\n", err)
		dumpSetupDiagnostics(testCluster.KubeconfigPath)
		return 1
	}
	fmt.Fprintln(os.Stderr, "[e2e] All workloads ready. Running tests.")

	return m.Run()
}

func retrySetup(ctx context.Context, label string, attempts int, fn func() error) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			fmt.Fprintf(os.Stderr, "[e2e] Retrying %s (attempt %d/%d) after: %v\n", label, attempt, attempts, lastErr)
		}
		if err := fn(); err != nil {
			lastErr = err
			if attempt == attempts {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: %w", label, ctx.Err())
			case <-time.After(time.Duration(attempt) * 5 * time.Second):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("%s: %w", label, lastErr)
}

func dumpSetupDiagnostics(kubeconfigPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dumpSetupKubectl(ctx, kubeconfigPath, "nodes", "get", "nodes", "-o", "wide")
	dumpSetupKubectl(ctx, kubeconfigPath, "describe-nodes", "describe", "nodes")
	dumpSetupKubectl(ctx, kubeconfigPath, "pods", "get", "pods", "-A", "-o", "wide")
	dumpSetupKubectl(ctx, kubeconfigPath, "describe-pods", "describe", "pods", "-A")
	dumpSetupKubectl(ctx, kubeconfigPath, "events", "get", "events", "-A", "--sort-by=.lastTimestamp")
	dumpSetupHelm(ctx, kubeconfigPath, "helm-list", "list", "-A")
	for _, release := range []string{"nats", "grafana", "kube-state-metrics"} {
		dumpSetupHelm(ctx, kubeconfigPath, "helm-status-"+release, "status", release, "--namespace", framework.E2ENamespace)
	}
}

func dumpSetupKubectl(ctx context.Context, kubeconfigPath, label string, args ...string) {
	allArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] diagnostics kubectl %s failed: %v\n%s\n", label, err, out)
		writeArtifact("setup-"+label+".txt", out)
		return
	}
	fmt.Fprintf(os.Stderr, "[e2e] diagnostics kubectl %s:\n%s\n", label, out)
	writeArtifact("setup-"+label+".txt", out)
}

func dumpSetupHelm(ctx context.Context, kubeconfigPath, label string, args ...string) {
	allArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "helm", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] diagnostics helm %s failed: %v\n%s\n", label, err, out)
		writeArtifact("setup-"+label+".txt", out)
		return
	}
	fmt.Fprintf(os.Stderr, "[e2e] diagnostics helm %s:\n%s\n", label, out)
	writeArtifact("setup-"+label+".txt", out)
}
