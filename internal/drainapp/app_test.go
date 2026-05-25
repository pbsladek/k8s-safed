package drainapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/config"
	"github.com/pbsladek/k8s-safed/pkg/k8s"
)

func defaultOptionsForTest() Options {
	return Options{
		Timeout:              30 * time.Minute,
		SkipDaemonSets:       true,
		GracePeriod:          -1,
		RolloutTimeout:       10 * time.Minute,
		PodVacateTimeout:     5 * time.Minute,
		EvictionTimeout:      5 * time.Minute,
		PDBRetryInterval:     5 * time.Second,
		PollInterval:         time.Second,
		MaxConcurrency:       1,
		LogFormat:            "plain",
		NodeConcurrency:      1,
		Preflight:            "warn",
		StatefulNamePatterns: []string{"postgres", "mysql"},
	}
}

func changedFlags(names ...string) FlagChanged {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return func(name string) bool {
		return set[name]
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestRunResolved_DryRunMultipleNodes(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	)}
	var out bytes.Buffer
	opts := Options{
		DryRun:           true,
		LogFormat:        "plain",
		Preflight:        "warn",
		GracePeriod:      -1,
		MaxConcurrency:   1,
		NodeConcurrency:  1,
		RolloutTimeout:   time.Minute,
		PodVacateTimeout: time.Minute,
		EvictionTimeout:  time.Minute,
		PDBRetryInterval: time.Second,
		PollInterval:     time.Millisecond,
	}

	if err := RunResolved(context.Background(), client, &out, []string{"node-a", "node-b"}, "test-context", opts); err != nil {
		t.Fatalf("RunResolved dry-run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `Dry-run complete`) {
		t.Fatalf("expected dry-run completion output, got:\n%s", out.String())
	}
}

func TestRunResolved_DryRunUnlimitedNodeConcurrencyWarns(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	)}
	var out bytes.Buffer
	opts := defaultOptionsForTest()
	opts.DryRun = true
	opts.NodeConcurrency = 0

	if err := RunResolved(context.Background(), client, &out, []string{"node-a", "node-b"}, "test-context", opts); err != nil {
		t.Fatalf("RunResolved dry-run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--node-concurrency=0 drains all 2 node(s) concurrently") {
		t.Fatalf("expected node concurrency warning, got:\n%s", out.String())
	}
}

func TestValidateTargets_RejectsSharedCheckpointForMultipleNodes(t *testing.T) {
	err := ValidateTargets(Options{CheckpointPath: "/tmp/checkpoint.json"}, []string{"node-a", "node-b"})
	if err == nil || !strings.Contains(err.Error(), "--checkpoint-path can only be used") {
		t.Fatalf("err = %v, want checkpoint validation", err)
	}
}

func TestValidateOptions_RejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "preflight", opts: Options{Preflight: "bad", LogFormat: "plain"}, want: "invalid --preflight"},
		{name: "log format", opts: Options{Preflight: "warn", LogFormat: "yaml"}, want: "invalid --log-format"},
		{name: "max concurrency", opts: Options{Preflight: "warn", LogFormat: "plain", MaxConcurrency: -1}, want: "--max-concurrency"},
		{name: "node concurrency", opts: Options{Preflight: "warn", LogFormat: "plain", NodeConcurrency: -1}, want: "--node-concurrency"},
		{name: "grace period", opts: Options{Preflight: "warn", LogFormat: "plain", GracePeriod: -2}, want: "--grace-period"},
		{name: "duration", opts: Options{Preflight: "warn", LogFormat: "plain", Timeout: -time.Second}, want: "--timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOptions(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveNodeNames_SelectorReturnsSortedMatches(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"pool": "spot"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "spot"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c", Labels: map[string]string{"pool": "ondemand"}}},
	)}

	got, err := ResolveNodeNames(context.Background(), client, nil, "pool=spot")
	if err != nil {
		t.Fatalf("ResolveNodeNames: %v", err)
	}
	if strings.Join(got, ",") != "node-a,node-b" {
		t.Fatalf("nodes = %v, want node-a,node-b", got)
	}
}

func TestResolveNodeNames_EmptySelectorReturnsArgs(t *testing.T) {
	client := &k8s.Client{Kubernetes: fake.NewClientset()}
	got, err := ResolveNodeNames(context.Background(), client, []string{"node-a", "node-b"}, "")
	if err != nil {
		t.Fatalf("ResolveNodeNames: %v", err)
	}
	if strings.Join(got, ",") != "node-a,node-b" {
		t.Fatalf("nodes = %v, want original args", got)
	}
}

func TestApplyConfig_DefaultsModeProfileOrder(t *testing.T) {
	cfgPath := writeConfig(t, `
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
	opts := defaultOptionsForTest()
	opts.ConfigFile = cfgPath
	opts.Mode = "prod"
	opts.Profile = "custom"

	if err := ApplyConfig(&opts, changedFlags()); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	if opts.Preflight != "strict" {
		t.Errorf("preflight = %q, want strict from prod mode", opts.Preflight)
	}
	if opts.Timeout != 30*time.Minute {
		t.Errorf("timeout = %v, want profile override 30m", opts.Timeout)
	}
	if opts.MaxConcurrency != 4 {
		t.Errorf("max concurrency = %d, want profile override 4", opts.MaxConcurrency)
	}
	if !opts.EmitEvents {
		t.Error("emit events should come from prod mode")
	}
	if got := strings.Join(opts.StatefulNamePatterns, ","); got != "postgres,mysql,ledger,temporal" {
		t.Errorf("stateful patterns = %q, want defaults plus config values", got)
	}
}

func TestApplyConfig_CLIOverridesConfigAndMode(t *testing.T) {
	cfgPath := writeConfig(t, `
defaults:
  preflight: strict
profiles:
  custom:
    preflight: warn
`)
	opts := defaultOptionsForTest()
	opts.ConfigFile = cfgPath
	opts.Mode = "prod"
	opts.Profile = "custom"
	opts.Preflight = "off"

	if err := ApplyConfig(&opts, changedFlags("preflight")); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if opts.Preflight != "off" {
		t.Fatalf("preflight = %q, want CLI override", opts.Preflight)
	}
}

func TestApplyConfig_InvalidMode(t *testing.T) {
	opts := defaultOptionsForTest()
	opts.Mode = "unknown"

	err := ApplyConfig(&opts, changedFlags())
	if err == nil || !strings.Contains(err.Error(), "invalid --mode") {
		t.Fatalf("err = %v, want invalid mode error", err)
	}
}

func TestApplyConfig_MissingExplicitConfigReturnsError(t *testing.T) {
	opts := defaultOptionsForTest()
	opts.ConfigFile = filepath.Join(t.TempDir(), "missing.yaml")

	err := ApplyConfig(&opts, changedFlags())
	if err == nil || !strings.Contains(err.Error(), "reading config file") {
		t.Fatalf("err = %v, want config read error", err)
	}
}

func TestApplyProfileValues_AllProfileFields(t *testing.T) {
	opts := defaultOptionsForTest()
	prof := config.Profile{
		Timeout:               durationPtr(11 * time.Minute),
		RolloutTimeout:        durationPtr(12 * time.Minute),
		PodVacateTimeout:      durationPtr(13 * time.Minute),
		EvictionTimeout:       durationPtr(14 * time.Minute),
		PDBRetryInterval:      durationPtr(15 * time.Second),
		PollInterval:          durationPtr(16 * time.Second),
		MaxConcurrency:        intPtr(2),
		NodeConcurrency:       intPtr(3),
		Preflight:             "strict",
		LogFormat:             "json",
		DryRun:                boolPtr(true),
		Force:                 boolPtr(true),
		IgnoreDaemonSets:      boolPtr(false),
		DeleteEmptyDir:        boolPtr(true),
		ForceDeleteStandalone: boolPtr(true),
		UncordonOnFailure:     boolPtr(true),
		EmitEvents:            boolPtr(true),
		StatefulNamePatterns:  []string{"ledger"},
	}

	ApplyProfileValues(&opts, changedFlags(), prof)

	checks := []struct {
		name string
		ok   bool
	}{
		{"timeout", opts.Timeout == 11*time.Minute},
		{"rollout-timeout", opts.RolloutTimeout == 12*time.Minute},
		{"pod-vacate-timeout", opts.PodVacateTimeout == 13*time.Minute},
		{"eviction-timeout", opts.EvictionTimeout == 14*time.Minute},
		{"pdb-retry-interval", opts.PDBRetryInterval == 15*time.Second},
		{"poll-interval", opts.PollInterval == 16*time.Second},
		{"max-concurrency", opts.MaxConcurrency == 2},
		{"node-concurrency", opts.NodeConcurrency == 3},
		{"preflight", opts.Preflight == "strict"},
		{"log-format", opts.LogFormat == "json"},
		{"dry-run", opts.DryRun},
		{"force", opts.Force},
		{"ignore-daemonsets", !opts.SkipDaemonSets},
		{"delete-emptydir-data", opts.DeleteEmptyDir},
		{"force-delete-standalone", opts.ForceDeleteStandalone},
		{"uncordon-on-failure", opts.UncordonOnFailure},
		{"emit-events", opts.EmitEvents},
		{"stateful-name-patterns", strings.Join(opts.StatefulNamePatterns, ",") == "postgres,mysql,ledger"},
	}
	for _, check := range checks {
		if !check.ok {
			t.Errorf("%s was not applied", check.name)
		}
	}
}

func TestBuiltinModeNamesSorted(t *testing.T) {
	got := strings.Join(BuiltinModeNames(), ",")
	if got != "debug,prod,scale-down" {
		t.Fatalf("BuiltinModeNames = %q", got)
	}
}

func TestSliceToSet(t *testing.T) {
	if got := sliceToSet(nil); got != nil {
		t.Fatalf("sliceToSet(nil) = %#v, want nil", got)
	}
	got := sliceToSet([]string{"a", "b", "a"})
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("sliceToSet = %#v, want a/b", got)
	}
}
