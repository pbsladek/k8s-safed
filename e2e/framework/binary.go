//go:build e2e

package framework

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	Command string
	Stdout  string
	Stderr  string
	// Err is non-nil when the process exited with a non-zero status.
	Err error
}

// RunningDrain wraps a kubectl-safed process that was started asynchronously.
type RunningDrain struct {
	ctx     context.Context
	command string
	cmd     *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
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

// DrainRaw runs `kubectl-safed drain --kubeconfig <path> [args...]`.
func (b *Binary) DrainRaw(ctx context.Context, args ...string) DrainResult {
	allArgs := append([]string{
		"drain",
		"--kubeconfig", b.KubeconfigPath,
	}, args...)
	return b.run(ctx, allArgs...)
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

// StartDrain starts `kubectl-safed drain <node> [flags...]` and returns before
// the process exits. Call Wait to collect output and reap the process.
func (b *Binary) StartDrain(ctx context.Context, node string, flags ...string) (*RunningDrain, error) {
	args := append([]string{
		"drain",
		"--kubeconfig", b.KubeconfigPath,
		node,
	}, flags...)
	return b.start(ctx, args...)
}

func (b *Binary) run(ctx context.Context, args ...string) DrainResult {
	proc, err := b.start(ctx, args...)
	if err != nil {
		command := b.Path + " " + strings.Join(args, " ")
		return DrainResult{Command: command, Err: err}
	}
	return proc.Wait()
}

func (b *Binary) start(ctx context.Context, args ...string) (*RunningDrain, error) {
	cmd := exec.CommandContext(ctx, b.Path, args...)
	proc := &RunningDrain{
		ctx:     ctx,
		command: b.Path + " " + strings.Join(args, " "),
		cmd:     cmd,
	}
	if os.Getenv("SAFED_E2E_STREAM") != "" {
		fmt.Fprintf(os.Stderr, "[e2e] running: %s\n", proc.command)
		cmd.Stdout = io.MultiWriter(&proc.stdout, os.Stderr)
		cmd.Stderr = io.MultiWriter(&proc.stderr, os.Stderr)
	} else {
		cmd.Stdout = &proc.stdout
		cmd.Stderr = &proc.stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", proc.command, err)
	}
	return proc, nil
}

// Kill sends SIGKILL to the running drain process.
func (p *RunningDrain) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// Wait waits for the drain process to exit and returns its captured output.
func (p *RunningDrain) Wait() DrainResult {
	err := p.cmd.Wait()
	if err != nil {
		err = enrichCommandError(p.ctx, p.command, err)
	}
	return DrainResult{
		Command: p.command,
		Stdout:  p.stdout.String(),
		Stderr:  p.stderr.String(),
		Err:     err,
	}
}

func enrichCommandError(ctx context.Context, command string, err error) error {
	msg := fmt.Sprintf("command %q failed: %v", command, err)
	if ctxErr := ctx.Err(); ctxErr != nil {
		msg += fmt.Sprintf(" (context: %v)", ctxErr)
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ProcessState != nil {
		msg += fmt.Sprintf(" (exitCode=%d", exitErr.ProcessState.ExitCode())
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			msg += fmt.Sprintf(" signal=%s", status.Signal())
		}
		msg += ")"
	}
	return errors.New(msg)
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

// CordonNode cordons a node using kubectl.
func CordonNode(ctx context.Context, kubeconfigPath, nodeName string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"cordon", nodeName,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl cordon %s: %w\n%s", nodeName, err, out)
	}
	return nil
}
