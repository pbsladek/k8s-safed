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
// TestDrain_DaemonSetEvictionOverrideDryRun
// --------------------------------------------------------------------------

func TestDrain_DaemonSetEvictionOverrideDryRun(t *testing.T) {
	waitAllReady(t)

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
	waitForPodWithSelectorOnNode(t, ctx, target, framework.E2ENamespace, "app=node-agent", 90*time.Second)

	result := testBinary.Drain(ctx, target,
		"--dry-run",
		"--preflight", "off",
		"--ignore-daemonsets=false",
		"--only-workload", "Deployment/e2e/does-not-exist",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("daemonset eviction override dry-run failed: %v\nstdout: %s\nstderr: %s", result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
	if !strings.Contains(result.Stdout, "Pod/e2e/node-agent") ||
		!strings.Contains(result.Stdout, "Would evict (owner: DaemonSet)") {
		t.Fatalf("dry-run output missing DaemonSet eviction plan\nstdout: %s", result.Stdout)
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
