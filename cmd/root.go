package cmd

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

var kubeConfigFlags *genericclioptions.ConfigFlags

var rootCmd = &cobra.Command{
	Use:   "kubectl-safed",
	Short: "Safe Kubernetes node drain with rolling restarts",
	Long: `kubectl-safed is a kubectl plugin that drains Kubernetes nodes safely.

Instead of evicting pods directly (which can cause downtime), it:
  1. Cordons the target node to prevent new scheduling
  2. Identifies all Deployments and StatefulSets with pods on the node
  3. Triggers rolling restarts so pods migrate gracefully to other nodes
  4. Waits for rollouts to complete before proceeding
  5. Handles remaining pods (DaemonSets, standalone) conventionally`,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	kubeConfigFlags = genericclioptions.NewConfigFlags(true)
	kubeConfigFlags.AddFlags(rootCmd.PersistentFlags())

	rootCmd.AddCommand(newDrainCmd())
}
