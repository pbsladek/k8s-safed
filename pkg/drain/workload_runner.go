package drain

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// runWorkloads dispatches rolling restarts according to MaxConcurrency.
//
// Sequential (MaxConcurrency == 1): workloads run one at a time with step
// counters in the log output.
//
// Batch (MaxConcurrency > 1): workloads are grouped into batches of that size.
// All workloads in a batch start concurrently; the batch must fully complete
// before the next one begins. The first error in a batch cancels all siblings
// in that batch via errgroup context cancellation (fail-fast).
//
// Fully parallel (MaxConcurrency == 0): treated as a single batch containing
// all workloads. Carries the same fail-fast guarantee.
func (d *Drainer) runWorkloads(ctx context.Context, workloads []workload.Workload) error {
	if len(workloads) == 0 {
		return nil
	}

	// Sort by annotation priority (lower = first). SliceStable preserves
	// discovery order within the same priority level.
	sort.SliceStable(workloads, func(i, j int) bool {
		return workloads[i].Priority < workloads[j].Priority
	})

	// Load checkpoint if resuming. Filter out already-completed workloads.
	var cp *Checkpoint
	if d.opts.Resume && d.opts.CheckpointPath != "" {
		var err error
		cp, err = LoadCheckpoint(d.opts.CheckpointPath)
		if err != nil {
			return fmt.Errorf("loading checkpoint: %w", err)
		}
		if err := d.validateCheckpoint(cp); err != nil {
			return err
		}
		remaining := workloads[:0:0]
		for _, w := range workloads {
			skip, err := d.checkpointCanSkip(ctx, cp, w)
			if err != nil {
				return err
			}
			if skip {
				d.opts.Out.Infof(d.opts.NodeName, "Skipping %s (already completed per checkpoint)", wSubject(w))
				continue
			}
			remaining = append(remaining, w)
		}
		workloads = remaining
		if len(workloads) == 0 {
			d.opts.Out.Info(d.opts.NodeName, "All workloads already completed per checkpoint")
			return nil
		}
	}
	if cp == nil {
		cp = newCheckpoint()
	}
	cp.NodeName = d.opts.NodeName
	cp.Context = d.opts.CheckpointContext

	// saveCP persists the checkpoint after each successful workload (best-effort).
	var cpMu sync.Mutex
	saveCP := func(w workload.Workload) {
		if d.opts.CheckpointPath == "" || d.opts.DryRun {
			return
		}
		cpMu.Lock()
		defer cpMu.Unlock()
		cp.NodeName = d.opts.NodeName
		cp.Context = d.opts.CheckpointContext
		cp.MarkDone(w)
		if err := cp.Save(d.opts.CheckpointPath); err != nil {
			d.opts.Out.Warnf(d.opts.NodeName, "failed to save checkpoint: %v", err)
		}
	}

	maxC := d.opts.MaxConcurrency
	out := d.opts.Out

	// Sequential path: one workload at a time with step counters.
	if maxC == 1 {
		for i, w := range workloads {
			t0 := time.Now()
			out.Startf(wSubject(w), "Rolling restart [%d/%d]", i+1, len(workloads))
			if err := d.rollingRestart(ctx, w); err != nil {
				return err
			}
			saveCP(w)
			if !d.opts.DryRun {
				out.Elapsed(t0, wSubject(w), "Complete")
			}
		}
		return nil
	}

	// Parallel / batch path.
	if maxC <= 0 {
		maxC = len(workloads) // 0 = unlimited
		out.Warnf(d.opts.NodeName, "--max-concurrency=0 restarts all %d workload(s) concurrently; monitor API server and rollout capacity", maxC)
	} else if maxC > 10 {
		out.Warnf(d.opts.NodeName, "workload concurrency is %d; high parallelism can create API and rollout pressure", maxC)
	}
	totalBatches := (len(workloads) + maxC - 1) / maxC

	for batchStart := 0; batchStart < len(workloads); batchStart += maxC {
		end := min(batchStart+maxC, len(workloads))
		batch := workloads[batchStart:end]
		batchNum := batchStart/maxC + 1

		if totalBatches > 1 {
			out.Infof(d.opts.NodeName, "batch %d/%d: starting %d workload(s) concurrently",
				batchNum, totalBatches, len(batch))
		} else {
			out.Infof(d.opts.NodeName, "Starting all %d workload(s) concurrently", len(batch))
		}
		for _, w := range batch {
			out.Infof(d.opts.NodeName, "  · %s", w)
		}

		g, gctx := errgroup.WithContext(ctx)
		for _, w := range batch {
			w := w // capture loop variable
			g.Go(func() error {
				t0 := time.Now()
				out.Start(wSubject(w), "Rolling restart")
				if err := d.rollingRestart(gctx, w); err != nil {
					return err
				}
				saveCP(w)
				if !d.opts.DryRun {
					out.Elapsed(t0, wSubject(w), "Complete")
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}

		if totalBatches > 1 {
			out.Infof(d.opts.NodeName, "batch %d/%d: all %d workload(s) complete",
				batchNum, totalBatches, len(batch))
		}
	}
	return nil
}
