package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/pbsladek/k8s-safed/pkg/drain"
	"github.com/pbsladek/k8s-safed/pkg/k8s"
)

type drainOptions struct {
	nodeName        string
	dryRun          bool
	timeout         time.Duration
	skipDaemonSets  bool
	deleteEmptyDir  bool
	gracePeriod     int32
	rolloutTimeout  time.Duration
	force           bool
}

func newDrainCmd() *cobra.Command {
	opts := &drainOptions{}

	cmd := &cobra.Command{
		Use:   "drain NODE",
		Short: "Safely drain a node using rolling restarts",
		Long: `Safely drain a Kubernetes node by triggering rolling restarts on workloads.

This command cordons the node then performs rolling restarts on all Deployments
and StatefulSets that have pods scheduled on it. Rolling restarts allow the
scheduler to place new pods on healthy nodes before terminating old ones,
avoiding the downtime caused by direct pod eviction.

After all managed workloads have been restarted and their pods have migrated,
any remaining unmanaged pods are evicted conventionally.

Examples:
  # Dry-run drain of node worker-1
  kubectl safed drain worker-1 --dry-run

  # Drain node worker-1, skipping DaemonSet pods
  kubectl safed drain worker-1 --skip-daemon-sets

  # Drain with a custom rollout wait timeout
  kubectl safed drain worker-1 --rollout-timeout=10m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.nodeName = args[0]
			return runDrain(cmd.Context(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Preview actions without making changes")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "Maximum time to wait for the node to be drained (0 = no timeout)")
	cmd.Flags().BoolVar(&opts.skipDaemonSets, "skip-daemon-sets", true, "Skip DaemonSet-managed pods")
	cmd.Flags().BoolVar(&opts.deleteEmptyDir, "delete-emptydir-data", false, "Delete pods using emptyDir volumes")
	cmd.Flags().Int32Var(&opts.gracePeriod, "grace-period", -1, "Pod termination grace period in seconds (-1 uses pod default)")
	cmd.Flags().DurationVar(&opts.rolloutTimeout, "rollout-timeout", 5*time.Minute, "Timeout waiting for each rolling restart to complete")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Force drain even if there are unmanaged pods")

	return cmd
}

func runDrain(ctx context.Context, opts *drainOptions) error {
	client, err := k8s.NewClient(kubeConfigFlags)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	drainer := drain.NewDrainer(drain.Options{
		Client:         client,
		NodeName:       opts.nodeName,
		DryRun:         opts.dryRun,
		Timeout:        opts.timeout,
		SkipDaemonSets: opts.skipDaemonSets,
		DeleteEmptyDir: opts.deleteEmptyDir,
		GracePeriod:    opts.gracePeriod,
		RolloutTimeout: opts.rolloutTimeout,
		Force:          opts.force,
		Out:            drain.NewPrinter(),
	})

	return drainer.Run(ctx)
}
