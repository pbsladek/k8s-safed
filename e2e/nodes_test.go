//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

// --------------------------------------------------------------------------
// TestDrain_MultiNodeRejectsCheckpointPath
// --------------------------------------------------------------------------

func TestDrain_MultiNodeRejectsCheckpointPath(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	agents, err := testCluster.AgentNodeNames(ctx)
	if err != nil || len(agents) < 2 {
		t.Skip("need at least 2 agent nodes for checkpoint-path validation")
	}
	for _, node := range agents[:2] {
		uncordon(t, node)
		defer uncordon(t, node)
	}

	result := testBinary.DrainNodes(ctx, agents[:2],
		"--checkpoint-path", filepath.Join(t.TempDir(), "checkpoint.json"),
	)
	if result.Err == nil {
		t.Fatal("multi-node drain with --checkpoint-path must fail")
	}
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "--checkpoint-path can only be used when draining a single node") {
		t.Fatalf("output missing checkpoint path validation\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	for _, node := range agents[:2] {
		verifyNodeNotCordoned(t, node)
	}
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
	patch := mustJSONPatch(t, map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{labelKey: "true"},
		},
	})
	if _, err := testClient.CoreV1().Nodes().Patch(
		ctx, target, "application/merge-patch+json", patch, metav1.PatchOptions{},
	); err != nil {
		t.Fatalf("label node: %v", err)
	}
	defer func() {
		removePatch := mustJSONPatch(t, map[string]any{
			"metadata": map[string]any{
				"labels": map[string]any{labelKey: nil},
			},
		})
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
// TestDrain_MultiNodePartialFailureUncordonsFailedNodeOnly
// --------------------------------------------------------------------------

func TestDrain_MultiNodePartialFailureUncordonsFailedNodeOnly(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	result := testBinary.DrainNodes(ctx, []string{target, "safed-e2e-missing-node"},
		"--preflight", "off",
		"--node-concurrency", "2",
		"--uncordon-on-failure",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("multi-node drain should fail when one target node does not exist")
	}
	verifyNodeNotCordoned(t, target)

	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "safed-e2e-missing-node") {
		t.Fatalf("multi-node partial failure output missing missing-node context\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}
