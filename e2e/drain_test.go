//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pbsladek/k8s-safed/e2e/framework"
	drainpkg "github.com/pbsladek/k8s-safed/pkg/drain"
)

const (
	drainTimeout       = 8 * time.Minute
	workloadReady      = 5 * time.Minute
	secondaryNamespace = "e2e-alt"
)

var diagnosticsRegistered sync.Map

// --------------------------------------------------------------------------
// Per-test helpers
// --------------------------------------------------------------------------

type deploymentRef struct {
	namespace string
	name      string
}

type jsonLogRecord struct {
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Subject string `json:"subject"`
	Msg     string `json:"msg"`
}

func registerDiagnostics(t *testing.T) {
	t.Helper()
	if _, loaded := diagnosticsRegistered.LoadOrStore(t.Name(), struct{}{}); loaded {
		return
	}
	t.Cleanup(func() {
		diagnosticsRegistered.Delete(t.Name())
		if t.Failed() {
			dumpDiagnostics(t)
		}
	})
}

func dumpDiagnostics(t *testing.T) {
	t.Helper()
	if testCluster == nil || testCluster.KubeconfigPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dumpKubectl(t, ctx, "nodes", "get", "nodes", "-o", "wide")
	dumpKubectl(t, ctx, "pods", "get", "pods", "-A", "-o", "wide")
	dumpKubectl(t, ctx, "events", "get", "events", "-A", "--sort-by=.lastTimestamp")
}

func dumpKubectl(t *testing.T, ctx context.Context, label string, args ...string) {
	t.Helper()
	allArgs := append([]string{"--kubeconfig", testCluster.KubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("[diagnostics] kubectl %s failed: %v\n%s", label, err, out)
		return
	}
	t.Logf("[diagnostics] kubectl %s:\n%s", label, out)
}

// waitAllReady blocks until NATS and Grafana are fully healthy. Call this at
// the start of every drain test so tests don't start against a degraded
// cluster from the previous test's rollout.
func waitAllReady(t *testing.T) {
	t.Helper()
	registerDiagnostics(t)
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
	registerDiagnostics(t)
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
	return getAnnotationInNamespace(t, framework.E2ENamespace, kind, name)
}

func getAnnotationInNamespace(t *testing.T, namespace, kind, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ann, err := framework.GetRestartAnnotation(ctx, testClient, namespace, kind, name)
	if err != nil {
		t.Fatalf("getAnnotation %s/%s/%s: %v", namespace, kind, name, err)
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

func assertRestartedInNamespace(t *testing.T, namespace, kind, name, before string) {
	t.Helper()
	after := getAnnotationInNamespace(t, namespace, kind, name)
	if after == before {
		t.Errorf("%s/%s/%s: restartedAt annotation did not change — workload was not restarted (value: %q)",
			namespace, kind, name, before)
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

func firstAgentNode(t *testing.T, ctx context.Context) string {
	t.Helper()
	registerDiagnostics(t)
	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) == 0 {
		t.Fatalf("no agent nodes: %v", err)
	}
	return agents[0]
}

func withOnlyNodeSchedulable(t *testing.T, ctx context.Context, target string, fn func()) {
	t.Helper()
	if err := framework.UncordonNode(ctx, testCluster.KubeconfigPath, target); err != nil {
		t.Fatalf("uncordon target before scheduling: %v", err)
	}

	nodes, err := testCluster.NodeNames(ctx)
	if err != nil {
		t.Fatalf("node names: %v", err)
	}
	var cordoned []string
	for _, node := range nodes {
		if node == target {
			continue
		}
		if err := framework.CordonNode(ctx, testCluster.KubeconfigPath, node); err != nil {
			t.Fatalf("cordon %s before scheduling: %v", node, err)
		}
		cordoned = append(cordoned, node)
	}
	defer func() {
		for _, node := range cordoned {
			if err := framework.UncordonNode(context.Background(), testCluster.KubeconfigPath, node); err != nil {
				t.Logf("uncordon %s after scheduling: %v (ignored)", node, err)
			}
		}
	}()

	fn()
}

func deployDeploymentsOnNode(t *testing.T, ctx context.Context, target, manifest string, names ...string) {
	t.Helper()
	refs := make([]deploymentRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, deploymentRef{namespace: framework.E2ENamespace, name: name})
	}
	deployManifestDeploymentsOnNode(t, ctx, target, manifest, refs...)
}

func deployManifestDeploymentsOnNode(t *testing.T, ctx context.Context, target, manifest string, refs ...deploymentRef) {
	t.Helper()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply test deployments: %v", err)
		}
		for _, ref := range refs {
			if err := framework.WaitForDeploymentReady(ctx, testClient, ref.namespace, ref.name, workloadReady); err != nil {
				t.Fatalf("deployment %s/%s not ready on target node: %v", ref.namespace, ref.name, err)
			}
			has, err := framework.NodeHasActivePodsWithSelector(ctx, testClient, target, ref.namespace, "app="+ref.name)
			if err != nil {
				t.Fatalf("check placement for %s/%s: %v", ref.namespace, ref.name, err)
			}
			if !has {
				t.Fatalf("deployment %s/%s did not schedule an active pod on %s", ref.namespace, ref.name, target)
			}
		}
	})
}

func simpleDeploymentManifest(name string, priority int) string {
	return framework.DeploymentManifest(framework.DeploymentManifestOptions{
		Name:     name,
		Priority: priority,
	})
}

func combinedManifest(parts ...string) string {
	return strings.Join(parts, "\n---\n")
}

func startIndex(output, subject string) int {
	for i, line := range strings.Split(output, "\n") {
		if strings.Contains(line, subject) && strings.Contains(line, " start ") {
			return i
		}
	}
	return -1
}

func parseJSONLogRecords(t *testing.T, stdout string) []jsonLogRecord {
	t.Helper()
	var records []jsonLogRecord
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec jsonLogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stdout line is not JSON: %v\nline: %s\nstdout: %s", err, line, stdout)
		}
		if rec.TS == "" || rec.Level == "" || rec.Subject == "" || rec.Msg == "" {
			t.Fatalf("JSON log record missing required fields: %+v", rec)
		}
		if _, err := time.Parse(time.RFC3339, rec.TS); err != nil {
			t.Fatalf("JSON log timestamp is not RFC3339: %q", rec.TS)
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		t.Fatalf("no JSON log records in stdout")
	}
	return records
}

func hasJSONRecord(records []jsonLogRecord, level, subjectContains, msgContains string) bool {
	for _, rec := range records {
		if level != "" && rec.Level != level {
			continue
		}
		if subjectContains != "" && !strings.Contains(rec.Subject, subjectContains) {
			continue
		}
		if msgContains != "" && !strings.Contains(rec.Msg, msgContains) {
			continue
		}
		return true
	}
	return false
}

func patchDeployment(t *testing.T, ctx context.Context, namespace, name string, patch []byte) {
	t.Helper()
	_, err := testClient.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("patch deployment %s/%s: %v", namespace, name, err)
	}
}

func setDeploymentMinReadySeconds(t *testing.T, ctx context.Context, namespace, name string, seconds int32) {
	t.Helper()
	patchDeployment(t, ctx, namespace, name, []byte(fmt.Sprintf(`{"spec":{"minReadySeconds":%d}}`, seconds)))
}

func waitForCheckpointEntry(t *testing.T, path, key string, timeout time.Duration) *drainpkg.Checkpoint {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		cp, err := drainpkg.LoadCheckpoint(path)
		if err != nil {
			lastErr = err
		} else if cp.Completed[key] {
			return cp
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("checkpoint %s did not contain %s within %s (lastErr=%v)", path, key, timeout, lastErr)
	return nil
}

func assertDrainRejectedBeforeCordon(t *testing.T, ctx context.Context, target string, want string, flags ...string) {
	t.Helper()
	result := testBinary.Drain(ctx, target, flags...)
	if result.Err == nil {
		t.Fatalf("drain should fail for flags %v\nstdout: %s\nstderr: %s", flags, result.Stdout, result.Stderr)
	}
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, want) {
		t.Fatalf("drain failure for flags %v missing %q\nerr: %v\nstdout: %s\nstderr: %s",
			flags, want, result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
}

func assertDrainNodesRejectedBeforeCordon(t *testing.T, ctx context.Context, nodes []string, want string, flags ...string) {
	t.Helper()
	result := testBinary.DrainNodes(ctx, nodes, flags...)
	if result.Err == nil {
		t.Fatalf("multi-node drain should fail for flags %v\nstdout: %s\nstderr: %s", flags, result.Stdout, result.Stderr)
	}
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, want) {
		t.Fatalf("multi-node drain failure for flags %v missing %q\nerr: %v\nstdout: %s\nstderr: %s",
			flags, want, result.Err, result.Stdout, result.Stderr)
	}
	for _, node := range nodes {
		verifyNodeNotCordoned(t, node)
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

// --------------------------------------------------------------------------
// TestDrain_Preflight_WarnMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_WarnMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	result := testBinary.Drain(ctx, target,
		"--preflight", "warn",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("warn mode must not abort: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)
	if !strings.Contains(result.Stdout, "RISK: single replica") {
		t.Errorf("warn mode output did not include single-replica risk\nstdout: %s", result.Stdout)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_StrictMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_StrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--rollout-timeout", "5m",
	)
	if result.Err == nil {
		t.Fatal("strict mode must exit non-zero when risk is found")
	}

	// Node must NOT be cordoned (drain aborted before cordon step).
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "RISK: single replica") ||
		!strings.Contains(combined, "downtime risk") {
		t.Errorf("strict mode output did not include expected preflight risk\nerr: %v\nstdout: %s",
			result.Err, result.Stdout)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_RecreateStrictMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_RecreateStrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.DeploymentManifest(framework.DeploymentManifestOptions{
		Name:     "recreate-risk",
		Replicas: 2,
		Priority: 100,
		Recreate: true,
	})
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "recreate-risk")

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--rollout-timeout", "5m",
	)
	if result.Err == nil {
		t.Fatal("strict preflight must abort on Recreate strategy")
	}
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "Recreate strategy") ||
		!strings.Contains(combined, "guaranteed downtime") {
		t.Fatalf("strict preflight output missing Recreate risk\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_ProfileConfigAndCLIOverride
// --------------------------------------------------------------------------

func TestDrain_ProfileConfigAndCLIOverride(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	configPath := filepath.Join(t.TempDir(), "safed.yaml")
	configData := []byte(`profiles:
  risky:
    preflight: strict
    rollout-timeout: 5m
    poll-interval: 1s
`)
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	strict := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--profile", "risky",
	)
	if strict.Err == nil {
		t.Fatal("profile preflight=strict must abort on single-replica worker")
	}
	verifyNodeNotCordoned(t, target)

	result := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--profile", "risky",
		"--preflight", "off",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("CLI --preflight=off should override strict profile: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)
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
// TestDrain_NodeSelectorErrors
// --------------------------------------------------------------------------

func TestDrain_NodeSelectorErrors(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	noMatch := testBinary.DrainWithSelector(ctx, "safed-e2e-no-such-label=true")
	if noMatch.Err == nil {
		t.Fatal("selector with no matching nodes must fail")
	}
	verifyNodeNotCordoned(t, target)

	invalid := testBinary.DrainWithSelector(ctx, "safed-e2e-invalid in (")
	if invalid.Err == nil {
		t.Fatal("invalid selector must fail")
	}
	verifyNodeNotCordoned(t, target)

	both := testBinary.DrainRaw(ctx, target, "--selector", "safed-e2e-target=true")
	if both.Err == nil {
		t.Fatal("positional node plus --selector must fail")
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_InvalidOptions
// --------------------------------------------------------------------------

func TestDrain_InvalidOptions(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	configPath := filepath.Join(t.TempDir(), "invalid-profile.yaml")
	configData := []byte(`profiles:
  invalid:
    preflight: maybe
    rollout-timeout: 5m
`)
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatalf("write invalid profile config: %v", err)
	}

	tests := []struct {
		name  string
		flags []string
		want  string
	}{
		{
			name:  "invalid preflight",
			flags: []string{"--preflight", "maybe"},
			want:  `invalid --preflight "maybe"`,
		},
		{
			name:  "invalid profile preflight",
			flags: []string{"--config", configPath, "--profile", "invalid"},
			want:  `invalid --preflight "maybe"`,
		},
		{
			name:  "invalid log format",
			flags: []string{"--log-format", "yaml"},
			want:  `invalid --log-format "yaml"`,
		},
		{
			name:  "negative max concurrency",
			flags: []string{"--max-concurrency=-1"},
			want:  "--max-concurrency must be >= 0",
		},
		{
			name:  "negative node concurrency",
			flags: []string{"--node-concurrency=-1"},
			want:  "--node-concurrency must be >= 0",
		},
		{
			name:  "negative rollout timeout",
			flags: []string{"--rollout-timeout=-1s"},
			want:  "--rollout-timeout must be >= 0",
		},
		{
			name:  "negative timeout",
			flags: []string{"--timeout=-1s"},
			want:  "--timeout must be >= 0",
		},
		{
			name:  "negative pod vacate timeout",
			flags: []string{"--pod-vacate-timeout=-1s"},
			want:  "--pod-vacate-timeout must be >= 0",
		},
		{
			name:  "negative eviction timeout",
			flags: []string{"--eviction-timeout=-1s"},
			want:  "--eviction-timeout must be >= 0",
		},
		{
			name:  "negative pdb retry interval",
			flags: []string{"--pdb-retry-interval=-1s"},
			want:  "--pdb-retry-interval must be >= 0",
		},
		{
			name:  "negative poll interval",
			flags: []string{"--poll-interval=-1s"},
			want:  "--poll-interval must be >= 0",
		},
		{
			name:  "invalid grace period",
			flags: []string{"--grace-period=-2"},
			want:  "--grace-period must be -1 or >= 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertDrainRejectedBeforeCordon(t, ctx, target, tc.want, tc.flags...)
		})
	}

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) < 2 {
		t.Skip("need at least 2 agent nodes for multi-node checkpoint validation")
	}
	for _, agent := range agents[:2] {
		uncordon(t, agent)
		defer uncordon(t, agent)
	}
	assertDrainNodesRejectedBeforeCordon(t, ctx, agents[:2],
		"--checkpoint-path can only be used when draining a single node",
		"--checkpoint-path", filepath.Join(t.TempDir(), "shared-checkpoint.json"),
	)
}

// --------------------------------------------------------------------------
// TestDrain_EmitEvents
// --------------------------------------------------------------------------

func TestDrain_EmitEvents(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := simpleDeploymentManifest("events-workload", 100)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "events-workload")

	since := time.Now().Add(-1 * time.Second)
	result := testBinary.Drain(ctx, target,
		"--emit-events",
		"--preflight", "off",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("drain with --emit-events failed: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}

	// Poll for current-run node and workload events.
	deadline := time.Now().Add(30 * time.Second)
	var foundNode, foundWorkload bool
	for !(foundNode && foundWorkload) && time.Now().Before(deadline) {
		nodeEvents, err := framework.EventsForNode(ctx, testClient, target)
		if err != nil {
			t.Fatalf("list node events: %v", err)
		}
		for _, e := range nodeEvents {
			if (e.Reason == "Draining" || e.Reason == "Drained") &&
				e.CreationTimestamp.Time.After(since) {
				foundNode = true
				break
			}
		}

		workloadEvents, err := framework.EventsForObject(ctx, testClient, framework.E2ENamespace,
			"Deployment", "events-workload")
		if err != nil {
			t.Fatalf("list workload events: %v", err)
		}
		for _, e := range workloadEvents {
			if (e.Reason == "RollingRestartTriggered" || e.Reason == "RollingRestartComplete") &&
				e.CreationTimestamp.Time.After(since) {
				foundWorkload = true
				break
			}
		}

		if !(foundNode && foundWorkload) {
			time.Sleep(2 * time.Second)
		}
	}
	if !foundNode {
		t.Errorf("no current Draining/Drained event on node %s after --emit-events", target)
	}
	if !foundWorkload {
		t.Errorf("no current rolling restart event on Deployment/events-workload after --emit-events")
	}
}

// --------------------------------------------------------------------------
// TestDrain_MultiNamespace
// --------------------------------------------------------------------------

func TestDrain_MultiNamespace(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := framework.EnsureNamespace(ctx, testClient, secondaryNamespace); err != nil {
		t.Fatalf("ensure secondary namespace: %v", err)
	}

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		framework.DeploymentManifest(framework.DeploymentManifestOptions{
			Name:      "multi-ns-primary",
			Namespace: framework.E2ENamespace,
			Priority:  100,
		}),
		framework.DeploymentManifest(framework.DeploymentManifestOptions{
			Name:      "multi-ns-secondary",
			Namespace: secondaryNamespace,
			Priority:  100,
		}),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployManifestDeploymentsOnNode(t, ctx, target, manifest,
		deploymentRef{namespace: framework.E2ENamespace, name: "multi-ns-primary"},
		deploymentRef{namespace: secondaryNamespace, name: "multi-ns-secondary"},
	)

	beforePrimary := getAnnotationInNamespace(t, framework.E2ENamespace, "Deployment", "multi-ns-primary")
	beforeSecondary := getAnnotationInNamespace(t, secondaryNamespace, "Deployment", "multi-ns-secondary")
	since := time.Now().Add(-1 * time.Second)

	result := testBinary.Drain(ctx, target,
		"--emit-events",
		"--preflight", "off",
		"--rollout-timeout", "5m",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("multi-namespace drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}

	assertRestartedInNamespace(t, framework.E2ENamespace, "Deployment", "multi-ns-primary", beforePrimary)
	assertRestartedInNamespace(t, secondaryNamespace, "Deployment", "multi-ns-secondary", beforeSecondary)

	deadline := time.Now().Add(30 * time.Second)
	var foundPrimary, foundSecondary bool
	for !(foundPrimary && foundSecondary) && time.Now().Before(deadline) {
		primaryEvents, err := framework.EventsForObject(ctx, testClient, framework.E2ENamespace, "Deployment", "multi-ns-primary")
		if err != nil {
			t.Fatalf("list primary events: %v", err)
		}
		for _, e := range primaryEvents {
			if e.Reason == "RollingRestartComplete" && e.CreationTimestamp.Time.After(since) {
				foundPrimary = true
				break
			}
		}

		secondaryEvents, err := framework.EventsForObject(ctx, testClient, secondaryNamespace, "Deployment", "multi-ns-secondary")
		if err != nil {
			t.Fatalf("list secondary events: %v", err)
		}
		for _, e := range secondaryEvents {
			if e.Reason == "RollingRestartComplete" && e.CreationTimestamp.Time.After(since) {
				foundSecondary = true
				break
			}
		}

		if !(foundPrimary && foundSecondary) {
			time.Sleep(2 * time.Second)
		}
	}
	if !foundPrimary || !foundSecondary {
		t.Fatalf("missing current workload events: primary=%v secondary=%v", foundPrimary, foundSecondary)
	}
}

// --------------------------------------------------------------------------
// TestDrain_JSONLogFormat
// --------------------------------------------------------------------------

func TestDrain_JSONLogFormat(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := simpleDeploymentManifest("json-workload", 100)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "json-workload")

	result := testBinary.Drain(ctx, target,
		"--log-format", "json",
		"--preflight", "off",
		"--rollout-timeout", "5m",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("json log drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	records := parseJSONLogRecords(t, result.Stdout)
	if !hasJSONRecord(records, "start", "Deployment/e2e/json-workload", "Rolling restart") {
		t.Fatalf("missing JSON start record for workload\nstdout: %s", result.Stdout)
	}
	if !hasJSONRecord(records, "done", target, "Drained") {
		t.Fatalf("missing JSON drained record for node\nstdout: %s", result.Stdout)
	}
}

// --------------------------------------------------------------------------
// TestDrain_JSONLogFormatFailure
// --------------------------------------------------------------------------

func TestDrain_JSONLogFormatFailure(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	result := testBinary.Drain(ctx, target,
		"--log-format", "json",
		"--preflight", "strict",
		"--rollout-timeout", "5m",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("strict preflight with JSON logs must fail")
	}
	records := parseJSONLogRecords(t, result.Stdout)
	if !hasJSONRecord(records, "warn", "Deployment/e2e/worker", "RISK: single replica") {
		t.Fatalf("missing JSON warning record for strict preflight\nstdout: %s", result.Stdout)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_DaemonSetNotRestarted
// --------------------------------------------------------------------------

func TestDrain_DaemonSetNotRestarted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.DaemonSetManifest); err != nil {
		t.Fatalf("apply daemonset: %v", err)
	}
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.DaemonSetManifest)
	}()

	waitAllReady(t)

	result := testBinary.Drain(ctx, target, "--preflight", "off", "--rollout-timeout", "5m")
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

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("resume-skip", 10),
		simpleDeploymentManifest("resume-run", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "resume-skip", "resume-run")

	cpPath := filepath.Join(t.TempDir(), "checkpoint.json")
	cpData := fmt.Sprintf(`{
  "nodeName": %q,
  "context": "",
  "completed": {
    "Deployment/e2e/resume-skip": true
  }
}`, target)
	if err := os.WriteFile(cpPath, []byte(cpData), 0600); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}

	beforeSkip := getAnnotation(t, "Deployment", "resume-skip")
	beforeRun := getAnnotation(t, "Deployment", "resume-run")

	wrongCPPath := filepath.Join(t.TempDir(), "wrong-node.json")
	wrongCPData := `{
  "nodeName": "not-the-target-node",
  "context": "",
  "completed": {
    "Deployment/e2e/resume-skip": true
  }
}`
	if err := os.WriteFile(wrongCPPath, []byte(wrongCPData), 0600); err != nil {
		t.Fatalf("write wrong-node checkpoint: %v", err)
	}
	wrong := testBinary.Drain(ctx, target,
		"--resume",
		"--checkpoint-path", wrongCPPath,
		"--preflight", "off",
		"--rollout-timeout", "5m",
	)
	if wrong.Err == nil {
		t.Fatal("resume with a checkpoint for the wrong node must fail")
	}
	wrongCombined := wrong.Stdout + wrong.Stderr + wrong.Err.Error()
	if !strings.Contains(wrongCombined, "checkpoint is for node") {
		t.Fatalf("wrong-node checkpoint failed with unexpected error: %v\nstdout: %s\nstderr: %s",
			wrong.Err, wrong.Stdout, wrong.Stderr)
	}
	if err := framework.UncordonNode(ctx, testCluster.KubeconfigPath, target); err != nil {
		t.Fatalf("uncordon after wrong-node checkpoint: %v", err)
	}

	result := testBinary.Drain(ctx, target,
		"--resume",
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--rollout-timeout", "5m",
	)
	if result.Err != nil {
		t.Fatalf("resume drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)

	assertNotRestarted(t, "Deployment", "resume-skip", beforeSkip)
	assertRestarted(t, "Deployment", "resume-run", beforeRun)
	if !strings.Contains(result.Stdout, "Skipping Deployment/e2e/resume-skip (already completed per checkpoint)") {
		t.Errorf("resume output did not show checkpoint skip\nstdout: %s", result.Stdout)
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Errorf("checkpoint should be deleted after successful resume, stat err: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_CheckpointResumeAfterProcessKill
// --------------------------------------------------------------------------

func TestDrain_CheckpointResumeAfterProcessKill(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("interrupt-done", 10),
		simpleDeploymentManifest("interrupt-pending", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "interrupt-done", "interrupt-pending")
	setDeploymentMinReadySeconds(t, ctx, framework.E2ENamespace, "interrupt-pending", 25)

	cpPath := filepath.Join(t.TempDir(), "checkpoint.json")
	proc, err := testBinary.StartDrain(ctx, target,
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--rollout-timeout", "2m",
		"--poll-interval", "1s",
		"--max-concurrency", "1",
	)
	if err != nil {
		t.Fatalf("start drain: %v", err)
	}

	cp := waitForCheckpointEntry(t, cpPath, "Deployment/e2e/interrupt-done", 90*time.Second)
	if cp.Completed["Deployment/e2e/interrupt-pending"] {
		t.Fatalf("pending workload completed before interruption: %+v", cp.Completed)
	}
	_ = proc.Kill()
	killed := proc.Wait()
	if killed.Err == nil {
		t.Fatalf("interrupted drain exited successfully before it could be killed\nstdout: %s\nstderr: %s",
			killed.Stdout, killed.Stderr)
	}

	afterKillDone := getAnnotation(t, "Deployment", "interrupt-done")
	afterKillPending := getAnnotation(t, "Deployment", "interrupt-pending")
	time.Sleep(1100 * time.Millisecond)

	result := testBinary.Drain(ctx, target,
		"--resume",
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--rollout-timeout", "2m",
		"--poll-interval", "1s",
		"--max-concurrency", "1",
	)
	if result.Err != nil {
		t.Fatalf("resume after killed drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)

	if after := getAnnotation(t, "Deployment", "interrupt-done"); after != afterKillDone {
		t.Fatalf("completed workload should not restart on resume: before=%q after=%q", afterKillDone, after)
	}
	assertRestarted(t, "Deployment", "interrupt-pending", afterKillPending)
	if !strings.Contains(result.Stdout, "Skipping Deployment/e2e/interrupt-done (already completed per checkpoint)") {
		t.Fatalf("resume output did not show checkpoint skip\nstdout: %s", result.Stdout)
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Fatalf("checkpoint should be deleted after successful resume, stat err: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_GlobalTimeoutKeepsCheckpointAndUncordons
// --------------------------------------------------------------------------

func TestDrain_GlobalTimeoutKeepsCheckpointAndUncordons(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		simpleDeploymentManifest("timeout-done", 10),
		simpleDeploymentManifest("timeout-slow", 100),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "timeout-done", "timeout-slow")
	setDeploymentMinReadySeconds(t, ctx, framework.E2ENamespace, "timeout-slow", 90)

	cpPath := filepath.Join(t.TempDir(), "checkpoint.json")
	result := testBinary.Drain(ctx, target,
		"--timeout", "25s",
		"--uncordon-on-failure",
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--rollout-timeout", "5m",
		"--poll-interval", "1s",
		"--max-concurrency", "1",
	)
	if result.Err == nil {
		t.Fatal("drain should fail when the global --timeout expires")
	}
	verifyNodeNotCordoned(t, target)

	cp, err := drainpkg.LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatalf("load checkpoint after timeout: %v", err)
	}
	if !cp.Completed["Deployment/e2e/timeout-done"] {
		t.Fatalf("completed workload was not preserved in checkpoint after timeout: %+v", cp.Completed)
	}
	if cp.Completed["Deployment/e2e/timeout-slow"] {
		t.Fatalf("timed-out workload should not be marked complete: %+v", cp.Completed)
	}
}

// --------------------------------------------------------------------------
// TestDrain_MultiNode
// --------------------------------------------------------------------------

// TestDrain_MultiNode drains both agent nodes in parallel and verifies a
// workload on one of them is restarted while both nodes end up cordoned.
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

	manifest := simpleDeploymentManifest("multi-node", 100)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, agents[0], manifest, "multi-node")

	before := getAnnotation(t, "Deployment", "multi-node")

	result := testBinary.DrainNodes(ctx, agents,
		"--preflight", "off",
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

	assertRestarted(t, "Deployment", "multi-node", before)

	if err := framework.WaitForDeploymentReady(ctx, testClient, framework.E2ENamespace, "multi-node", workloadReady); err != nil {
		t.Fatalf("test workload not healthy after multi-node drain: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestDrain_UnmanagedPodEvictionOptions
// --------------------------------------------------------------------------

func TestDrain_UnmanagedPodEvictionOptions(t *testing.T) {
	waitAllReady(t)

	tests := []struct {
		name          string
		manifest      string
		labelSelector string
		flags         []string
		wantErr       bool
		wantText      string
	}{
		{
			name:          "standalone requires force",
			manifest:      framework.StandalonePodManifest(framework.E2ENamespace, "standalone-blocked", false),
			labelSelector: "app=standalone-blocked",
			wantErr:       true,
			wantText:      "standalone pods require --force",
		},
		{
			name:          "standalone evicted with force",
			manifest:      framework.StandalonePodManifest(framework.E2ENamespace, "standalone-force", false),
			labelSelector: "app=standalone-force",
			flags:         []string{"--force"},
		},
		{
			name:          "standalone force deleted",
			manifest:      framework.StandalonePodManifest(framework.E2ENamespace, "standalone-delete", false),
			labelSelector: "app=standalone-delete",
			flags:         []string{"--force-delete-standalone"},
			wantText:      "Force-deleted (standalone)",
		},
		{
			name:          "Job pod requires force",
			manifest:      framework.JobManifest(framework.E2ENamespace, "job-blocked"),
			labelSelector: "app=job-blocked",
			wantErr:       true,
			wantText:      "Job-owned pods require --force",
		},
		{
			name:          "Job pod evicted with force",
			manifest:      framework.JobManifest(framework.E2ENamespace, "job-force"),
			labelSelector: "app=job-force",
			flags:         []string{"--force"},
		},
		{
			name:          "emptyDir requires delete flag",
			manifest:      framework.ReplicaSetManifest(framework.E2ENamespace, "emptydir-blocked", true),
			labelSelector: "app=emptydir-blocked",
			wantErr:       true,
			wantText:      "emptyDir pods require --delete-emptydir-data or --force",
		},
		{
			name:          "emptyDir evicted with delete flag",
			manifest:      framework.ReplicaSetManifest(framework.E2ENamespace, "emptydir-delete", true),
			labelSelector: "app=emptydir-delete",
			flags:         []string{"--delete-emptydir-data"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registerDiagnostics(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			target := firstAgentNode(t, ctx)
			defer uncordon(t, target)
			defer func() {
				_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, tc.manifest)
			}()

			withOnlyNodeSchedulable(t, ctx, target, func() {
				if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, tc.manifest); err != nil {
					t.Fatalf("apply unmanaged workload: %v", err)
				}
				got, err := framework.NodeWithPodForLabel(ctx, testClient, framework.E2ENamespace, tc.labelSelector, 60*time.Second)
				if err != nil {
					t.Fatalf("pod did not start: %v", err)
				}
				if got != target {
					t.Fatalf("pod scheduled on %s, want %s", got, target)
				}
			})

			flags := append([]string{
				"--preflight", "off",
				"--poll-interval", "1s",
				"--eviction-timeout", "30s",
				"--pdb-retry-interval", "1s",
			}, tc.flags...)
			result := testBinary.Drain(ctx, target, flags...)
			if tc.wantErr && result.Err == nil {
				t.Fatalf("drain should fail\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
			}
			if !tc.wantErr && result.Err != nil {
				t.Fatalf("drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
			}
			if tc.wantText != "" {
				combined := result.Stdout + result.Stderr
				if result.Err != nil {
					combined += result.Err.Error()
				}
				if !strings.Contains(combined, tc.wantText) {
					t.Fatalf("output missing %q\nerr: %v\nstdout: %s\nstderr: %s",
						tc.wantText, result.Err, result.Stdout, result.Stderr)
				}
			}
			verifyNodeCordoned(t, target)
			if !tc.wantErr {
				if err := framework.WaitForNoActivePodsWithSelectorOnNode(ctx, testClient, target,
					framework.E2ENamespace, tc.labelSelector, 60*time.Second); err != nil {
					t.Fatalf("pod still active on drained node: %v", err)
				}
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestDrain_PDBAllowedEviction
// --------------------------------------------------------------------------

func TestDrain_PDBAllowedEviction(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := combinedManifest(
		framework.ReplicaSetManifest(framework.E2ENamespace, "pdb-allowed", false),
		framework.PDBManifest(framework.E2ENamespace, "pdb-allowed", "pdb-allowed", 1),
	)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply PDB-allowed pod: %v", err)
		}
		got, err := framework.NodeWithPodForLabel(ctx, testClient, framework.E2ENamespace, "app=pdb-allowed", 60*time.Second)
		if err != nil {
			t.Fatalf("PDB pod did not start: %v", err)
		}
		if got != target {
			t.Fatalf("PDB pod scheduled on %s, want %s", got, target)
		}
	})

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--eviction-timeout", "30s",
		"--pdb-retry-interval", "1s",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("PDB-allowed drain failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeCordoned(t, target)
	if !strings.Contains(result.Stdout, "Evicted") {
		t.Fatalf("drain output did not show eviction\nstdout: %s", result.Stdout)
	}
	if err := framework.WaitForNoActivePodsWithSelectorOnNode(ctx, testClient, target,
		framework.E2ENamespace, "app=pdb-allowed", 60*time.Second); err != nil {
		t.Fatalf("PDB pod still active on drained node after eviction: %v", err)
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

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.StandalonePodWithPDBManifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.StandalonePodWithPDBManifest); err != nil {
			t.Fatalf("apply standalone pod + PDB: %v", err)
		}
		got, err := framework.NodeWithPodForLabel(ctx, testClient, framework.E2ENamespace,
			framework.StandalonePDBPodSelector, 60*time.Second)
		if err != nil {
			t.Fatalf("standalone pod not running: %v", err)
		}
		if got != target {
			t.Fatalf("standalone pod scheduled on %s, want %s", got, target)
		}
	})

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

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.CrashingDeploymentManifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.CrashingDeploymentManifest); err != nil {
			t.Fatalf("apply crashing deployment: %v", err)
		}
		got, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
			framework.CrasherPodSelector, 90*time.Second)
		if err != nil {
			t.Fatalf("crashing pod did not crash within timeout: %v", err)
		}
		if got != target {
			t.Fatalf("crashing pod scheduled on %s, want %s", got, target)
		}
	})

	result := testBinary.Drain(ctx, target, "--rollout-timeout", "3m")
	if result.Err == nil {
		t.Fatal("drain should fail fast when a pod is in CrashLoopBackOff")
	}
}

// --------------------------------------------------------------------------
// TestDrain_ImagePullAbort — drain aborts fast on ImagePullBackOff
// --------------------------------------------------------------------------

func TestDrain_ImagePullAbort(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := simpleDeploymentManifest("imagepull-bad", 100)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "imagepull-bad")

	patchDeployment(t, ctx, framework.E2ENamespace, "imagepull-bad", []byte(`{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "app",
            "image": "127.0.0.1:9/k8s-safed/missing:latest"
          }
        ]
      }
    }
  }
}`))

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--rollout-timeout", "2m",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("drain should fail fast when a rollout pod cannot pull its image")
	}
	verifyNodeCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "ImagePullBackOff") && !strings.Contains(combined, "ErrImagePull") {
		t.Fatalf("image pull failure output missing expected reason\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
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

	target := firstAgentNode(t, ctx)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, framework.CrashingDeploymentManifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.CrashingDeploymentManifest); err != nil {
			t.Fatalf("apply crashing deployment: %v", err)
		}
		got, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
			framework.CrasherPodSelector, 90*time.Second)
		if err != nil {
			t.Fatalf("crashing pod did not crash within timeout: %v", err)
		}
		if got != target {
			t.Fatalf("crashing pod scheduled on %s, want %s", got, target)
		}
	})
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
