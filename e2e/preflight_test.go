//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

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
		cleanupManifest(t, framework.WorkerManifest)
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
		cleanupManifest(t, framework.WorkerManifest)
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
		cleanupManifest(t, manifest)
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
// TestDrain_Preflight_PausedDeploymentStrictMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_PausedDeploymentStrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.DeploymentManifest(framework.DeploymentManifestOptions{
		Name:     "paused-risk",
		Replicas: 2,
		Priority: 100,
	})
	defer func() {
		cleanupManifest(t, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "paused-risk")
	patchDeployment(t, ctx, framework.E2ENamespace, "paused-risk", mustJSONPatch(t, map[string]any{
		"spec": map[string]any{"paused": true},
	}))

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--only-workload", "Deployment/e2e/paused-risk",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("strict preflight must abort on paused Deployment")
	}
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "paused Deployment") {
		t.Fatalf("strict preflight output missing paused Deployment risk\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_StatefulSetSingleReplicaStrictMode
// --------------------------------------------------------------------------

func TestDrain_Preflight_StatefulSetSingleReplicaStrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.StatefulSetManifest(framework.StatefulSetManifestOptions{
		Name:     "single-sts",
		Priority: 100,
		Replicas: 1,
	})
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply single-replica StatefulSet: %v", err)
		}
		if err := framework.WaitForStatefulSetReady(ctx, testClient, framework.E2ENamespace, "single-sts", workloadReady); err != nil {
			t.Fatalf("single-replica StatefulSet not ready on target node: %v", err)
		}
	})

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--only-workload", "StatefulSet/e2e/single-sts",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("strict preflight must abort on single-replica StatefulSet")
	}
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "single replica StatefulSet") {
		t.Fatalf("strict preflight output missing StatefulSet risk\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

func TestDrain_Preflight_StatefulSetOnDeleteStrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.StatefulSetManifest(framework.StatefulSetManifestOptions{
		Name:     "ondelete-sts",
		Priority: 100,
		Replicas: 2,
		OnDelete: true,
	})
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply OnDelete StatefulSet: %v", err)
		}
		if err := framework.WaitForStatefulSetReady(ctx, testClient, framework.E2ENamespace, "ondelete-sts", workloadReady); err != nil {
			t.Fatalf("OnDelete StatefulSet not ready on target node: %v", err)
		}
	})

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--only-workload", "StatefulSet/e2e/ondelete-sts",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("strict preflight must abort on OnDelete StatefulSet")
	}
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "OnDelete update strategy") {
		t.Fatalf("strict preflight output missing OnDelete risk\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

func TestDrain_Preflight_StatefulSetPartitionStrictMode(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	partition := int32(1)
	manifest := framework.StatefulSetManifest(framework.StatefulSetManifestOptions{
		Name:      "partition-sts",
		Priority:  100,
		Replicas:  2,
		Partition: &partition,
	})
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply partitioned StatefulSet: %v", err)
		}
		if err := framework.WaitForStatefulSetReady(ctx, testClient, framework.E2ENamespace, "partition-sts", workloadReady); err != nil {
			t.Fatalf("partitioned StatefulSet not ready on target node: %v", err)
		}
	})

	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--only-workload", "StatefulSet/e2e/partition-sts",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("strict preflight must abort on partitioned StatefulSet")
	}
	verifyNodeNotCordoned(t, target)
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "partitioned StatefulSet rollout") {
		t.Fatalf("strict preflight output missing partition risk\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_Preflight_PDBZeroDisruptionsNote
// --------------------------------------------------------------------------

func TestDrain_Preflight_PDBZeroDisruptionsNote(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		cleanupManifest(t, framework.BlockingPDBManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.BlockingPDBManifest, "pdb-target")

	result := testBinary.Drain(ctx, target,
		"--dry-run",
		"--preflight", "warn",
		"--only-workload", "Deployment/e2e/pdb-target",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("PDB preflight dry-run failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
	if !strings.Contains(result.Stdout, "PodDisruptionBudget/e2e/pdb-target") ||
		!strings.Contains(result.Stdout, "0 disruptions currently allowed") {
		t.Fatalf("PDB preflight output missing zero-disruptions note\nstdout: %s", result.Stdout)
	}
}
