//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/k8s-safed/e2e/framework"
	drainpkg "github.com/pbsladek/k8s-safed/pkg/drain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
		cleanupManifest(t, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "resume-skip", "resume-run")

	cpPath := filepath.Join(t.TempDir(), "checkpoint.json")
	cpData := mustJSONPatch(t, checkpointForDeployment(t, ctx, target, framework.E2ENamespace, "resume-skip"))
	if err := os.WriteFile(cpPath, cpData, 0600); err != nil {
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
	verifyNodeNotCordoned(t, target)

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
// TestDrain_CorruptCheckpointFailsBeforeCordon
// --------------------------------------------------------------------------

func TestDrain_CorruptCheckpointFailsBeforeCordon(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	cpPath := filepath.Join(t.TempDir(), "corrupt-checkpoint.json")
	if err := os.WriteFile(cpPath, []byte(`{"nodeName":`), 0600); err != nil {
		t.Fatalf("write corrupt checkpoint: %v", err)
	}

	result := testBinary.Drain(ctx, target,
		"--resume",
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("resume with a corrupt checkpoint must fail")
	}
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "loading checkpoint") || !strings.Contains(combined, "parsing checkpoint") {
		t.Fatalf("corrupt checkpoint failed with unexpected error: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_CheckpointResumeRejectsContextMismatchBeforeCordon
// --------------------------------------------------------------------------

func TestDrain_CheckpointResumeRejectsContextMismatchBeforeCordon(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	cpPath := filepath.Join(t.TempDir(), "wrong-context.json")
	cpData := mustJSONPatch(t, drainpkg.Checkpoint{
		NodeName:  target,
		Context:   "not-the-current-context",
		Completed: map[string]bool{},
	})
	if err := os.WriteFile(cpPath, cpData, 0600); err != nil {
		t.Fatalf("write wrong-context checkpoint: %v", err)
	}

	result := testBinary.Drain(ctx, target,
		"--resume",
		"--checkpoint-path", cpPath,
		"--preflight", "off",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatal("resume with a checkpoint for the wrong kube context must fail")
	}
	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "checkpoint is for kube context") {
		t.Fatalf("wrong-context checkpoint failed with unexpected error: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
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
		cleanupManifest(t, manifest)
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
	if strings.Contains(result.Stdout, "Deployment/e2e/interrupt-done") {
		t.Fatalf("completed workload should not be restarted on resume\nstdout: %s", result.Stdout)
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Fatalf("checkpoint should be deleted after successful resume, stat err: %v", err)
	}
}

func checkpointForDeployment(t *testing.T, ctx context.Context, nodeName, namespace, name string) drainpkg.Checkpoint {
	t.Helper()

	dep, err := testClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s/%s for checkpoint metadata: %v", namespace, name, err)
	}
	key := "Deployment/" + namespace + "/" + name
	return drainpkg.Checkpoint{
		NodeName: nodeName,
		Context:  "",
		Completed: map[string]bool{
			key: true,
		},
		Workloads: map[string]drainpkg.CheckpointWork{
			key: {
				Kind:        "Deployment",
				Namespace:   namespace,
				Name:        name,
				UID:         string(dep.UID),
				Generation:  dep.Generation,
				CompletedAt: time.Now().UTC(),
			},
		},
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
		cleanupManifest(t, manifest)
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
