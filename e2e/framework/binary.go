//go:build e2e

package framework

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Binary wraps the kubectl-safed binary under test.
type Binary struct {
	// Path is the absolute path to the compiled kubectl-safed binary.
	Path string
	// KubeconfigPath is passed as --kubeconfig to every invocation.
	KubeconfigPath string
}

// DrainResult holds the output of a drain invocation.
type DrainResult struct {
	Stdout string
	Stderr string
	// Err is non-nil when the process exited with a non-zero status.
	Err error
}

// Drain runs `kubectl-safed drain <node> [flags...]` and returns the combined output.
func (b *Binary) Drain(ctx context.Context, node string, flags ...string) DrainResult {
	args := append([]string{
		"drain",
		"--kubeconfig", b.KubeconfigPath,
		node,
	}, flags...)
	return b.run(ctx, args...)
}

// DrainNodes runs `kubectl-safed drain <nodes...> [flags...]`.
func (b *Binary) DrainNodes(ctx context.Context, nodes []string, flags ...string) DrainResult {
	args := append(append([]string{
		"drain",
		"--kubeconfig", b.KubeconfigPath,
	}, nodes...), flags...)
	return b.run(ctx, args...)
}

// DrainWithSelector runs `kubectl-safed drain --selector=<selector> [flags...]`.
func (b *Binary) DrainWithSelector(ctx context.Context, selector string, flags ...string) DrainResult {
	args := append([]string{
		"drain",
		"--kubeconfig", b.KubeconfigPath,
		"--selector", selector,
	}, flags...)
	return b.run(ctx, args...)
}

func (b *Binary) run(ctx context.Context, args ...string) DrainResult {
	cmd := exec.CommandContext(ctx, b.Path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return DrainResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Err:    err,
	}
}

// BuildBinary compiles the kubectl-safed binary from the module root into a
// temp directory. Returns the path to the compiled binary.
func BuildBinary(moduleRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "kubectl-safed-e2e-bin-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for binary: %w", err)
	}

	binPath := filepath.Join(dir, "kubectl-safed")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build: %w", err)
	}
	return binPath, nil
}

// ApplyManifest applies a YAML manifest to the cluster using kubectl.
func ApplyManifest(ctx context.Context, kubeconfigPath, yaml string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(yaml)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply: %w\n%s", err, out)
	}
	return nil
}

// DeleteManifest deletes resources defined in the YAML manifest.
func DeleteManifest(ctx context.Context, kubeconfigPath, yaml string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"delete", "-f", "-",
		"--ignore-not-found",
	)
	cmd.Stdin = strings.NewReader(yaml)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl delete: %w\n%s", err, out)
	}
	return nil
}

// UncordonNode uncordons a node so the cluster is clean between tests.
func UncordonNode(ctx context.Context, kubeconfigPath, nodeName string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"uncordon", nodeName,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl uncordon %s: %w\n%s", nodeName, err, out)
	}
	return nil
}
