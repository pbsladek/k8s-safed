package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/pbsladek/k8s-safed/internal/drainapp"
	"github.com/spf13/cobra"
)

type drainOptions struct {
	dryRun                bool
	timeout               time.Duration
	skipDaemonSets        bool
	deleteEmptyDir        bool
	gracePeriod           int32
	rolloutTimeout        time.Duration
	podVacateTimeout      time.Duration
	evictionTimeout       time.Duration
	pdbRetryInterval      time.Duration
	pollInterval          time.Duration
	force                 bool
	forceDeleteStandalone bool
	maxConcurrency        int
	logFormat             string
	uncordonOnFailure     bool
	// Multi-node options.
	nodeSelector    string
	nodeConcurrency int
	// Pre-flight options.
	preflight string
	// Workload filtering.
	skipWorkloads []string
	onlyWorkloads []string
	// Profile support.
	profile    string
	configFile string
	mode       string
	// Organization/application conventions.
	statefulNamePatterns []string
	// Event emission.
	emitEvents bool
	// Checkpoint / resume.
	resume         bool
	checkpointPath string
}

func newDrainCmd() *cobra.Command {
	opts := &drainOptions{}

	cmd := &cobra.Command{
		Use:   "drain NODE [NODE...] [--selector SELECTOR]",
		Short: "Safely drain one or more nodes using rolling restarts",
		Long: `Safely drain one or more Kubernetes nodes by triggering rolling restarts on workloads.

This command cordons each node then performs rolling restarts on all Deployments
and StatefulSets that have pods scheduled on it. Rolling restarts allow the
scheduler to place new pods on healthy nodes before terminating old ones,
avoiding the downtime caused by direct pod eviction.

Before making any cluster changes, pre-flight checks surface downtime risks
(single-replica Deployments, Recreate strategy, etc.) and known stateful services.
Use --preflight=strict to abort when any risk is detected, or --preflight=off to
skip checks entirely.

After all managed workloads have been restarted and their pods have migrated,
any remaining unmanaged pods are evicted conventionally.

Examples:
  # Dry-run drain of node worker-1
  kubectl safed drain worker-1 --dry-run

  # Drain multiple nodes sequentially
  kubectl safed drain worker-1 worker-2 worker-3

  # Drain all nodes matching a label selector
  kubectl safed drain --selector node-pool=spot

  # Drain two nodes in parallel
  kubectl safed drain worker-1 worker-2 --node-concurrency=2

  # Abort drain if any downtime risk is detected
  kubectl safed drain worker-1 --preflight=strict`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && opts.nodeSelector == "" {
				return fmt.Errorf("must specify at least one node name or --selector")
			}
			if len(args) > 0 && opts.nodeSelector != "" {
				return fmt.Errorf("cannot specify both node names and --selector")
			}
			if len(opts.skipWorkloads) > 0 && len(opts.onlyWorkloads) > 0 {
				return fmt.Errorf("cannot use both --skip-workload and --only-workload")
			}
			return runDrain(cmd, args, opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.dryRun, "dry-run", "d", false, "Preview actions without making changes")
	cmd.Flags().DurationVarP(&opts.timeout, "timeout", "t", 0, "Maximum time to wait for each node to be drained (0 = no timeout)")
	cmd.Flags().BoolVar(&opts.skipDaemonSets, "ignore-daemonsets", true, "Skip DaemonSet-managed pods")
	// --skip-daemon-sets is the old name; keep it as a hidden alias.
	cmd.Flags().BoolVar(&opts.skipDaemonSets, "skip-daemon-sets", true, "")
	_ = cmd.Flags().MarkHidden("skip-daemon-sets")
	cmd.Flags().BoolVar(&opts.deleteEmptyDir, "delete-emptydir-data", false, "Delete pods using emptyDir volumes")
	cmd.Flags().Int32Var(&opts.gracePeriod, "grace-period", -1, "Pod termination grace period in seconds (-1 uses pod default)")
	cmd.Flags().DurationVar(&opts.rolloutTimeout, "rollout-timeout", 5*time.Minute, "Per-workload timeout waiting for rolling restart to complete (0 = no per-workload limit, only --timeout applies)")
	cmd.Flags().DurationVar(&opts.podVacateTimeout, "pod-vacate-timeout", 2*time.Minute, "Per-workload timeout waiting for pods to leave the node after rollout")
	cmd.Flags().DurationVar(&opts.evictionTimeout, "eviction-timeout", 5*time.Minute, "Per-pod timeout for evictions blocked by a PodDisruptionBudget")
	cmd.Flags().DurationVar(&opts.pdbRetryInterval, "pdb-retry-interval", 5*time.Second, "Base retry interval when eviction is blocked by a PDB (doubles on each attempt, capped at 60s)")
	cmd.Flags().DurationVar(&opts.pollInterval, "poll-interval", 5*time.Second, "Interval between status checks in all wait loops")
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Force drain even if there are unmanaged pods")
	cmd.Flags().BoolVar(&opts.forceDeleteStandalone, "force-delete-standalone", false,
		"Force-delete standalone pods (no owner) with gracePeriodSeconds=0 instead of evicting them. Implies --force.")
	cmd.Flags().IntVar(&opts.maxConcurrency, "max-concurrency", 1,
		"Number of workloads to rolling-restart concurrently per node (1 = sequential, 0 = all at once, N = batches of N)")
	cmd.Flags().StringVarP(&opts.logFormat, "log-format", "o", "plain",
		`Log output format: "plain" (human-readable, grepable) or "json" (one object per line for log aggregators)`)
	cmd.Flags().BoolVar(&opts.uncordonOnFailure, "uncordon-on-failure", false,
		"Uncordon the node if the drain fails (only applies when this run cordoned the node)")
	cmd.Flags().StringVarP(&opts.nodeSelector, "selector", "l", "",
		"Label selector to target nodes (e.g. node-pool=spot). Mutually exclusive with positional node names.")
	cmd.Flags().IntVar(&opts.nodeConcurrency, "node-concurrency", 1,
		"Number of nodes to drain in parallel (1 = sequential, default). Use with care on production clusters.")
	cmd.Flags().StringVar(&opts.preflight, "preflight", "warn",
		`Pre-flight check mode: "warn" (log risks, continue), "strict" (abort on any risk), "off" (skip all checks)`)
	cmd.Flags().StringArrayVar(&opts.skipWorkloads, "skip-workload", nil,
		`Leave a managed workload untouched by restart and conventional eviction (format: Kind/namespace/name, e.g. Deployment/default/api). Repeatable. Mutually exclusive with --only-workload.`)
	cmd.Flags().StringArrayVar(&opts.onlyWorkloads, "only-workload", nil,
		`Restart only these managed workloads and leave other managed workloads untouched (format: Kind/namespace/name). Repeatable. Mutually exclusive with --skip-workload.`)
	cmd.Flags().StringVar(&opts.profile, "profile", "",
		`Load flag defaults from a named profile in the safed config file (see --config). CLI flags override profile values.`)
	cmd.Flags().StringVar(&opts.configFile, "config", "",
		`Path to the safed config file (default: ~/.kube/safed.yaml; env: KUBECTL_SAFED_CONFIG)`)
	cmd.Flags().StringVar(&opts.mode, "mode", "",
		`Use a built-in drain mode: "prod", "scale-down", or "debug". CLI flags override mode values.`)
	cmd.Flags().StringArrayVar(&opts.statefulNamePatterns, "stateful-name-pattern", nil,
		`Add a custom pre-flight stateful workload name pattern. Repeatable. Also supported in config as stateful-name-patterns.`)
	cmd.Flags().BoolVar(&opts.emitEvents, "emit-events", false,
		"Emit Kubernetes Events to node and workload objects during drain (requires events/create RBAC permission)")
	cmd.Flags().BoolVar(&opts.resume, "resume", false,
		"Resume a previously interrupted drain, skipping workloads already recorded as complete in the checkpoint file")
	cmd.Flags().StringVar(&opts.checkpointPath, "checkpoint-path", "",
		"Override the checkpoint file path (default: ~/.kube/safed-checkpoints/<context>-<node>.json)")

	return cmd
}

// NewDrainCommand returns a fresh drain command with its own option state.
// It is primarily useful for tests and documentation validation that need to
// inspect the public command surface without using the package-level root.
func NewDrainCommand() *cobra.Command {
	return newDrainCmd()
}

func toAppOptions(opts *drainOptions) drainapp.Options {
	return drainapp.Options{
		DryRun:                opts.dryRun,
		Timeout:               opts.timeout,
		SkipDaemonSets:        opts.skipDaemonSets,
		DeleteEmptyDir:        opts.deleteEmptyDir,
		GracePeriod:           opts.gracePeriod,
		RolloutTimeout:        opts.rolloutTimeout,
		PodVacateTimeout:      opts.podVacateTimeout,
		EvictionTimeout:       opts.evictionTimeout,
		PDBRetryInterval:      opts.pdbRetryInterval,
		PollInterval:          opts.pollInterval,
		Force:                 opts.force,
		ForceDeleteStandalone: opts.forceDeleteStandalone,
		MaxConcurrency:        opts.maxConcurrency,
		LogFormat:             opts.logFormat,
		UncordonOnFailure:     opts.uncordonOnFailure,
		NodeSelector:          opts.nodeSelector,
		NodeConcurrency:       opts.nodeConcurrency,
		Preflight:             opts.preflight,
		SkipWorkloads:         opts.skipWorkloads,
		OnlyWorkloads:         opts.onlyWorkloads,
		Profile:               opts.profile,
		ConfigFile:            opts.configFile,
		Mode:                  opts.mode,
		StatefulNamePatterns:  opts.statefulNamePatterns,
		EmitEvents:            opts.emitEvents,
		Resume:                opts.resume,
		CheckpointPath:        opts.checkpointPath,
	}
}

func applyAppOptions(opts *drainOptions, appOpts drainapp.Options) {
	opts.dryRun = appOpts.DryRun
	opts.timeout = appOpts.Timeout
	opts.skipDaemonSets = appOpts.SkipDaemonSets
	opts.deleteEmptyDir = appOpts.DeleteEmptyDir
	opts.gracePeriod = appOpts.GracePeriod
	opts.rolloutTimeout = appOpts.RolloutTimeout
	opts.podVacateTimeout = appOpts.PodVacateTimeout
	opts.evictionTimeout = appOpts.EvictionTimeout
	opts.pdbRetryInterval = appOpts.PDBRetryInterval
	opts.pollInterval = appOpts.PollInterval
	opts.force = appOpts.Force
	opts.forceDeleteStandalone = appOpts.ForceDeleteStandalone
	opts.maxConcurrency = appOpts.MaxConcurrency
	opts.logFormat = appOpts.LogFormat
	opts.uncordonOnFailure = appOpts.UncordonOnFailure
	opts.nodeSelector = appOpts.NodeSelector
	opts.nodeConcurrency = appOpts.NodeConcurrency
	opts.preflight = appOpts.Preflight
	opts.skipWorkloads = appOpts.SkipWorkloads
	opts.onlyWorkloads = appOpts.OnlyWorkloads
	opts.profile = appOpts.Profile
	opts.configFile = appOpts.ConfigFile
	opts.mode = appOpts.Mode
	opts.statefulNamePatterns = appOpts.StatefulNamePatterns
	opts.emitEvents = appOpts.EmitEvents
	opts.resume = appOpts.Resume
	opts.checkpointPath = appOpts.CheckpointPath
}

func runDrain(cmd *cobra.Command, nodeArgs []string, opts *drainOptions) error {
	appOpts := toAppOptions(opts)
	if err := drainapp.Run(cmd.Context(), kubeConfigFlags, os.Stdout, nodeArgs, &appOpts, cmd.Flags().Changed); err != nil {
		return err
	}
	applyAppOptions(opts, appOpts)
	return nil
}

func validateDrainTargets(opts *drainOptions, nodes []string) error {
	return drainapp.ValidateTargets(toAppOptions(opts), nodes)
}

func validateDrainOptions(opts *drainOptions) error {
	return drainapp.ValidateOptions(toAppOptions(opts))
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

// applyConfig applies defaults in this order:
// built-in flag defaults -> config defaults -> built-in mode -> named profile -> CLI flags.
func applyConfig(cmd *cobra.Command, opts *drainOptions) error {
	appOpts := toAppOptions(opts)
	err := drainapp.ApplyConfig(&appOpts, cmd.Flags().Changed)
	applyAppOptions(opts, appOpts)
	return err
}
