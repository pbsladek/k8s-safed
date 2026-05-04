package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
