//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

const (
	drainTimeout  = 8 * time.Minute
	workloadReady = 5 * time.Minute
)

// --------------------------------------------------------------------------
// Per-test helpers
// --------------------------------------------------------------------------

// waitAllReady blocks until NATS and Grafana are fully healthy. Call this at
// the start of every drain test so tests don't start against a degraded
// cluster from the previous test's rollout.
func waitAllReady(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), workloadReady)
	defer cancel()
	if err := framework.WaitForCoreWorkloads(ctx, testClient, framework.E2ENamespace, workloadReady); err != nil {
		t.Fatalf("cluster not ready before test: %v", err)
	}
}

// agentNodeWithPod returns an agent node that currently has a running pod
// matching labelSelector. If no agent node qualifies the test is skipped —
// this is a valid cluster state (e.g. scheduler placed the pod on the server
// node), not a test failure.
func agentNodeWithPod(t *testing.T, labelSelector string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := framework.AgentNodeWithPod(ctx, testClient, testCluster, framework.E2ENamespace, labelSelector)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	return node
}

// uncordon restores a node to schedulable. Errors are logged, not fatal —
// uncordon is best-effort cleanup.
func uncordon(t *testing.T, nodeName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := framework.UncordonNode(ctx, testCluster.KubeconfigPath, nodeName); err != nil {
		t.Logf("uncordon %s: %v (ignored)", nodeName, err)
	}
}

// getAnnotation captures the current restartedAt annotation on a workload.
// Use before and after a drain and compare; do NOT just check for non-empty.
func getAnnotation(t *testing.T, kind, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ann, err := framework.GetRestartAnnotation(ctx, testClient, framework.E2ENamespace, kind, name)
	if err != nil {
		t.Fatalf("getAnnotation %s/%s: %v", kind, name, err)
	}
	return ann
}

// assertRestarted fails if the workload's restartedAt annotation did not
// change from before (meaning kubectl-safed did not trigger a rolling restart).
func assertRestarted(t *testing.T, kind, name, before string) {
	t.Helper()
	after := getAnnotation(t, kind, name)
	if after == before {
		t.Errorf("%s/%s: restartedAt annotation did not change — workload was not restarted (value: %q)",
			kind, name, before)
	}
}

// assertNotRestarted fails if the workload's restartedAt annotation changed,
// meaning kubectl-safed incorrectly restarted a workload it should have skipped.
func assertNotRestarted(t *testing.T, kind, name, before string) {
	t.Helper()
	after := getAnnotation(t, kind, name)
	if after != before {
		t.Errorf("%s/%s: restartedAt changed from %q to %q — workload should NOT have been restarted",
			kind, name, before, after)
	}
}

// verifyNodeCordoned asserts that nodeName is Unschedulable.
func verifyNodeCordoned(t *testing.T, nodeName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", nodeName, err)
	}
	if !node.Spec.Unschedulable {
		t.Errorf("node %s should be cordoned (Unschedulable=true) after drain", nodeName)
	}
}

// verifyNodeNotCordoned asserts that nodeName is schedulable (dry-run / abort cases).
func verifyNodeNotCordoned(t *testing.T, nodeName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", nodeName, err)
	}
	if node.Spec.Unschedulable {
		t.Errorf("node %s should NOT be cordoned", nodeName)
	}
}

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
// TestDrain_MultipleWorkloads — NATS + Grafana on the same node
// --------------------------------------------------------------------------

// TestDrain_MultipleWorkloads targets a node that hosts both a NATS pod and a
// Grafana pod, verifying both workloads receive a rolling restart.
func TestDrain_MultipleWorkloads(t *testing.T) {
	waitAllReady(t)

	// Find a node that has both NATS and Grafana pods.
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) == 0 {
		t.Fatalf("no agent nodes: %v", err)
	}

	// Find an agent node that has pods from both NATS and Grafana.
	var target string
	for _, agent := range agents {
		hasNATS, _ := framework.NodeHasActivePodsWithSelector(ctx, testClient, agent, framework.E2ENamespace, framework.NATSPodSelector)
		hasGrafana, _ := framework.NodeHasActivePodsWithSelector(ctx, testClient, agent, framework.E2ENamespace, framework.GrafanaPodSelector)
		if hasNATS && hasGrafana {
			target = agent
			break
		}
	}
	if target == "" {
		t.Skip("no agent node has both NATS and Grafana pods — skipping")
	}
	defer uncordon(t, target)

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "5m", "--pod-vacate-timeout", "2m")
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	verifyNodeCordoned(t, target)

	// Both workloads had pods on this node, so both must have been restarted.
	assertRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)
	assertRestarted(t, "Deployment", framework.GrafanaDeploymentName, beforeGrafana)

	if err := framework.WaitForCoreWorkloads(ctx, testClient, framework.E2ENamespace, workloadReady); err != nil {
		t.Fatalf("workloads not healthy after multi-workload drain: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Priority — high-priority NATS before low-priority Grafana
// --------------------------------------------------------------------------

// TestDrain_Priority runs a sequential drain (--max-concurrency=1) on a node
// that has both NATS (priority=10) and Grafana (priority=100), then verifies
// that NATS received its restartedAt annotation before Grafana by comparing
// RFC3339 timestamps. Lower priority value = restarts first.
//
// The test skips if no agent node currently has both workloads — this is a
// valid scheduler outcome and the ordering assertion cannot be made without
// both workloads on the same drained node.
func TestDrain_Priority(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Need a node hosting both NATS and Grafana so kubectl-safed restarts both
	// in this single drain, making the timestamp comparison meaningful.
	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) == 0 {
		t.Skipf("no agent nodes: %v", err)
	}
	var target string
	for _, a := range agents {
		hasNATS, _ := framework.NodeHasActivePodsWithSelector(ctx, testClient, a, framework.E2ENamespace, framework.NATSPodSelector)
		hasGrafana, _ := framework.NodeHasActivePodsWithSelector(ctx, testClient, a, framework.E2ENamespace, framework.GrafanaPodSelector)
		if hasNATS && hasGrafana {
			target = a
			break
		}
	}
	if target == "" {
		t.Skip("no agent node has both NATS and Grafana pods — cannot assert priority ordering")
	}
	defer uncordon(t, target)

	// Capture before so we can verify both workloads were actually restarted
	// by this drain, not carry-over timestamps from previous tests.
	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	result := testBinary.Drain(ctx, target,
		"--rollout-timeout", "5m",
		"--max-concurrency", "1", // sequential: ordering is deterministic
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	natsAnn := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	grafanaAnn := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	if natsAnn == beforeNATS {
		t.Fatal("NATS annotation did not change — was not restarted by this drain")
	}
	if grafanaAnn == beforeGrafana {
		t.Fatal("Grafana annotation did not change — was not restarted by this drain")
	}

	// RFC3339 timestamps are lexicographically ordered — NATS (priority=10)
	// must have been restarted before or at the same time as Grafana (priority=100).
	if natsAnn > grafanaAnn {
		t.Errorf("priority ordering violated: NATS (priority=10) annotation %q is LATER than "+
			"Grafana (priority=100) annotation %q — lower priority value must restart first",
			natsAnn, grafanaAnn)
	}
}

// --------------------------------------------------------------------------
// TestDrain_SkipWorkload
// --------------------------------------------------------------------------

func TestDrain_SkipWorkload(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target,
		"--skip-workload", "Deployment/e2e/grafana",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	// Grafana must NOT have been restarted.
	assertNotRestarted(t, "Deployment", framework.GrafanaDeploymentName, beforeGrafana)
	// NATS must have been restarted.
	assertRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)
}

// --------------------------------------------------------------------------
// TestDrain_OnlyWorkload
// --------------------------------------------------------------------------

func TestDrain_OnlyWorkload(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)
	beforeGrafana := getAnnotation(t, "Deployment", framework.GrafanaDeploymentName)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target,
		"--only-workload", "StatefulSet/e2e/nats",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	// NATS must have been restarted.
	assertRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)
	// Grafana must NOT have been restarted.
	assertNotRestarted(t, "Deployment", framework.GrafanaDeploymentName, beforeGrafana)
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_WarnMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_WarnMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Deploy single-replica worker.
	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.WorkerManifest); err != nil {
		t.Fatalf("apply worker: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	if err := framework.WaitForDeploymentReady(ctx, testClient, framework.E2ENamespace, "worker", workloadReady); err != nil {
		t.Fatalf("worker not ready: %v", err)
	}

	// Drain the node where the single-replica worker pod is running, so the
	// preflight check actually detects the single-replica risk.
	target := agentNodeWithPod(t, framework.WorkerPodSelector)
	defer uncordon(t, target)

	result := testBinary.Drain(ctx, target,
		"--preflight", "warn",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("warn mode must not abort: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_StrictMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_StrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Deploy single-replica worker that will trigger preflight risk.
	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.WorkerManifest); err != nil {
		t.Fatalf("apply worker: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	if err := framework.WaitForDeploymentReady(ctx, testClient, framework.E2ENamespace, "worker", workloadReady); err != nil {
		t.Fatalf("worker not ready: %v", err)
	}

	// Drain the node where the single-replica worker pod is running, so the
	// preflight check actually detects the single-replica risk.
	target := agentNodeWithPod(t, framework.WorkerPodSelector)
	// No defer uncordon — strict mode must abort before cordoning.

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--rollout-timeout", "5m",
	)
	if result.Err == nil {
		t.Fatal("strict mode must exit non-zero when risk is found")
		_ = framework.UncordonNode(ctx, testCluster.KubeconfigPath, target)
	}

	// Node must NOT be cordoned (drain aborted before cordon step).
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_NodeSelector
// --------------------------------------------------------------------------

func TestDrain_NodeSelector(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) < 2 {
		t.Skip("need at least 2 agent nodes for selector test")
	}

	target := agents[0]
	other := agents[1]
	defer uncordon(t, target)

	// Label only the target node.
	const labelKey = "safed-e2e-target"
	patch := []byte(`{"metadata":{"labels":{"` + labelKey + `":"true"}}}`)
	if _, err := testClient.CoreV1().Nodes().Patch(
		ctx, target, "application/merge-patch+json", patch, metav1.PatchOptions{},
	); err != nil {
		t.Fatalf("label node: %v", err)
	}
	defer func() {
		removePatch := []byte(`{"metadata":{"labels":{"` + labelKey + `":null}}}`)
		_, _ = testClient.CoreV1().Nodes().Patch(
			context.Background(), target, "application/merge-patch+json",
			removePatch, metav1.PatchOptions{},
		)
	}()

	result := testBinary.DrainWithSelector(ctx, labelKey+"=true", "--rollout-timeout", "5m")
	if result.Err != nil {
		t.Fatalf("selector drain failed: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}

	verifyNodeCordoned(t, target)

	// The other agent must remain schedulable.
	n, err := testClient.CoreV1().Nodes().Get(ctx, other, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", other, err)
	}
	if n.Spec.Unschedulable {
		t.Errorf("non-targeted node %s should not be cordoned", other)
	}
}

// --------------------------------------------------------------------------
// TestDrain_EmitEvents
// --------------------------------------------------------------------------

func TestDrain_EmitEvents(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	result := testBinary.Drain(ctx, target,
		"--emit-events",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain with --emit-events failed: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}

	// Poll for a Draining or Drained event on the node.
	deadline := time.Now().Add(30 * time.Second)
	var found bool
	for !found && time.Now().Before(deadline) {
		events, err := framework.EventsForNode(ctx, testClient, target)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		for _, e := range events {
			if e.Reason == "Draining" || e.Reason == "Drained" {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(2 * time.Second)
		}
	}
	if !found {
		t.Errorf("no Draining/Drained event on node %s after --emit-events", target)
	}
}

// --------------------------------------------------------------------------
// TestDrain_DaemonSetNotRestarted
// --------------------------------------------------------------------------

func TestDrain_DaemonSetNotRestarted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.DaemonSetManifest); err != nil {
		t.Fatalf("apply daemonset: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.DaemonSetManifest)
	}()

	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "5m")
	if result.Err != nil {
		t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	ds, err := testClient.AppsV1().DaemonSets(framework.E2ENamespace).Get(ctx, "node-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}
	if ann := ds.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; ann != "" {
		t.Errorf("DaemonSet received restartedAt %q — DaemonSets must never be restarted by safed", ann)
	}
}

// --------------------------------------------------------------------------
// TestDrain_CheckpointResume
// --------------------------------------------------------------------------

func TestDrain_CheckpointResume(t *testing.T) {
	waitAllReady(t)

	target := agentNodeWithPod(t, framework.NATSPodSelector)
	defer uncordon(t, target)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// First drain completes fully and deletes the checkpoint on success.
	r1 := testBinary.Drain(ctx, target, "--rollout-timeout", "5m")
	if r1.Err != nil {
		t.Fatalf("first drain failed: %v\nstdout: %s\nstderr: %s", r1.Err, r1.Stdout, r1.Stderr)
	}
	verifyNodeCordoned(t, target)

	// Uncordon so we can drain it again.
	if err := framework.UncordonNode(ctx, testCluster.KubeconfigPath, target); err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	defer uncordon(t, target)

	// Wait for workloads to be ready before the second drain.
	waitAllReady(t)

	// Second drain with --resume. Since the first drain succeeded and deleted
	// its checkpoint, this runs as a fresh drain.
	r2 := testBinary.Drain(ctx, target, "--rollout-timeout", "5m", "--resume")
	if r2.Err != nil {
		t.Fatalf("resume drain failed: %v\nstdout: %s\nstderr: %s", r2.Err, r2.Stdout, r2.Stderr)
	}
	verifyNodeCordoned(t, target)

	// Output must not contain any error about missing checkpoint.
	combined := r2.Stdout + r2.Stderr
	if strings.Contains(strings.ToLower(combined), "checkpoint error") {
		t.Errorf("unexpected checkpoint error in output: %s", combined)
	}
}

// --------------------------------------------------------------------------
// TestDrain_MultiNode
// --------------------------------------------------------------------------

// TestDrain_MultiNode is intentionally placed after all tests that require
// existing workload pods on agent nodes. Draining both agents moves all pods
// to the server node; they do not automatically reschedule back after uncordon.
// Tests that follow create their own fresh pods (PDB, CrashLoop) so they are
// unaffected by this redistribution.
func TestDrain_MultiNode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) < 2 {
		t.Skip("need at least 2 agent nodes for multi-node drain")
	}
	for _, a := range agents {
		defer uncordon(t, a)
	}

	beforeNATS := getAnnotation(t, "StatefulSet", framework.NATSStatefulSetName)

	result := testBinary.DrainNodes(ctx, agents,
		"--rollout-timeout", "5m",
		"--node-concurrency", "2",
	)
	if result.Err != nil {
		t.Fatalf("multi-node drain failed: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}

	for _, agent := range agents {
		verifyNodeCordoned(t, agent)
	}

	assertRestarted(t, "StatefulSet", framework.NATSStatefulSetName, beforeNATS)

	// All workloads must recover (pods reschedule to server node).
	if err := framework.WaitForCoreWorkloads(ctx, testClient, framework.E2ENamespace, workloadReady); err != nil {
		t.Fatalf("workloads not healthy after multi-node drain: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_PDBBlockedEviction — evictWithPDBRetry times out on blocking PDB
// --------------------------------------------------------------------------

// TestDrain_PDBBlockedEviction deploys a standalone pod (no owner) with a
// zero-tolerance PDB, then drains the node with --force. Because the pod has
// no owner, kubectl-safed must evict it via the eviction API. The PDB
// (maxUnavailable=0) permanently blocks eviction, so the drain must fail after
// --eviction-timeout, exercising the evictWithPDBRetry path.
func TestDrain_PDBBlockedEviction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.StandalonePodWithPDBManifest); err != nil {
		t.Fatalf("apply standalone pod + PDB: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.StandalonePodWithPDBManifest)
	}()

	// Wait for the standalone pod to be Running somewhere.
	target, err := framework.NodeWithPodForLabel(ctx, testClient, framework.E2ENamespace,
		framework.StandalonePDBPodSelector, 60*time.Second)
	if err != nil {
		t.Skipf("standalone pod not running: %v", err)
	}

	// Only test against agent nodes to avoid disrupting the server.
	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil {
		t.Fatalf("agent nodes: %v", err)
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}
	if !agentSet[target] {
		t.Skipf("standalone pod scheduled on server node %s — skipping", target)
	}
	defer uncordon(t, target)

	// Drain with --force (required for standalone pods) and a short timeout so
	// the test completes quickly. The PDB must block eviction and the drain fails.
	result := testBinary.Drain(ctx, target,
		"--force",
		"--eviction-timeout", "20s",
		"--pdb-retry-interval", "2s",
		"--rollout-timeout", "2m",
	)
	if result.Err == nil {
		t.Fatal("drain should fail: PDB (maxUnavailable=0) must block eviction of standalone pod")
	}

	// Node must be cordoned — drain got as far as the cordon step before failing.
	verifyNodeCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_CrashLoopAbort — drain aborts fast on CrashLoopBackOff
// --------------------------------------------------------------------------

// TestDrain_CrashLoopAbort deploys a container that exits immediately (exit 1).
// kubectl-safed must detect the CrashLoopBackOff condition during the rolling
// restart poll loop and abort with a non-zero exit code.
func TestDrain_CrashLoopAbort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.CrashingDeploymentManifest); err != nil {
		t.Fatalf("apply crashing deployment: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.CrashingDeploymentManifest)
	}()

	// Wait for the crashing pod to fail at least once (LastTerminationState set).
	target, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
		framework.CrasherPodSelector, 90*time.Second)
	if err != nil {
		t.Skipf("crashing pod did not crash on any node within timeout: %v", err)
	}

	// Only drain agent nodes.
	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil {
		t.Fatalf("agent nodes: %v", err)
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}
	if !agentSet[target] {
		t.Skipf("crashing pod on server node %s — skipping", target)
	}
	defer uncordon(t, target)

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "3m")
	if result.Err == nil {
		t.Fatal("drain should fail fast when a pod is in CrashLoopBackOff")
	}
}

// --------------------------------------------------------------------------
// TestDrain_UncordonOnFailure — node is uncordoned when drain fails
// --------------------------------------------------------------------------

// TestDrain_UncordonOnFailure verifies that --uncordon-on-failure restores the
// node to schedulable after a drain that aborts due to CrashLoopBackOff.
func TestDrain_UncordonOnFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.CrashingDeploymentManifest); err != nil {
		t.Fatalf("apply crashing deployment: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.CrashingDeploymentManifest)
	}()

	target, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
		framework.CrasherPodSelector, 90*time.Second)
	if err != nil {
		t.Skipf("crashing pod did not crash on any node within timeout: %v", err)
	}

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil {
		t.Fatalf("agent nodes: %v", err)
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}
	if !agentSet[target] {
		t.Skipf("crashing pod on server node %s — skipping", target)
	}
	// Best-effort cleanup — uncordon-on-failure should already handle this, but
	// defer a cleanup just in case the assertion below fails.
	defer uncordon(t, target)

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "3m", "--uncordon-on-failure")
	if result.Err == nil {
		t.Fatal("drain should fail on CrashLoopBackOff")
	}

	// --uncordon-on-failure must restore the node to schedulable.
	verifyNodeNotCordoned(t, target)
}
