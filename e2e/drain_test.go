//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

// --------------------------------------------------------------------------
// TestDrain_NodeNotFound
// --------------------------------------------------------------------------

func TestDrain_NodeNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := testBinary.Drain(ctx, "nonexistent-node-xyz")
	if result.Err == nil {
		t.Fatal("expected non-zero exit for missing node, got nil")
	}
}

// --------------------------------------------------------------------------
// TestDrain_DryRun
// --------------------------------------------------------------------------

func TestDrain_DryRun(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target, "--dry-run")
	if result.Err != nil {
		t.Fatalf("dry-run failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	// Node must NOT be cordoned.
	verifyNodeNotCordoned(t, target)

	// No annotations must have changed.
	assertNotRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)
	assertNotRestarted(t, "Deployment", framework.GrafanaDeploymentName, beforeGrafana)
}

// --------------------------------------------------------------------------
// TestDrain_NATS — StatefulSet rolling restart
// --------------------------------------------------------------------------

// TestDrain_NATS drains a node that hosts a NATS pod, verifying:
//   - Node is cordoned.
//   - NATS StatefulSet has a new restartedAt annotation (kubectl-safed triggered a restart).
//   - NATS cluster returns to 3 ready replicas.
//   - No active NATS pods remain on the drained node.
func TestDrain_NATS(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "5m", "--pod-vacate-timeout", "2m")
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	verifyNodeCordoned(t, target)
	assertRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)

	// NATS cluster must return to full health.
	if err := framework.WaitForStatefulSetReady(ctx, testClient, framework.E2ENamespace,
		framework.NATSStatefulSetName, workloadReady); err != nil {
		t.Fatalf("NATS not healthy after drain: %v", err)
	}

	// No active NATS pods on the drained node.
	if err := framework.WaitForNoActivePodsOnNode(ctx, testClient, target,
		framework.E2ENamespace, 2*time.Minute); err != nil {
		t.Errorf("active pods still on drained node: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Grafana — Deployment rolling restart
// --------------------------------------------------------------------------

func TestDrain_Grafana(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.GrafanaPodSelector)
	defer uncordon(t, target)

	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "5m", "--pod-vacate-timeout", "2m")
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	verifyNodeCordoned(t, target)
	assertRestarted(t, "Deployment", framework.GrafanaDeploymentName, beforeGrafana)

	if err := framework.WaitForDeploymentReady(ctx, testClient, framework.E2ENamespace,
		framework.GrafanaDeploymentName, workloadReady); err != nil {
		t.Fatalf("Grafana not healthy after drain: %v", err)
	}

	if err := framework.WaitForNoActivePodsOnNode(ctx, testClient, target,
		framework.E2ENamespace, 2*time.Minute); err != nil {
		t.Errorf("active pods still on drained node: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_MultipleWorkloads — two Deployments on the same node
// --------------------------------------------------------------------------

// TestDrain_MultipleWorkloads targets a node with two dedicated Deployments,
// verifying both workloads receive a rolling restart.
func TestDrain_MultipleWorkloads(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("multi-a", 100),
		simpleDeploymentManifest("multi-b", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "multi-a", "multi-b")

	beforeA := getAnnotation(t, "Deployment", "multi-a")
	beforeB := getAnnotation(t, "Deployment", "multi-b")

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--rollout-timeout", "5m",
		"--pod-vacate-timeout", "2m",
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	verifyNodeCordoned(t, target)

	// Both workloads had pods on this node, so both must have been restarted.
	assertRestarted(t, "Deployment", "multi-a", beforeA)
	assertRestarted(t, "Deployment", "multi-b", beforeB)

	for _, name := range []string{"multi-a", "multi-b"} {
		if err := framework.WaitForNoActivePodsWithSelectorOnNode(ctx, testClient, target,
			framework.E2ENamespace, "app="+name, 2*time.Minute); err != nil {
			t.Fatalf("%s still has pods on drained node: %v", name, err)
		}
	}
}

// --------------------------------------------------------------------------
// TestDrain_Priority — high-priority workload before low-priority workload
// --------------------------------------------------------------------------

// TestDrain_Priority runs a sequential drain (--max-concurrency=1) on a node
// that has two dedicated Deployments with different drain priorities, then
// verifies the higher-priority workload starts first in the drain log. Lower
// priority value = restarts first.
func TestDrain_Priority(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("priority-high", 10),
		simpleDeploymentManifest("priority-low", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "priority-high", "priority-low")

	beforeHigh := getAnnotation(t, "Deployment", "priority-high")
	beforeLow := getAnnotation(t, "Deployment", "priority-low")

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--rollout-timeout", "5m",
		"--max-concurrency", "1", // sequential: ordering is deterministic
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	assertRestarted(t, "Deployment", "priority-high", beforeHigh)
	assertRestarted(t, "Deployment", "priority-low", beforeLow)

	highStart := startIndex(result.Stdout, "Deployment/e2e/priority-high")
	lowStart := startIndex(result.Stdout, "Deployment/e2e/priority-low")
	if highStart == -1 || lowStart == -1 {
		t.Fatalf("missing start lines for priority workloads\nstdout: %s", result.Stdout)
	}
	if highStart > lowStart {
		t.Errorf("priority ordering violated: high priority workload started at line %d after low priority line %d\nstdout: %s",
			highStart, lowStart, result.Stdout)
	}
}

// --------------------------------------------------------------------------
// TestDrain_MaxConcurrencyBatches
// --------------------------------------------------------------------------

func TestDrain_MaxConcurrencyBatches(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("batch-a", 100),
		simpleDeploymentManifest("batch-b", 100),
		simpleDeploymentManifest("batch-c", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "batch-a", "batch-b", "batch-c")

	result := testBinary.Drain(ctx, target,
		"--dry-run",
		"--preflight", "off",
		"--poll-interval", "1s",
		"--max-concurrency", "2",
		"--only-workload", "Deployment/e2e/batch-a",
		"--only-workload", "Deployment/e2e/batch-b",
		"--only-workload", "Deployment/e2e/batch-c",
	)
	if result.Err != nil {
		t.Fatalf("batch drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
	if !strings.Contains(result.Stdout, "batch 1/2: starting 2 workload(s) concurrently") ||
		!strings.Contains(result.Stdout, "batch 2/2: starting 1 workload(s) concurrently") {
		t.Fatalf("batch drain output missing expected batch boundaries\nstdout: %s", result.Stdout)
	}
	for _, name := range []string{"batch-a", "batch-b", "batch-c"} {
		if !strings.Contains(result.Stdout, "Deployment/e2e/"+name) {
			t.Fatalf("batch drain output missing %s\nstdout: %s", name, result.Stdout)
		}
	}

	allAtOnce := testBinary.Drain(ctx, target,
		"--dry-run",
		"--preflight", "off",
		"--poll-interval", "1s",
		"--max-concurrency", "0",
		"--only-workload", "Deployment/e2e/batch-a",
		"--only-workload", "Deployment/e2e/batch-b",
	)
	if allAtOnce.Err != nil {
		t.Fatalf("all-at-once dry-run failed: %v\nstdout: %s\nstderr: %s", allAtOnce.Err, allAtOnce.Stdout, allAtOnce.Stderr)
	}
	if !strings.Contains(allAtOnce.Stdout, "Starting all 2 workload(s) concurrently") {
		t.Fatalf("all-at-once output missing concurrent start line\nstdout: %s", allAtOnce.Stdout)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_SkipWorkload
// --------------------------------------------------------------------------

func TestDrain_SkipWorkload(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("skip-keep", 100),
		simpleDeploymentManifest("skip-drop", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "skip-keep", "skip-drop")

	beforeKeep := getAnnotation(t, "Deployment", "skip-keep")
	beforeDrop := getAnnotation(t, "Deployment", "skip-drop")

	result := testBinary.Drain(ctx, target,
		"--skip-workload", "Deployment/e2e/skip-drop",
		"--preflight", "off",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	assertRestarted(t, "Deployment", "skip-keep", beforeKeep)
	assertNotRestarted(t, "Deployment", "skip-drop", beforeDrop)
	if !strings.Contains(result.Stdout, "Skipping Deployment/e2e/skip-drop (--skip-workload)") {
		t.Errorf("missing skip-workload log line\nstdout: %s", result.Stdout)
	}
}

// --------------------------------------------------------------------------
// TestDrain_OnlyWorkload
// --------------------------------------------------------------------------

func TestDrain_OnlyWorkload(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("only-keep", 100),
		simpleDeploymentManifest("only-drop", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "only-keep", "only-drop")

	beforeKeep := getAnnotation(t, "Deployment", "only-keep")
	beforeDrop := getAnnotation(t, "Deployment", "only-drop")

	result := testBinary.Drain(ctx, target,
		"--only-workload", "Deployment/e2e/only-keep",
		"--preflight", "off",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	assertRestarted(t, "Deployment", "only-keep", beforeKeep)
	assertNotRestarted(t, "Deployment", "only-drop", beforeDrop)
	if !strings.Contains(result.Stdout, "Skipping Deployment/e2e/only-drop (not in --only-workload list)") {
		t.Errorf("missing only-workload log line\nstdout: %s", result.Stdout)
	}
}
