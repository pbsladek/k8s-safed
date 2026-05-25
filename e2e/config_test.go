//go:build e2e

package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

// --------------------------------------------------------------------------
// TestDrain_ProfileConfigAndCLIOverride
// --------------------------------------------------------------------------

func TestConfigFixturesAreValidYAML(t *testing.T) {
	fixtures := []string{
		"risky-profile.yaml",
		"defaults-mode-profile.yaml",
		"env-strict.yaml",
		"explicit-dry-run.yaml",
		"unknown-field.yaml",
		"invalid-profile.yaml",
	}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			f, err := os.Open(configFixturePath(t, fixture))
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			defer func() { _ = f.Close() }()

			decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
			var doc map[string]any
			if err := decoder.Decode(&doc); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if len(doc) == 0 {
				t.Fatal("fixture decoded to an empty document")
			}

			var extra map[string]any
			err = decoder.Decode(&extra)
			if err != nil && err != io.EOF {
				t.Fatalf("decode extra document: %v", err)
			}
		})
	}
}

func TestDrain_ProfileConfigAndCLIOverride(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		cleanupManifest(t, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	configPath := configFixturePath(t, "risky-profile.yaml")

	strict := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--profile", "risky",
		"--only-workload", "Deployment/e2e/worker",
		"--timeout", "20s",
	)
	if strict.Err == nil {
		t.Fatal("profile preflight=strict must abort on single-replica worker")
	}
	verifyNodeNotCordoned(t, target)

	result := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--profile", "risky",
		"--preflight", "off",
		"--dry-run",
		"--only-workload", "Deployment/e2e/worker",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("CLI --preflight=off should override strict profile: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Dry-run complete") {
		t.Fatalf("CLI override output missing dry-run confirmation\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_ConfigDefaultsModeProfilePrecedence
// --------------------------------------------------------------------------

func TestDrain_ConfigDefaultsModeProfilePrecedence(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		cleanupManifest(t, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	configPath := configFixturePath(t, "defaults-mode-profile.yaml")

	defaults := testBinary.Drain(ctx, target, "--config", configPath)
	if defaults.Err == nil {
		t.Fatal("config defaults preflight=strict must abort on single-replica worker")
	}
	verifyNodeNotCordoned(t, target)

	mode := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--mode", "debug",
	)
	if mode.Err != nil {
		t.Fatalf("--mode=debug should override strict defaults and dry-run successfully: %v\nstdout: %s\nstderr: %s",
			mode.Err, mode.Stdout, mode.Stderr)
	}
	if !strings.Contains(mode.Stdout, "Dry-run complete") {
		t.Fatalf("--mode=debug output missing dry-run confirmation\nstdout: %s\nstderr: %s", mode.Stdout, mode.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	profile := testBinary.Drain(ctx, target,
		"--config", configPath,
		"--mode", "prod",
		"--profile", "lenient",
		"--preflight", "off",
		"--dry-run",
		"--only-workload", "Deployment/e2e/worker",
		"--poll-interval", "1s",
	)
	if profile.Err != nil {
		t.Fatalf("CLI --preflight=off should override defaults/mode/profile: %v\nstdout: %s\nstderr: %s",
			profile.Err, profile.Stdout, profile.Stderr)
	}
	if !strings.Contains(profile.Stdout, "Dry-run complete") {
		t.Fatalf("CLI override output missing dry-run confirmation\nstdout: %s\nstderr: %s", profile.Stdout, profile.Stderr)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_ConfigEnvAndExplicitConfigPrecedence
// --------------------------------------------------------------------------

func TestDrain_ConfigEnvAndExplicitConfigPrecedence(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)
	defer func() {
		cleanupManifest(t, framework.WorkerManifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, framework.WorkerManifest, "worker")

	envConfigPath := configFixturePath(t, "env-strict.yaml")
	t.Setenv("KUBECTL_SAFED_CONFIG", envConfigPath)

	fromEnv := testBinary.Drain(ctx, target,
		"--only-workload", "Deployment/e2e/worker",
		"--timeout", "20s",
	)
	if fromEnv.Err == nil {
		t.Fatal("env config preflight=strict must abort on single-replica worker")
	}
	verifyNodeNotCordoned(t, target)

	explicitConfigPath := configFixturePath(t, "explicit-dry-run.yaml")

	explicit := testBinary.Drain(ctx, target,
		"--config", explicitConfigPath,
		"--only-workload", "Deployment/e2e/worker",
	)
	if explicit.Err != nil {
		t.Fatalf("explicit --config should override KUBECTL_SAFED_CONFIG: %v\nstdout: %s\nstderr: %s",
			explicit.Err, explicit.Stdout, explicit.Stderr)
	}
	if !strings.Contains(explicit.Stdout, "Dry-run complete") {
		t.Fatalf("explicit config output missing dry-run confirmation\nstdout: %s\nstderr: %s", explicit.Stdout, explicit.Stderr)
	}
	verifyNodeNotCordoned(t, target)
}

// --------------------------------------------------------------------------
// TestDrain_ConfigValidationAndModeErrors
// --------------------------------------------------------------------------

func TestDrain_ConfigValidationAndModeErrors(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	badConfigPath := configFixturePath(t, "unknown-field.yaml")

	tests := []struct {
		name  string
		flags []string
		want  string
	}{
		{
			name:  "unknown config field",
			flags: []string{"--config", badConfigPath},
			want:  "typo-field",
		},
		{
			name:  "invalid mode",
			flags: []string{"--mode", "unknown"},
			want:  "invalid --mode",
		},
		{
			name:  "missing profile config",
			flags: []string{"--config", filepath.Join(t.TempDir(), "missing.yaml"), "--profile", "prod"},
			want:  "reading config file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := testBinary.Drain(ctx, target, tc.flags...)
			if result.Err == nil {
				t.Fatal("expected command to fail")
			}
			combined := result.Stdout + result.Stderr + result.Err.Error()
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("output missing %q\nerr: %v\nstdout: %s\nstderr: %s",
					tc.want, result.Err, result.Stdout, result.Stderr)
			}
			verifyNodeNotCordoned(t, target)
		})
	}
}

// --------------------------------------------------------------------------
// TestDrain_CustomStatefulPatternAndInvalidPriorityWarning
// --------------------------------------------------------------------------

func TestDrain_CustomStatefulPatternAndInvalidPriorityWarning(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := framework.DeploymentManifest(framework.DeploymentManifestOptions{
		Namespace: framework.E2ENamespace,
		Name:      "ledger-api",
		Priority:  100,
		Replicas:  2,
	})
	defer func() {
		cleanupManifest(t, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "ledger-api")

	patchDeployment(t, ctx, framework.E2ENamespace, "ledger-api", mustJSONPatch(t, map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{
				"kubectl.safed.io/drain-priority": "not-a-number",
			},
		},
	}))
	result := testBinary.Drain(ctx, target,
		"--preflight", "strict",
		"--dry-run",
		"--only-workload", "Deployment/e2e/ledger-api",
		"--stateful-name-pattern", "ledger",
		"--rollout-timeout", "5m",
		"--pod-vacate-timeout", "2m",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("drain should succeed; custom stateful pattern is a note, not a risk: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	combined := result.Stdout + result.Stderr
	if !strings.Contains(combined, `invalid kubectl.safed.io/drain-priority="not-a-number"`) {
		t.Fatalf("output missing invalid priority warning\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	if !strings.Contains(combined, `detected known stateful service ("ledger")`) {
		t.Fatalf("output missing custom stateful pattern note\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Dry-run complete") {
		t.Fatalf("custom stateful output missing dry-run confirmation\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
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

	configPath := configFixturePath(t, "invalid-profile.yaml")

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
		{
			name:  "skip and only workload are mutually exclusive",
			flags: []string{"--skip-workload", "Deployment/e2e/api", "--only-workload", "Deployment/e2e/api"},
			want:  "cannot use both --skip-workload and --only-workload",
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

func configFixturePath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "configs", name)
}
