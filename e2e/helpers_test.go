//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
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

func mustJSONPatch(t *testing.T, patch any) []byte {
	t.Helper()
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	return data
}

func setDeploymentMinReadySeconds(t *testing.T, ctx context.Context, namespace, name string, seconds int32) {
	t.Helper()
	patchDeployment(t, ctx, namespace, name, mustJSONPatch(t, map[string]any{
		"spec": map[string]any{"minReadySeconds": seconds},
	}))
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

func waitForPodWithSelectorOnNode(t *testing.T, ctx context.Context, nodeName, namespace, labelSelector string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		pods, err := testClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
			LabelSelector: labelSelector,
		})
		if err != nil {
			t.Fatalf("list pods with %q on %s: %v", labelSelector, nodeName, err)
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == "Running" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no running pod with %q on %s within %s", labelSelector, nodeName, timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for pod with %q on %s: %v", labelSelector, nodeName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
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
