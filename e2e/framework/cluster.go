//go:build e2e

package framework

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// DefaultClusterName is the k3d cluster name used for local e2e runs.
	// Override with SAFED_E2E_CLUSTER_NAME for CI isolation.
	DefaultClusterName = "safed-e2e"
	// E2ENamespace is the namespace where test workloads are deployed.
	E2ENamespace = "e2e"
)

// ClusterName returns the cluster name from SAFED_E2E_CLUSTER_NAME if set,
// otherwise DefaultClusterName. Use this everywhere instead of the constant
// so CI jobs with concurrent runs stay isolated.
func ClusterName() string {
	if name := os.Getenv("SAFED_E2E_CLUSTER_NAME"); name != "" {
		return name
	}
	return DefaultClusterName
}

// Cluster manages a k3d test cluster.
type Cluster struct {
	Name           string
	KubeconfigPath string
}

// NewCluster returns a Cluster with the given name.
func NewCluster(name string) *Cluster {
	return &Cluster{Name: name}
}

// Create creates a k3d cluster with 1 server and 2 agents, then writes the
// kubeconfig to a temp file. The caller must call Destroy when done.
func (c *Cluster) Create(ctx context.Context) error {
	// Delete any stale cluster with the same name (ignore errors).
	_ = exec.CommandContext(ctx, "k3d", "cluster", "delete", c.Name).Run()

	args := []string{
		"cluster", "create", c.Name,
		"--agents", "2",
		"--wait",
		"--timeout", "120s",
		// Disable traefik/metrics-server to keep the cluster lightweight.
		"--k3s-arg", "--disable=traefik@server:*",
		"--k3s-arg", "--disable=metrics-server@server:*",
		// No load-balancer port mapping needed for these tests.
		"--no-lb",
	}
	// Allow CI to pin a specific k3s image via K3S_IMAGE env var.
	if img := os.Getenv("K3S_IMAGE"); img != "" {
		args = append(args, "--image", img)
	}
	cmd := exec.CommandContext(ctx, "k3d", args...)
	cmd.Stdout = os.Stderr // progress to stderr so test output stays clean
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k3d cluster create: %w", err)
	}

	// Write kubeconfig to a temp file.
	f, err := os.CreateTemp("", "safed-e2e-kubeconfig-*.yaml")
	if err != nil {
		return fmt.Errorf("create kubeconfig temp file: %w", err)
	}
	f.Close()
	c.KubeconfigPath = f.Name()

	writeCmd := exec.CommandContext(ctx, "k3d", "kubeconfig", "write", c.Name,
		"--output", c.KubeconfigPath)
	if out, err := writeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("k3d kubeconfig write: %w\n%s", err, out)
	}

	// Wait until all nodes are Ready.
	return c.waitNodesReady(ctx, 3, 60*time.Second)
}

// Destroy tears down the k3d cluster and removes the kubeconfig file.
func (c *Cluster) Destroy(ctx context.Context) error {
	if c.KubeconfigPath != "" {
		_ = os.Remove(c.KubeconfigPath)
	}
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "delete", c.Name)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k3d cluster delete: %w", err)
	}
	return nil
}

// NodeNames returns the Kubernetes node names for all nodes in the cluster.
func (c *Cluster) NodeNames(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", c.KubeconfigPath,
		"get", "nodes",
		"-o", "jsonpath={.items[*].metadata.name}",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes: %w", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil, fmt.Errorf("no nodes found in cluster")
	}
	return names, nil
}

// AgentNodeNames returns only the agent (worker) node names.
func (c *Cluster) AgentNodeNames(ctx context.Context) ([]string, error) {
	all, err := c.NodeNames(ctx)
	if err != nil {
		return nil, err
	}
	var agents []string
	for _, name := range all {
		// k3d agent nodes are named k3d-<cluster>-agent-N; server nodes are
		// k3d-<cluster>-server-N. Filter on the cluster name + "agent".
		if strings.Contains(name, c.Name+"-agent") {
			agents = append(agents, name)
		}
	}
	return agents, nil
}

// waitNodesReady waits until at least wantCount nodes report Ready=True using
// `kubectl wait`, which is more reliable than jsonpath on the conditions array.
func (c *Cluster) waitNodesReady(ctx context.Context, wantCount int, timeout time.Duration) error {
	// kubectl wait --for=condition=Ready blocks until the condition is met or
	// the timeout elapses. We run it in a poll loop so we can also check the
	// actual ready count (in case fewer nodes than expected joined the cluster).
	deadline := time.Now().Add(timeout)
	for {
		cmd := exec.CommandContext(ctx, "kubectl",
			"--kubeconfig", c.KubeconfigPath,
			"wait", "--for=condition=Ready", "node", "--all",
			fmt.Sprintf("--timeout=%ds", int(time.Until(deadline).Seconds())),
		)
		if err := cmd.Run(); err == nil {
			// All current nodes are Ready; verify we have enough of them.
			names, err := c.NodeNames(ctx)
			if err == nil && len(names) >= wantCount {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d nodes to be ready", wantCount)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
