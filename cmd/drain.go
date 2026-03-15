package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pbsladek/k8s-safed/pkg/drain"
	"github.com/pbsladek/k8s-safed/pkg/k8s"
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
			return runDrain(cmd.Context(), args, opts)
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

	return cmd
}

func runDrain(ctx context.Context, nodeArgs []string, opts *drainOptions) error {
	client, err := k8s.NewClient(kubeConfigFlags)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	nodes, err := resolveNodeNames(ctx, client, nodeArgs, opts.nodeSelector)
	if err != nil {
		return err
	}

	// --force-delete-standalone implies --force (standalone pods require force).
	force := opts.force || opts.forceDeleteStandalone

	out := drain.NewPrinterWithFormat(os.Stdout, drain.LogFormat(opts.logFormat))

	drainNode := func(ctx context.Context, nodeName string) error {
		drainer := drain.NewDrainer(drain.Options{
			Client:                client,
			NodeName:              nodeName,
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
			Force:                 force,
			ForceDeleteStandalone: opts.forceDeleteStandalone,
			MaxConcurrency:        opts.maxConcurrency,
			Out:                   out,
			UncordonOnFailure:     opts.uncordonOnFailure,
			Preflight:             drain.PreflightMode(opts.preflight),
		})
		return drainer.Run(ctx)
	}

	concurrency := opts.nodeConcurrency
	if concurrency <= 0 {
		concurrency = len(nodes)
	}

	// Sequential fast-path.
	if concurrency == 1 {
		for _, node := range nodes {
			if err := drainNode(ctx, node); err != nil {
				return err
			}
		}
		return nil
	}

	// Parallel / batch path — process nodes in batches of `concurrency`.
	for batchStart := 0; batchStart < len(nodes); batchStart += concurrency {
		end := batchStart + concurrency
		if end > len(nodes) {
			end = len(nodes)
		}
		batch := nodes[batchStart:end]

		g, gctx := errgroup.WithContext(ctx)
		for _, nodeName := range batch {
			nodeName := nodeName // capture loop variable
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

// resolveNodeNames returns the list of node names to drain. When nodeSelector
// is non-empty, it lists nodes matching that label selector; otherwise it
// returns nodeArgs directly.
func resolveNodeNames(ctx context.Context, client *k8s.Client, nodeArgs []string, nodeSelector string) ([]string, error) {
	if nodeSelector == "" {
		return nodeArgs, nil
	}

	list, err := client.Kubernetes.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: nodeSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodes with selector %q: %w", nodeSelector, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no nodes matched selector %q", nodeSelector)
	}

	names := make([]string, len(list.Items))
	for i, n := range list.Items {
		names[i] = n.Name
	}
	return names, nil
}
