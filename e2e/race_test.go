//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

// --------------------------------------------------------------------------
// TestDrain_StaleReplicaSetOwnerPodIsSkippedAsWorkload
// --------------------------------------------------------------------------

func TestDrain_StaleReplicaSetOwnerPodIsSkippedAsWorkload(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.StandalonePodManifest(framework.E2ENamespace, "race-stale-rs-pod", false)
	defer func() {
		cleanupManifest(t, manifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply stale-owner pod: %v", err)
		}
		waitForPodWithSelectorOnNode(t, ctx, target, framework.E2ENamespace, "app=race-stale-rs-pod", workloadReady)
	})
	patchPodOwnerReferences(t, ctx, framework.E2ENamespace, "race-stale-rs-pod", "race-deleted-rs")

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--dry-run",
		"--only-workload", "Deployment/e2e/does-not-exist",
		"--force",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("drain should tolerate a pod whose ReplicaSet owner disappeared: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, "race-deleted-rs") {
		t.Fatalf("drain should not surface missing ReplicaSet owner as a workload\nstdout: %s\nstderr: %s",
			result.Stdout, result.Stderr)
	}
	if !strings.Contains(combined, "Dry-run complete") {
		t.Fatalf("stale owner drain output missing dry-run completion\nstdout: %s\nstderr: %s",
			result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_TerminatingPodIsSkippedDuringEviction
// --------------------------------------------------------------------------

func TestDrain_TerminatingPodIsSkippedDuringEviction(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.StandalonePodManifest(framework.E2ENamespace, "race-terminating-pod", false)
	t.Cleanup(func() {
		grace := int64(0)
		_ = testClient.CoreV1().Pods(framework.E2ENamespace).Delete(context.Background(), "race-terminating-pod", metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
		cleanupManifest(t, manifest)
	})
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, manifest); err != nil {
			t.Fatalf("apply terminating pod: %v", err)
		}
		waitForPodWithSelectorOnNode(t, ctx, target, framework.E2ENamespace, "app=race-terminating-pod", workloadReady)
	})

	grace := int64(60)
	if err := testClient.CoreV1().Pods(framework.E2ENamespace).Delete(ctx, "race-terminating-pod", metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil {
		t.Fatalf("delete pod with grace period: %v", err)
	}
	waitForPodDeletionTimestamp(t, ctx, framework.E2ENamespace, "race-terminating-pod", 30*time.Second)

	result := testBinary.Drain(ctx, target,
		"--preflight", "off",
		"--dry-run",
		"--only-workload", "Deployment/e2e/does-not-exist",
		"--force",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("drain should skip already-terminating pods: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	if strings.Contains(result.Stdout+result.Stderr, "Pod/e2e/race-terminating-pod") {
		t.Fatalf("terminating pod should be skipped from eviction output\nstdout: %s\nstderr: %s",
			result.Stdout, result.Stderr)
	}
}

func patchPodOwnerReferences(t *testing.T, ctx context.Context, namespace, podName, ownerName string) {
	t.Helper()
	patch := mustJSONPatch(t, map[string]any{
		"metadata": map[string]any{
			"ownerReferences": []map[string]any{{
				"apiVersion": "apps/v1",
				"kind":       "ReplicaSet",
				"name":       ownerName,
				"uid":        "11111111-2222-3333-4444-555555555555",
				"controller": true,
			}},
		},
	})
	_, err := testClient.CoreV1().Pods(namespace).Patch(ctx, podName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("patch pod %s/%s owner references: %v", namespace, podName, err)
	}
}

func waitForPodDeletionTimestamp(t *testing.T, ctx context.Context, namespace, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		pod, err := testClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get pod %s/%s while waiting for deletion timestamp: %v", namespace, name, err)
		}
		if pod.DeletionTimestamp != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s did not receive deletion timestamp within %s", namespace, name, timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for pod %s/%s deletion timestamp: %v", namespace, name, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
