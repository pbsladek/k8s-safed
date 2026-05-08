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

	manifest := crashingDeploymentManifest("crasher-abort")
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply crashing deployment: %v", err)
		}
		got, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
			"app=crasher-abort", 90*time.Second)
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
		cleanupManifest(t, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "imagepull-bad")

	patchDeployment(t, ctx, framework.E2ENamespace, "imagepull-bad", mustJSONPatch(t, map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]string{{
						"name":  "app",
						"image": "127.0.0.1:9/k8s-safed/missing:latest",
					}},
				},
			},
		},
	}))

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
	manifest := crashingDeploymentManifest("crasher-uncordon")
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply crashing deployment: %v", err)
		}
		got, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
			"app=crasher-uncordon", 90*time.Second)
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

// --------------------------------------------------------------------------
// TestDrain_AlreadyCordonedFailureDoesNotUncordon
// --------------------------------------------------------------------------

func TestDrain_AlreadyCordonedFailureDoesNotUncordon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	manifest := crashingDeploymentManifest("crasher-already-cordoned")
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply crashing deployment: %v", err)
		}
		got, err := framework.WaitForCrashingPod(ctx, testClient, framework.E2ENamespace,
			"app=crasher-already-cordoned", 90*time.Second)
		if err != nil {
			t.Fatalf("crashing pod did not crash within timeout: %v", err)
		}
		if got != target {
			t.Fatalf("crashing pod scheduled on %s, want %s", got, target)
		}
	})

	if err := framework.CordonNode(ctx, testCluster.KubeconfigPath, target); err != nil {
		t.Fatalf("cordon target before drain: %v", err)
	}
	defer uncordon(t, target)

	result := testBinary.Drain(ctx, target,
		"--rollout-timeout", "3m",
		"--uncordon-on-failure",
	)
	if result.Err == nil {
		t.Fatal("drain should fail on CrashLoopBackOff")
	}
	verifyNodeCordoned(t, target)

	if !strings.Contains(result.Stdout+result.Stderr, "--uncordon-on-failure has no effect") {
		t.Fatalf("output missing already-cordoned uncordon warning\nstdout: %s\nstderr: %s",
			result.Stdout, result.Stderr)
	}
}

func crashingDeploymentManifest(name string) string {
	return framework.DeploymentManifest(framework.DeploymentManifestOptions{
		Namespace: framework.E2ENamespace,
		Name:      name,
		Priority:  100,
		Command:   "exit 1",
	})
}
