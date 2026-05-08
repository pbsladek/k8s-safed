//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

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
		cleanupManifest(t, manifest)
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
	foundNode := map[string]bool{}
	foundWorkload := map[string]bool{}
	for (!foundNode["Draining"] || !foundNode["Drained"] ||
		!foundWorkload["RollingRestartTriggered"] || !foundWorkload["RollingRestartComplete"]) &&
		time.Now().Before(deadline) {
		nodeEvents, err := framework.EventsForNode(ctx, testClient, target)
		if err != nil {
			t.Fatalf("list node events: %v", err)
		}
		for _, e := range nodeEvents {
			if e.CreationTimestamp.Time.After(since) {
				foundNode[e.Reason] = true
			}
		}

		workloadEvents, err := framework.EventsForObject(ctx, testClient, framework.E2ENamespace,
			"Deployment", "events-workload")
		if err != nil {
			t.Fatalf("list workload events: %v", err)
		}
		for _, e := range workloadEvents {
			if e.CreationTimestamp.Time.After(since) {
				foundWorkload[e.Reason] = true
			}
		}

		if !foundNode["Draining"] || !foundNode["Drained"] ||
			!foundWorkload["RollingRestartTriggered"] || !foundWorkload["RollingRestartComplete"] {
			time.Sleep(2 * time.Second)
		}
	}
	if !foundNode["Draining"] || !foundNode["Drained"] {
		t.Errorf("missing current node events after --emit-events: got=%v", foundNode)
	}
	if !foundWorkload["RollingRestartTriggered"] || !foundWorkload["RollingRestartComplete"] {
		t.Errorf("missing current workload events after --emit-events: got=%v", foundWorkload)
	}
}

// --------------------------------------------------------------------------
// TestDrain_EmitEventsFailure
// --------------------------------------------------------------------------

func TestDrain_EmitEventsFailure(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		cleanupManifest(t, framework.StandalonePodWithPDBManifest)
	}()
	withOnlyNodeSchedulable(t, ctx, target, func() {
		if err := framework.ApplyManifest(ctx, testCluster.KubeconfigPath, framework.StandalonePodWithPDBManifest); err != nil {
			t.Fatalf("apply PDB-blocked standalone pod: %v", err)
		}
		waitForPodWithSelectorOnNode(t, ctx, target, framework.E2ENamespace, framework.StandalonePDBPodSelector, workloadReady)
	})

	since := time.Now().Add(-1 * time.Second)
	result := testBinary.Drain(ctx, target,
		"--emit-events",
		"--preflight", "off",
		"--force",
		"--eviction-timeout", "10s",
		"--pdb-retry-interval", "1s",
		"--uncordon-on-failure",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatalf("drain should fail for PDB-blocked standalone pod\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}

	deadline := time.Now().Add(30 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		nodeEvents, err := framework.EventsForNode(ctx, testClient, target)
		if err != nil {
			t.Fatalf("list node events: %v", err)
		}
		for _, e := range nodeEvents {
			if e.Reason == "DrainFailed" && e.CreationTimestamp.Time.After(since) {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(2 * time.Second)
		}
	}
	if !found {
		t.Fatalf("no current DrainFailed event on node %s after failed --emit-events drain", target)
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
		cleanupManifest(t, manifest)
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
		cleanupManifest(t, manifest)
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
		cleanupManifest(t, framework.WorkerManifest)
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
