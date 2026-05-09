// Package drainapp contains the application-level orchestration for the drain
// command. The Cobra layer owns parsing and help text; this package owns config
// precedence, target resolution, checkpoint selection, and multi-node execution.
package drainapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/pbsladek/k8s-safed/pkg/config"
	"github.com/pbsladek/k8s-safed/pkg/drain"
	"github.com/pbsladek/k8s-safed/pkg/k8s"
)

// Options is the application-level drain command configuration after Cobra has
// parsed flags but before config/mode/profile defaults are applied.
type Options struct {
	DryRun                bool
	Timeout               time.Duration
	SkipDaemonSets        bool
	DeleteEmptyDir        bool
	GracePeriod           int32
	RolloutTimeout        time.Duration
	PodVacateTimeout      time.Duration
	EvictionTimeout       time.Duration
	PDBRetryInterval      time.Duration
	PollInterval          time.Duration
	Force                 bool
	ForceDeleteStandalone bool
	MaxConcurrency        int
	LogFormat             string
	UncordonOnFailure     bool
	NodeSelector          string
	NodeConcurrency       int
	Preflight             string
	SkipWorkloads         []string
	OnlyWorkloads         []string
	Profile               string
	ConfigFile            string
	Mode                  string
	StatefulNamePatterns  []string
	EmitEvents            bool
	Resume                bool
	CheckpointPath        string
}

// FlagChanged reports whether a CLI flag was explicitly set by the user.
type FlagChanged func(name string) bool

// Run applies config/defaults and executes one or more node drains.
func Run(
	ctx context.Context,
	kubeFlags *genericclioptions.ConfigFlags,
	stdout io.Writer,
	nodeArgs []string,
	opts *Options,
	changed FlagChanged,
) error {
	if err := ApplyConfig(opts, changed); err != nil {
		return err
	}
	if err := ValidateOptions(*opts); err != nil {
		return err
	}

	client, err := k8s.NewClient(kubeFlags)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	nodes, err := ResolveNodeNames(ctx, client, nodeArgs, opts.NodeSelector)
	if err != nil {
		return err
	}
	if err := ValidateTargets(*opts, nodes); err != nil {
		return err
	}

	kubeCtx, err := EffectiveKubeContext(kubeFlags)
	if err != nil {
		return fmt.Errorf("resolving kube context: %w", err)
	}

	return RunResolved(ctx, client, stdout, nodes, kubeCtx, *opts)
}

// RunResolved drains a previously resolved node set. Tests can use it to cover
// orchestration without involving kubeconfig loading or selector resolution.
func RunResolved(ctx context.Context, client *k8s.Client, stdout io.Writer, nodes []string, kubeCtx string, opts Options) error {
	out := drain.NewPrinterWithFormat(stdout, drain.LogFormat(opts.LogFormat))
	var coordinator *drain.WorkloadCoordinator
	if len(nodes) > 1 {
		coordinator = drain.NewWorkloadCoordinator()
	}

	drainNode := func(ctx context.Context, nodeName string) error {
		cpPath := opts.CheckpointPath
		if cpPath == "" {
			var err error
			cpPath, err = drain.CheckpointPath(kubeCtx, nodeName)
			if err != nil {
				return fmt.Errorf("resolving checkpoint path: %w", err)
			}
		}

		drainer := drain.NewDrainer(drain.Options{
			Client:                client,
			NodeName:              nodeName,
			DryRun:                opts.DryRun,
			Timeout:               opts.Timeout,
			SkipDaemonSets:        opts.SkipDaemonSets,
			DeleteEmptyDir:        opts.DeleteEmptyDir,
			GracePeriod:           opts.GracePeriod,
			RolloutTimeout:        opts.RolloutTimeout,
			PodVacateTimeout:      opts.PodVacateTimeout,
			EvictionTimeout:       opts.EvictionTimeout,
			PDBRetryInterval:      opts.PDBRetryInterval,
			PollInterval:          opts.PollInterval,
			Force:                 opts.Force || opts.ForceDeleteStandalone,
			ForceDeleteStandalone: opts.ForceDeleteStandalone,
			MaxConcurrency:        opts.MaxConcurrency,
			Out:                   out,
			UncordonOnFailure:     opts.UncordonOnFailure,
			Preflight:             drain.PreflightMode(opts.Preflight),
			StatefulNamePatterns:  opts.StatefulNamePatterns,
			SkipWorkloads:         sliceToSet(opts.SkipWorkloads),
			OnlyWorkloads:         sliceToSet(opts.OnlyWorkloads),
			EmitEvents:            opts.EmitEvents,
			Resume:                opts.Resume,
			CheckpointPath:        cpPath,
			CheckpointContext:     kubeCtx,
			WorkloadCoordinator:   coordinator,
		})
		return drainer.Run(ctx)
	}

	concurrency := opts.NodeConcurrency
	if concurrency <= 0 {
		concurrency = len(nodes)
		out.Warnf("drain", "--node-concurrency=0 drains all %d node(s) concurrently; monitor API server and workload capacity", concurrency)
	}
	if concurrency > 10 {
		out.Warnf("drain", "node concurrency is %d; high parallelism can create API and scheduling pressure", concurrency)
	}

	if concurrency == 1 {
		for _, node := range nodes {
			if err := drainNode(ctx, node); err != nil {
				return err
			}
		}
		return nil
	}

	for batchStart := 0; batchStart < len(nodes); batchStart += concurrency {
		end := batchStart + concurrency
		if end > len(nodes) {
			end = len(nodes)
		}
		batch := nodes[batchStart:end]

		g, gctx := errgroup.WithContext(ctx)
		for _, nodeName := range batch {
			nodeName := nodeName
			g.Go(func() error {
				return drainNode(gctx, nodeName)
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTargets validates constraints that depend on the resolved node list.
func ValidateTargets(opts Options, nodes []string) error {
	if opts.CheckpointPath != "" && len(nodes) > 1 {
		return fmt.Errorf("--checkpoint-path can only be used when draining a single node; omit it for per-node default checkpoints")
	}
	return nil
}

// ValidateOptions validates scalar drain options after config/mode/profile
// defaults have been applied.
func ValidateOptions(opts Options) error {
	switch drain.PreflightMode(opts.Preflight) {
	case drain.PreflightModeWarn, drain.PreflightModeStrict, drain.PreflightModeOff:
	default:
		return fmt.Errorf("invalid --preflight %q (must be one of: warn, strict, off)", opts.Preflight)
	}

	switch drain.LogFormat(opts.LogFormat) {
	case drain.LogFormatPlain, drain.LogFormatJSON:
	default:
		return fmt.Errorf("invalid --log-format %q (must be one of: plain, json)", opts.LogFormat)
	}

	if opts.MaxConcurrency < 0 {
		return fmt.Errorf("--max-concurrency must be >= 0")
	}
	if opts.NodeConcurrency < 0 {
		return fmt.Errorf("--node-concurrency must be >= 0")
	}
	if opts.GracePeriod < -1 {
		return fmt.Errorf("--grace-period must be -1 or >= 0")
	}

	durationChecks := []struct {
		name  string
		value time.Duration
	}{
		{"--timeout", opts.Timeout},
		{"--rollout-timeout", opts.RolloutTimeout},
		{"--pod-vacate-timeout", opts.PodVacateTimeout},
		{"--eviction-timeout", opts.EvictionTimeout},
		{"--pdb-retry-interval", opts.PDBRetryInterval},
		{"--poll-interval", opts.PollInterval},
	}
	for _, check := range durationChecks {
		if check.value < 0 {
			return fmt.Errorf("%s must be >= 0", check.name)
		}
	}

	return nil
}

// ResolveNodeNames returns the list of node names to drain. When nodeSelector
// is non-empty, it lists nodes matching that label selector; otherwise it
// returns nodeArgs directly. Transient API errors are retried up to 3 times.
func ResolveNodeNames(ctx context.Context, client *k8s.Client, nodeArgs []string, nodeSelector string) ([]string, error) {
	if nodeSelector == "" {
		return nodeArgs, nil
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		nodeList, err := client.Kubernetes.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: nodeSelector,
		})
		if err != nil {
			if k8s.IsTransientAPIError(err) {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("listing nodes with selector %q: %w", nodeSelector, err)
				case <-time.After(2 * time.Second):
				}
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("listing nodes with selector %q: %w", nodeSelector, err)
		}
		if len(nodeList.Items) == 0 {
			return nil, fmt.Errorf("no nodes matched selector %q", nodeSelector)
		}
		names := make([]string, len(nodeList.Items))
		for i, n := range nodeList.Items {
			names[i] = n.Name
		}
		sort.Strings(names)
		return names, nil
	}
	return nil, fmt.Errorf("listing nodes with selector %q: %w", nodeSelector, lastErr)
}

// EffectiveKubeContext returns the kubeconfig context that this command will
// use. An explicit --context flag wins; otherwise use the current context from
// the loaded kubeconfig so default checkpoint names don't collide across
// clusters.
func EffectiveKubeContext(kubeFlags *genericclioptions.ConfigFlags) (string, error) {
	if kubeFlags.Context != nil && *kubeFlags.Context != "" {
		return *kubeFlags.Context, nil
	}
	rawCfg, err := kubeFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "", err
	}
	return rawCfg.CurrentContext, nil
}

// ApplyConfig applies defaults in this order:
// built-in flag defaults -> config defaults -> built-in mode -> named profile -> CLI flags.
func ApplyConfig(opts *Options, changed FlagChanged) error {
	if changed == nil {
		changed = func(string) bool { return false }
	}
	cfgPath := opts.ConfigFile
	explicitConfig := cfgPath != ""
	if cfgPath == "" {
		cfgPath = os.Getenv("KUBECTL_SAFED_CONFIG")
	}
	envConfig := cfgPath != "" && !explicitConfig
	if cfgPath == "" {
		var err error
		cfgPath, err = config.DefaultConfigPath()
		if err != nil {
			return err
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || explicitConfig || envConfig || opts.Profile != "" {
			return err
		}
		cfg = nil
	}
	if cfg != nil {
		ApplyProfileValues(opts, changed, cfg.Defaults)
	}

	if opts.Mode != "" {
		mode, ok := BuiltinDrainModes[opts.Mode]
		if !ok {
			return fmt.Errorf("invalid --mode %q (must be one of: %s)", opts.Mode, strings.Join(BuiltinModeNames(), ", "))
		}
		ApplyProfileValues(opts, changed, mode)
	}

	if opts.Profile != "" {
		if cfg == nil {
			return fmt.Errorf("profile %q requested but config file %q was not loaded", opts.Profile, cfgPath)
		}
		prof, err := cfg.GetProfile(opts.Profile)
		if err != nil {
			return err
		}
		ApplyProfileValues(opts, changed, prof)
	}
	return nil
}

// BuiltinDrainModes are the named drain mode defaults.
var BuiltinDrainModes = map[string]config.Profile{
	"prod": {
		Preflight:         config.PreflightMode(drain.PreflightModeStrict),
		Timeout:           durationPtr(45 * time.Minute),
		MaxConcurrency:    intPtr(1),
		NodeConcurrency:   intPtr(1),
		UncordonOnFailure: boolPtr(true),
		EmitEvents:        boolPtr(true),
	},
	"scale-down": {
		Preflight:         config.PreflightMode(drain.PreflightModeWarn),
		RolloutTimeout:    durationPtr(6 * time.Minute),
		PodVacateTimeout:  durationPtr(2 * time.Minute),
		EvictionTimeout:   durationPtr(2 * time.Minute),
		MaxConcurrency:    intPtr(2),
		NodeConcurrency:   intPtr(5),
		UncordonOnFailure: boolPtr(true),
	},
	"debug": {
		Preflight:    config.PreflightMode(drain.PreflightModeWarn),
		DryRun:       boolPtr(true),
		PollInterval: durationPtr(1 * time.Second),
		Timeout:      durationPtr(10 * time.Minute),
	},
}

func durationPtr(d time.Duration) *config.Duration { return &config.Duration{D: d} }
func intPtr(v int) *int                            { return &v }
func boolPtr(v bool) *bool                         { return &v }

// BuiltinModeNames returns the sorted built-in drain mode names.
func BuiltinModeNames() []string {
	names := make([]string, 0, len(BuiltinDrainModes))
	for name := range BuiltinDrainModes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ApplyProfileValues applies profile-like values for any scalar flag that was
// not explicitly set on the command line. Stateful name patterns are additive:
// config defaults, modes, profiles, and CLI values all extend the built-in list.
func ApplyProfileValues(opts *Options, changed FlagChanged, prof config.Profile) {
	if prof.Timeout != nil && !changed("timeout") {
		opts.Timeout = prof.Timeout.D
	}
	if prof.RolloutTimeout != nil && !changed("rollout-timeout") {
		opts.RolloutTimeout = prof.RolloutTimeout.D
	}
	if prof.PodVacateTimeout != nil && !changed("pod-vacate-timeout") {
		opts.PodVacateTimeout = prof.PodVacateTimeout.D
	}
	if prof.EvictionTimeout != nil && !changed("eviction-timeout") {
		opts.EvictionTimeout = prof.EvictionTimeout.D
	}
	if prof.PDBRetryInterval != nil && !changed("pdb-retry-interval") {
		opts.PDBRetryInterval = prof.PDBRetryInterval.D
	}
	if prof.PollInterval != nil && !changed("poll-interval") {
		opts.PollInterval = prof.PollInterval.D
	}
	if prof.MaxConcurrency != nil && !changed("max-concurrency") {
		opts.MaxConcurrency = *prof.MaxConcurrency
	}
	if prof.NodeConcurrency != nil && !changed("node-concurrency") {
		opts.NodeConcurrency = *prof.NodeConcurrency
	}
	if prof.Preflight != "" && !changed("preflight") {
		opts.Preflight = string(prof.Preflight)
	}
	if prof.LogFormat != "" && !changed("log-format") {
		opts.LogFormat = prof.LogFormat
	}
	if prof.DryRun != nil && !changed("dry-run") {
		opts.DryRun = *prof.DryRun
	}
	if prof.Force != nil && !changed("force") {
		opts.Force = *prof.Force
	}
	if prof.IgnoreDaemonSets != nil && !changed("ignore-daemonsets") {
		opts.SkipDaemonSets = *prof.IgnoreDaemonSets
	}
	if prof.DeleteEmptyDir != nil && !changed("delete-emptydir-data") {
		opts.DeleteEmptyDir = *prof.DeleteEmptyDir
	}
	if prof.ForceDeleteStandalone != nil && !changed("force-delete-standalone") {
		opts.ForceDeleteStandalone = *prof.ForceDeleteStandalone
	}
	if prof.UncordonOnFailure != nil && !changed("uncordon-on-failure") {
		opts.UncordonOnFailure = *prof.UncordonOnFailure
	}
	if prof.EmitEvents != nil && !changed("emit-events") {
		opts.EmitEvents = *prof.EmitEvents
	}
	if len(prof.StatefulNamePatterns) > 0 {
		opts.StatefulNamePatterns = append(opts.StatefulNamePatterns, prof.StatefulNamePatterns...)
	}
}

func sliceToSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
