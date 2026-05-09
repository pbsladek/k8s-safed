package drain

import (
	"context"
	"fmt"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

func (d *Drainer) validateCheckpoint(cp *Checkpoint) error {
	if cp.NodeName != "" && cp.NodeName != d.opts.NodeName {
		return fmt.Errorf("checkpoint is for node %q, not %q", cp.NodeName, d.opts.NodeName)
	}
	if cp.Context != "" && d.opts.CheckpointContext != "" && cp.Context != d.opts.CheckpointContext {
		return fmt.Errorf("checkpoint is for kube context %q, not %q", cp.Context, d.opts.CheckpointContext)
	}
	return nil
}

func (d *Drainer) checkpointCanSkip(ctx context.Context, cp *Checkpoint, w workload.Workload) (bool, error) {
	if !cp.IsDone(w) {
		return false, nil
	}
	if meta, ok := cp.Work(w); ok {
		if meta.UID != "" && w.UID != "" && meta.UID != string(w.UID) {
			d.opts.Out.Warnf(wSubject(w), "checkpoint entry UID changed; restarting workload")
			return false, nil
		}
		if meta.Generation != 0 && w.Generation != 0 && meta.Generation != w.Generation {
			d.opts.Out.Warnf(wSubject(w), "checkpoint entry generation changed; restarting workload")
			return false, nil
		}
		return true, nil
	}

	// Legacy checkpoints do not include workload identity. Only trust them when
	// this workload no longer has pods on the target node.
	hasPods, err := d.workloadHasPodsOnNode(ctx, w)
	if err != nil {
		return false, fmt.Errorf("validating legacy checkpoint entry for %s: %w", wSubject(w), err)
	}
	if hasPods {
		d.opts.Out.Warnf(wSubject(w), "legacy checkpoint entry found but pods remain on node; restarting workload")
		return false, nil
	}
	return true, nil
}
