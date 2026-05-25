package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/internal/drainapp"
	"github.com/pbsladek/k8s-safed/pkg/k8s"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestValidateDrainOptions_Defaults(t *testing.T) {
	opts := &drainOptions{
		preflight:        "warn",
		logFormat:        "plain",
		maxConcurrency:   1,
		nodeConcurrency:  1,
		gracePeriod:      -1,
		rolloutTimeout:   5 * time.Minute,
		podVacateTimeout: 2 * time.Minute,
		evictionTimeout:  5 * time.Minute,
		pdbRetryInterval: 5 * time.Second,
		pollInterval:     5 * time.Second,
	}
	if err := validateDrainOptions(opts); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
}

func TestDrainOptionSpecsMatchCommandFlags(t *testing.T) {
	cmd := NewDrainCommand()
	got := map[string]bool{}
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		got[flag.Name] = true
	})
	want := publicDrainOptionNames(true)
	for name := range want {
		if !got[name] {
			t.Errorf("drainOptionSpecs contains %q but command flag is missing", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("command flag %q is missing from drainOptionSpecs", name)
		}
	}
}

func TestValidateDrainOptions_AllowsExplicitZeroModes(t *testing.T) {
	opts := &drainOptions{
		preflight:        "off",
		logFormat:        "json",
		maxConcurrency:   0,
		nodeConcurrency:  0,
		gracePeriod:      0,
		timeout:          0,
		rolloutTimeout:   0,
		podVacateTimeout: 0,
		evictionTimeout:  0,
		pdbRetryInterval: 0,
		pollInterval:     0,
	}
	if err := validateDrainOptions(opts); err != nil {
		t.Fatalf("explicit zero modes should validate: %v", err)
	}
}

func TestValidateDrainOptions_RejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*drainOptions)
		want   string
	}{
		{
			name: "invalid preflight",
			mutate: func(o *drainOptions) {
				o.preflight = "sometimes"
			},
			want: "invalid --preflight",
		},
		{
			name: "invalid log format",
			mutate: func(o *drainOptions) {
				o.logFormat = "yaml"
			},
			want: "invalid --log-format",
		},
		{
			name: "negative max concurrency",
			mutate: func(o *drainOptions) {
				o.maxConcurrency = -1
			},
			want: "--max-concurrency must be >= 0",
		},
		{
			name: "negative node concurrency",
			mutate: func(o *drainOptions) {
				o.nodeConcurrency = -1
			},
			want: "--node-concurrency must be >= 0",
		},
		{
			name: "invalid grace period",
			mutate: func(o *drainOptions) {
				o.gracePeriod = -2
			},
			want: "--grace-period must be -1 or >= 0",
		},
		{
			name: "negative timeout",
			mutate: func(o *drainOptions) {
				o.timeout = -time.Second
			},
			want: "--timeout must be >= 0",
		},
		{
			name: "negative rollout timeout",
			mutate: func(o *drainOptions) {
				o.rolloutTimeout = -time.Second
			},
			want: "--rollout-timeout must be >= 0",
		},
		{
			name: "negative pod vacate timeout",
			mutate: func(o *drainOptions) {
				o.podVacateTimeout = -time.Second
			},
			want: "--pod-vacate-timeout must be >= 0",
		},
		{
			name: "negative eviction timeout",
			mutate: func(o *drainOptions) {
				o.evictionTimeout = -time.Second
			},
			want: "--eviction-timeout must be >= 0",
		},
		{
			name: "negative pdb retry interval",
			mutate: func(o *drainOptions) {
				o.pdbRetryInterval = -time.Second
			},
			want: "--pdb-retry-interval must be >= 0",
		},
		{
			name: "negative poll interval",
			mutate: func(o *drainOptions) {
				o.pollInterval = -time.Second
			},
			want: "--poll-interval must be >= 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := &drainOptions{
				preflight:        "warn",
				logFormat:        "plain",
				maxConcurrency:   1,
				nodeConcurrency:  1,
				gracePeriod:      -1,
				rolloutTimeout:   5 * time.Minute,
				podVacateTimeout: 2 * time.Minute,
				evictionTimeout:  5 * time.Minute,
				pdbRetryInterval: 5 * time.Second,
				pollInterval:     5 * time.Second,
			}
			tc.mutate(opts)

			err := validateDrainOptions(opts)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateDrainTargets_RejectsSharedCheckpointPathForMultipleNodes(t *testing.T) {
	opts := &drainOptions{checkpointPath: "/tmp/safed-checkpoint.json"}
	err := validateDrainTargets(opts, []string{"node-a", "node-b"})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "--checkpoint-path can only be used when draining a single node") {
		t.Fatalf("validation error %q did not mention checkpoint path restriction", err.Error())
	}
}

func TestValidateDrainTargets_AllowsDefaultPerNodeCheckpoints(t *testing.T) {
	opts := &drainOptions{}
	if err := validateDrainTargets(opts, []string{"node-a", "node-b"}); err != nil {
		t.Fatalf("default per-node checkpoints should validate: %v", err)
	}
}

func TestValidateDrainTargets_AllowsCustomCheckpointPathForSingleNode(t *testing.T) {
	opts := &drainOptions{checkpointPath: "/tmp/safed-checkpoint.json"}
	if err := validateDrainTargets(opts, []string{"node-a"}); err != nil {
		t.Fatalf("single-node custom checkpoint should validate: %v", err)
	}
}

func TestDrainCommand_RequiresNodeOrSelector(t *testing.T) {
	cmd := NewDrainCommand()
	cmd.SetArgs([]string{"--dry-run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "must specify at least one node name or --selector") {
		t.Fatalf("err = %v, want missing target validation", err)
	}
}

func TestDrainCommand_RejectsNodeAndSelector(t *testing.T) {
	cmd := NewDrainCommand()
	cmd.SetArgs([]string{"node-a", "--selector", "pool=spot"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot specify both node names and --selector") {
		t.Fatalf("err = %v, want node plus selector validation", err)
	}
}

func TestDrainCommand_RejectsSkipAndOnlyWorkload(t *testing.T) {
	cmd := NewDrainCommand()
	cmd.SetArgs([]string{
		"node-a",
		"--skip-workload", "Deployment/default/api",
		"--only-workload", "Deployment/default/api",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot use both --skip-workload and --only-workload") {
		t.Fatalf("err = %v, want skip/only validation", err)
	}
}

func TestApplyConfig_DefaultsModeProfileOrder(t *testing.T) {
	cfgPath := writeDrainConfig(t, `
defaults:
  preflight: warn
  timeout: 20m
  stateful-name-patterns:
    - ledger
profiles:
  custom:
    timeout: 30m
    max-concurrency: 4
    stateful-name-patterns:
      - temporal
`)
	opts := defaultDrainOptionsForTest()
	opts.configFile = cfgPath
	opts.mode = "prod"
	opts.profile = "custom"
	cmd := drainCommandForConfigTest()

	if err := applyConfig(cmd, opts); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	if opts.preflight != "strict" {
		t.Errorf("preflight = %q, want strict from prod mode", opts.preflight)
	}
	if opts.timeout != 30*time.Minute {
		t.Errorf("timeout = %v, want profile override 30m", opts.timeout)
	}
	if opts.maxConcurrency != 4 {
		t.Errorf("maxConcurrency = %d, want profile override 4", opts.maxConcurrency)
	}
	if !opts.emitEvents {
		t.Error("emitEvents should come from prod mode")
	}
	if got := strings.Join(opts.statefulNamePatterns, ","); got != "ledger,temporal" {
		t.Errorf("statefulNamePatterns = %q, want ledger,temporal", got)
	}
}

func TestApplyConfig_MissingDefaultConfigIgnoredWithoutProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECTL_SAFED_CONFIG", "")
	opts := defaultDrainOptionsForTest()

	if err := applyConfig(drainCommandForConfigTest(), opts); err != nil {
		t.Fatalf("missing default config should be ignored without profile: %v", err)
	}
}

func TestApplyConfig_EnvConfigMissingReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("KUBECTL_SAFED_CONFIG", path)
	opts := defaultDrainOptionsForTest()

	err := applyConfig(drainCommandForConfigTest(), opts)
	if err == nil || !strings.Contains(err.Error(), "reading config file") {
		t.Fatalf("err = %v, want missing env config error", err)
	}
}

func TestApplyConfig_CLIOverridesConfigAndMode(t *testing.T) {
	cfgPath := writeDrainConfig(t, `
defaults:
  preflight: strict
profiles:
  custom:
    preflight: warn
`)
	opts := defaultDrainOptionsForTest()
	opts.configFile = cfgPath
	opts.mode = "prod"
	opts.profile = "custom"
	opts.preflight = "off"
	cmd := drainCommandForConfigTest()
	if err := cmd.Flags().Set("preflight", "off"); err != nil {
		t.Fatal(err)
	}

	if err := applyConfig(cmd, opts); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}
	if opts.preflight != "off" {
		t.Errorf("preflight = %q, want CLI override off", opts.preflight)
	}
}

func TestApplyConfig_AllProfileFieldsAreWired(t *testing.T) {
	cfgPath := writeDrainConfig(t, `
defaults:
  timeout: 11m
  rollout-timeout: 12m
  pod-vacate-timeout: 13m
  eviction-timeout: 14m
  pdb-retry-interval: 15s
  poll-interval: 16s
  max-concurrency: 2
  node-concurrency: 3
  preflight: strict
  log-format: json
  dry-run: true
  force: true
  ignore-daemonsets: false
  delete-emptydir-data: true
  force-delete-standalone: true
  uncordon-on-failure: true
  emit-events: true
  stateful-name-patterns:
    - ledger
`)
	opts := defaultDrainOptionsForTest()
	opts.configFile = cfgPath

	if err := applyConfig(drainCommandForConfigTest(), opts); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	checks := []struct {
		name string
		ok   bool
	}{
		{"timeout", opts.timeout == 11*time.Minute},
		{"rollout-timeout", opts.rolloutTimeout == 12*time.Minute},
		{"pod-vacate-timeout", opts.podVacateTimeout == 13*time.Minute},
		{"eviction-timeout", opts.evictionTimeout == 14*time.Minute},
		{"pdb-retry-interval", opts.pdbRetryInterval == 15*time.Second},
		{"poll-interval", opts.pollInterval == 16*time.Second},
		{"max-concurrency", opts.maxConcurrency == 2},
		{"node-concurrency", opts.nodeConcurrency == 3},
		{"preflight", opts.preflight == "strict"},
		{"log-format", opts.logFormat == "json"},
		{"dry-run", opts.dryRun},
		{"force", opts.force},
		{"ignore-daemonsets", !opts.skipDaemonSets},
		{"delete-emptydir-data", opts.deleteEmptyDir},
		{"force-delete-standalone", opts.forceDeleteStandalone},
		{"uncordon-on-failure", opts.uncordonOnFailure},
		{"emit-events", opts.emitEvents},
		{"stateful-name-patterns", strings.Join(opts.statefulNamePatterns, ",") == "ledger"},
	}
	for _, check := range checks {
		if !check.ok {
			t.Errorf("%s was not applied from config profile", check.name)
		}
	}
}

func TestApplyConfig_InvalidMode(t *testing.T) {
	opts := defaultDrainOptionsForTest()
	opts.mode = "unknown"
	err := applyConfig(drainCommandForConfigTest(), opts)
	if err == nil {
		t.Fatal("expected invalid mode error")
	}
	if !strings.Contains(err.Error(), "invalid --mode") {
		t.Fatalf("error = %q, want invalid --mode", err.Error())
	}
}

func TestResolveNodeNames_SelectorReturnsSortedMatches(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"pool": "spot"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "spot"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c", Labels: map[string]string{"pool": "ondemand"}}},
	)}

	got, err := drainapp.ResolveNodeNames(context.Background(), client, nil, "pool=spot")
	if err != nil {
		t.Fatalf("resolveNodeNames: %v", err)
	}
	if strings.Join(got, ",") != "node-a,node-b" {
		t.Fatalf("nodes = %v, want node-a,node-b", got)
	}
}

func TestResolveNodeNames_SelectorNoMatches(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "ondemand"}}},
	)}

	_, err := drainapp.ResolveNodeNames(context.Background(), client, nil, "pool=spot")
	if err == nil || !strings.Contains(err.Error(), "no nodes matched selector") {
		t.Fatalf("err = %v, want no-match error", err)
	}
}

func writeDrainConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "safed.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func defaultDrainOptionsForTest() *drainOptions {
	return &drainOptions{
		preflight:        "warn",
		logFormat:        "plain",
		maxConcurrency:   1,
		nodeConcurrency:  1,
		gracePeriod:      -1,
		rolloutTimeout:   5 * time.Minute,
		podVacateTimeout: 2 * time.Minute,
		evictionTimeout:  5 * time.Minute,
		pdbRetryInterval: 5 * time.Second,
		pollInterval:     5 * time.Second,
		skipDaemonSets:   true,
	}
}

func drainCommandForConfigTest() *cobra.Command {
	cmd := &cobra.Command{}
	flags := cmd.Flags()
	flags.Duration("timeout", 0, "")
	flags.Duration("rollout-timeout", 0, "")
	flags.Duration("pod-vacate-timeout", 0, "")
	flags.Duration("eviction-timeout", 0, "")
	flags.Duration("pdb-retry-interval", 0, "")
	flags.Duration("poll-interval", 0, "")
	flags.Int("max-concurrency", 1, "")
	flags.Int("node-concurrency", 1, "")
	flags.String("preflight", "warn", "")
	flags.String("log-format", "plain", "")
	flags.Bool("dry-run", false, "")
	flags.Bool("force", false, "")
	flags.Bool("ignore-daemonsets", true, "")
	flags.Bool("delete-emptydir-data", false, "")
	flags.Bool("force-delete-standalone", false, "")
	flags.Bool("uncordon-on-failure", false, "")
	flags.Bool("emit-events", false, "")
	return cmd
}
