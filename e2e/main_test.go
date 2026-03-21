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
)

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
	defer cancel()

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

	// ── Namespace ─────────────────────────────────────────────────────────────
	if err := framework.EnsureNamespace(ctx, client, framework.E2ENamespace); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] create namespace: %v\n", err)
		return 1
	}

	// ── Helm repos ────────────────────────────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Setting up Helm repos...")
	if err := framework.HelmSetupRepos(ctx); err != nil {
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
		if err := framework.HelmInstall(ctx, testCluster.KubeconfigPath, r); err != nil {
			fmt.Fprintf(os.Stderr, "[e2e] helm install %s: %v\n", r.ReleaseName, err)
			return 1
		}
	}

	// ── Wait for all workloads to settle ──────────────────────────────────────
	fmt.Fprintln(os.Stderr, "[e2e] Waiting for workloads to be ready...")
	if err := framework.WaitForCoreWorkloads(ctx, client, framework.E2ENamespace, 5*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] workloads not ready: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "[e2e] All workloads ready. Running tests.")

	return m.Run()
}
