package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

func (d *Drainer) validateNode(ctx context.Context) (*corev1.Node, error) {
	out := d.opts.Out
	out.Infof(d.opts.NodeName, "Validating %q", d.opts.NodeName)
	var node *corev1.Node
	if err := d.retryTransient(ctx, func() error {
		var e error
		node, e = d.nodes().Get(ctx, d.opts.NodeName, metav1.GetOptions{})
		return e
	}); err != nil {
		return nil, fmt.Errorf("node %q not found: %w", d.opts.NodeName, err)
	}
	out.Infof(d.opts.NodeName, "Found · kernel=%s ready=%v",
		node.Status.NodeInfo.KernelVersion, isNodeReady(node))
	return node, nil
}

func (d *Drainer) discoverWorkloads(ctx context.Context) ([]workload.Workload, error) {
	out := d.opts.Out
	out.Info(d.opts.NodeName, "Discovering managed workloads...")
	var workloads []workload.Workload
	if err := d.retryTransient(ctx, func() error {
		var e error
		workloads, e = d.finder.FindForNode(ctx, d.opts.NodeName)
		return e
	}); err != nil {
		return nil, fmt.Errorf("discovering workloads: %w", err)
	}

	if len(workloads) == 0 {
		out.Info(d.opts.NodeName, "No managed workloads found")
	} else {
		out.Infof(d.opts.NodeName, "Found %d managed workload(s) to restart:", len(workloads))
		for _, w := range workloads {
			out.Infof(d.opts.NodeName, "  · %s", w)
		}
	}
	return workloads, nil
}

func (d *Drainer) prepareBeforeCordon(ctx context.Context, state *runState, workloads []workload.Workload) ([]workload.Workload, error) {
	workloads = d.filterWorkloads(state, workloads)
	d.warnInvalidPriorityAnnotations(workloads)

	if d.opts.Preflight != PreflightModeOff {
		if err := d.runPreflight(ctx, workloads); err != nil {
			return nil, err
		}
	}

	if d.opts.Resume && d.opts.CheckpointPath != "" {
		cp, err := LoadCheckpoint(d.opts.CheckpointPath)
		if err != nil {
			return nil, fmt.Errorf("loading checkpoint: %w", err)
		}
		if err := d.validateCheckpoint(cp); err != nil {
			return nil, err
		}
	}
	return workloads, nil
}

func (d *Drainer) beginCordonedDrain(ctx context.Context, node *corev1.Node, workloadCount int) (bool, error) {
	cordonedByUs, err := d.cordon(ctx, node)
	if err != nil {
		return false, err
	}
	d.events.NodeEvent(ctx, d.opts.NodeName, "Draining",
		fmt.Sprintf("kubectl-safed: beginning drain of %q (%d workload(s))", d.opts.NodeName, workloadCount),
		corev1.EventTypeNormal)
	return cordonedByUs, nil
}

func (d *Drainer) finishSuccessfulDrain(start time.Time, state *runState) {
	out := d.opts.Out
	if d.opts.DryRun {
		out.DryRunf(d.opts.NodeName, "Dry-run complete — no changes were made to %q", d.opts.NodeName)
		return
	}

	elapsed := d.now().Sub(start).Round(time.Second)
	if len(state.protectedWorkloads) > 0 {
		out.Warnf(d.opts.NodeName, "%d filtered managed workload(s) were left untouched; node may still have pods by design", len(state.protectedWorkloads))
	}
	d.events.NodeEvent(context.Background(), d.opts.NodeName, "Drained",
		fmt.Sprintf("kubectl-safed: drain of %q complete (%s)", d.opts.NodeName, elapsed),
		corev1.EventTypeNormal)
	out.Elapsed(start, d.opts.NodeName, fmt.Sprintf("Drained %q", d.opts.NodeName))
}

func (d *Drainer) emitDrainFailed(err error) {
	d.events.NodeEvent(context.Background(), d.opts.NodeName, "DrainFailed",
		fmt.Sprintf("kubectl-safed: drain of %q failed: %v", d.opts.NodeName, err),
		corev1.EventTypeWarning)
}
