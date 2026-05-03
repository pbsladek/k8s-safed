// Package drain implements safe Kubernetes node draining via rolling restarts.
package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/pbsladek/k8s-safed/pkg/k8s"
	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// wSubject returns a "Kind/namespace/name" subject string for a workload.
func wSubject(w workload.Workload) string {
	return fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
}

// depSubject returns a "Deployment/namespace/name" subject string.
func depSubject(ns, name string) string { return "Deployment/" + ns + "/" + name }

// stsSubject returns a "StatefulSet/namespace/name" subject string.
func stsSubject(ns, name string) string { return "StatefulSet/" + ns + "/" + name }

// deploymentProgressDeadlineExceeded is the Reason string set by the Deployment
// controller when a rollout stalls beyond progressDeadlineSeconds.
// Not exported as a constant in k8s.io/api v0.31.0 — defined locally.
const deploymentProgressDeadlineExceeded = "ProgressDeadlineExceeded"

// Options configures the Drainer.
type Options struct {
	Client         *k8s.Client
	NodeName       string
	DryRun         bool
	Timeout        time.Duration
	SkipDaemonSets bool
	DeleteEmptyDir bool
	GracePeriod    int32
	RolloutTimeout time.Duration
	Force          bool
	// ForceDeleteStandalone force-deletes pods with no owner references using
	// a direct Delete (gracePeriodSeconds=0) instead of the Eviction API. Use
	// this when you need these pods gone immediately and don't care about their
	// shutdown hooks. Has no effect unless Force is also true.
	ForceDeleteStandalone bool
	Out                   *Printer
	// PollInterval is the interval between condition checks in all wait loops.
	// Defaults to 5s when zero; set to a small value in tests.
	PollInterval time.Duration
	// PodVacateTimeout is the per-workload deadline for verifying pods have left
	// the node after a successful rollout. This is separate from RolloutTimeout
	// because pod departure is bounded by terminationGracePeriodSeconds, not
	// rollout convergence time. Defaults to 2 min when zero.
	PodVacateTimeout time.Duration
	// EvictionTimeout bounds how long evictWithPDBRetry will keep retrying a
	// single pod that is blocked by a PodDisruptionBudget. Defaults to 5 min
	// when zero. Set to a short value if you want a fast failure on PDB issues.
	EvictionTimeout time.Duration
	// PDBRetryInterval is the base interval between PDB-blocked eviction retries.
	// Retries use exponential backoff starting at this value, capped at 60s.
	// Defaults to 5s when zero.
	PDBRetryInterval time.Duration
	// MaxConcurrency controls how many workload rolling-restarts run in parallel.
	//   1  – sequential, one workload at a time (default, safest)
	//   N  – process workloads in batches of N; wait for each batch before starting the next
	//   0  – all workloads concurrently (equivalent to N = len(workloads))
	MaxConcurrency int
	// UncordonOnFailure uncordons the node when the drain fails, restoring
	// schedulability. Only applies if this drain session was the one that
	// cordoned the node; nodes that were already cordoned before the drain
	// started are left as-is.
	UncordonOnFailure bool
	// Preflight controls pre-drain health checks. PreflightModeWarn (default)
	// logs findings and continues. PreflightModeStrict aborts on any risk-level
	// issue. PreflightModeOff skips all checks.
	Preflight PreflightMode
	// SkipWorkloads is a set of "Kind/namespace/name" keys to exclude from
	// rolling restarts. Skipped workloads still fall through to eviction normally.
	// Mutually exclusive with OnlyWorkloads.
	SkipWorkloads map[string]bool
	// OnlyWorkloads restricts rolling restarts to exactly this set of
	// "Kind/namespace/name" keys; all others are left untouched.
	// Mutually exclusive with SkipWorkloads.
	OnlyWorkloads map[string]bool
	// EmitEvents causes the drainer to emit Kubernetes Events to the node and
	// workload objects during drain. Requires events/create RBAC permission on
	// the core API group. Disabled by default to avoid surprising users.
	EmitEvents bool
	// Resume causes the drainer to skip workloads that are already recorded as
	// completed in the checkpoint file at CheckpointPath, allowing an
	// interrupted drain to be continued without redundant rolling restarts.
	Resume bool
	// CheckpointPath is the local file path used to persist drain progress.
	// When empty at the CLI layer, the path is derived from the kubeconfig
	// context and node name. Progress is written for non-dry-run drains and the
	// file is deleted after a successful drain.
	CheckpointPath string
	// CheckpointContext is the kubeconfig context used for checkpoint metadata
	// and resume validation. It may be empty when the context cannot be resolved.
	CheckpointContext string
}

// Drainer orchestrates the safe drain sequence.
type Drainer struct {
	opts   Options
	client kubernetes.Interface
	finder *workload.Finder
	events *EventEmitter
}

// pollInterval returns the configured poll interval, falling back to 5 s.
func (d *Drainer) pollInterval() time.Duration {
	if d.opts.PollInterval > 0 {
		return d.opts.PollInterval
	}
	return 5 * time.Second
}

// podVacateTimeout returns the per-workload deadline for pod departure, falling
// back to 2 min.
func (d *Drainer) podVacateTimeout() time.Duration {
	if d.opts.PodVacateTimeout > 0 {
		return d.opts.PodVacateTimeout
	}
	return 2 * time.Minute
}

// evictionTimeout returns the per-pod PDB-retry deadline, falling back to 5 min.
func (d *Drainer) evictionTimeout() time.Duration {
	if d.opts.EvictionTimeout > 0 {
		return d.opts.EvictionTimeout
	}
	return 5 * time.Minute
}

// pdbRetryInterval returns the base backoff interval for PDB-blocked evictions,
// falling back to 5 s.
func (d *Drainer) pdbRetryInterval() time.Duration {
	if d.opts.PDBRetryInterval > 0 {
		return d.opts.PDBRetryInterval
	}
	return 5 * time.Second
}

// NewDrainer creates a Drainer from the provided options.
func NewDrainer(opts Options) *Drainer {
	return &Drainer{
		opts:   opts,
		client: opts.Client.Kubernetes,
		finder: workload.NewFinder(opts.Client.Kubernetes),
		events: NewEventEmitter(opts.Client.Kubernetes, opts.Out, opts.EmitEvents),
	}
}

// Run executes the full safe-drain sequence:
//
//  1. Validate the node exists.
//  2. Discover all Deployments and StatefulSets with non-terminal pods on the node.
//  3. Pre-flight checks: surface downtime risks and stateful-service warnings
//     before making any cluster changes. Behaviour is controlled by Preflight:
//     warn (default) logs and continues; strict aborts on any risk-level finding;
//     off skips all checks.
//  4. Cordon the node (idempotent, patch-based — no resourceVersion conflicts).
//  5. Rolling-restart workloads according to MaxConcurrency:
//     - 1 (default): strictly sequential, one workload fully done before the next.
//     - N > 1:       batches of N run concurrently; each batch must complete before the next starts.
//     - 0:           all workloads concurrently (use with caution on large nodes).
//     Within a batch the first error cancels all siblings (fail-fast via errgroup).
//  6. Evict any remaining pods (DaemonSets, standalones, Jobs) per flags.
func (d *Drainer) Run(ctx context.Context) (retErr error) {
	// Apply the overall drain deadline when set. All subordinate wait loops
	// receive this bounded context so they cannot exceed the global budget.
	if d.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.opts.Timeout)
		defer cancel()
	}

	out := d.opts.Out
	start := time.Now()

	// Step 1: Validate node.
	out.Infof(d.opts.NodeName, "Validating %q", d.opts.NodeName)
	var node *corev1.Node
	if err := retryTransient(ctx, d.pollInterval(), func() error {
		var e error
		node, e = d.client.CoreV1().Nodes().Get(ctx, d.opts.NodeName, metav1.GetOptions{})
		return e
	}); err != nil {
		return fmt.Errorf("node %q not found: %w", d.opts.NodeName, err)
	}
	out.Infof(d.opts.NodeName, "Found · kernel=%s ready=%v",
		node.Status.NodeInfo.KernelVersion, isNodeReady(node))

	// Step 2: Discover workloads — before any cluster changes so pre-flight
	// can see the full picture and the operator can abort without side effects.
	out.Info(d.opts.NodeName, "Discovering managed workloads...")
	var workloads []workload.Workload
	if err := retryTransient(ctx, d.pollInterval(), func() error {
		var e error
		workloads, e = d.finder.FindForNode(ctx, d.opts.NodeName)
		return e
	}); err != nil {
		return fmt.Errorf("discovering workloads: %w", err)
	}

	if len(workloads) == 0 {
		out.Info(d.opts.NodeName, "No managed workloads found")
	} else {
		out.Infof(d.opts.NodeName, "Found %d managed workload(s) to restart:", len(workloads))
		for _, w := range workloads {
			out.Infof(d.opts.NodeName, "  · %s", w)
		}
	}

	// Filter workloads per --skip-workload / --only-workload before pre-flight
	// so pre-flight only scans workloads that will actually be restarted.
	workloads = d.filterWorkloads(workloads)

	// Step 3: Pre-flight checks — surface risks before making any cluster changes.
	if d.opts.Preflight != PreflightModeOff {
		if err := d.runPreflight(ctx, workloads); err != nil {
			return err
		}
	}

	// Step 4: Cordon.
	cordonedByUs, err := d.cordon(ctx, node)
	if err != nil {
		return err
	}
	d.events.NodeEvent(ctx, d.opts.NodeName, "Draining",
		fmt.Sprintf("kubectl-safed: beginning drain of %q (%d workload(s))", d.opts.NodeName, len(workloads)),
		corev1.EventTypeNormal)
	// Emit DrainFailed event on any error path after the cordon.
	defer func() {
		if retErr != nil {
			d.events.NodeEvent(context.Background(), d.opts.NodeName, "DrainFailed",
				fmt.Sprintf("kubectl-safed: drain of %q failed: %v", d.opts.NodeName, retErr),
				corev1.EventTypeWarning)
		}
	}()
	// If UncordonOnFailure is set and we were the ones who cordoned the node,
	// schedule an uncordon on any failure path. We use a fresh context because
	// the drain context may already be cancelled (e.g. on --timeout expiry).
	if cordonedByUs && d.opts.UncordonOnFailure {
		defer func() {
			if retErr != nil {
				d.uncordon(out)
			}
		}()
	}

	// Step 5: Rolling restart workloads (sequential, batch, or fully parallel).
	if err := d.runWorkloads(ctx, workloads); err != nil {
		return err
	}

	// Step 6: Evict remaining pods.
	if err := d.evictRemaining(ctx); err != nil {
		return err
	}

	// Delete the checkpoint on successful completion — it's no longer needed.
	if d.opts.CheckpointPath != "" && !d.opts.DryRun {
		if err := DeleteCheckpoint(d.opts.CheckpointPath); err != nil {
			out.Warnf(d.opts.NodeName, "failed to remove checkpoint: %v", err)
		}
	}

	if d.opts.DryRun {
		out.DryRunf(d.opts.NodeName, "Dry-run complete — no changes were made to %q", d.opts.NodeName)
	} else {
		elapsed := time.Since(start).Round(time.Second)
		// Use context.Background() so the event is reliably emitted even if
		// the drain context expired right as the last workload completed.
		d.events.NodeEvent(context.Background(), d.opts.NodeName, "Drained",
			fmt.Sprintf("kubectl-safed: drain of %q complete (%s)", d.opts.NodeName, elapsed),
			corev1.EventTypeNormal)
		out.Elapsed(start, d.opts.NodeName, fmt.Sprintf("Drained %q", d.opts.NodeName))
	}
	return nil
}

// filterWorkloads applies SkipWorkloads / OnlyWorkloads filtering and logs
// each exclusion. It is a no-op when both maps are empty.
func (d *Drainer) filterWorkloads(workloads []workload.Workload) []workload.Workload {
	if len(d.opts.SkipWorkloads) == 0 && len(d.opts.OnlyWorkloads) == 0 {
		return workloads
	}
	out := d.opts.Out
	filtered := workloads[:0:0] // zero-length, same backing array avoided
	for _, w := range workloads {
		key := fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
		if len(d.opts.OnlyWorkloads) > 0 && !d.opts.OnlyWorkloads[key] {
			out.Infof(d.opts.NodeName, "Skipping %s (not in --only-workload list)", wSubject(w))
			continue
		}
		if d.opts.SkipWorkloads[key] {
			out.Infof(d.opts.NodeName, "Skipping %s (--skip-workload)", wSubject(w))
			continue
		}
		filtered = append(filtered, w)
	}
	return filtered
}

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
			if cp.IsDone(w) {
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
		cp = &Checkpoint{Completed: make(map[string]bool)}
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

func (d *Drainer) validateCheckpoint(cp *Checkpoint) error {
	if cp.NodeName != "" && cp.NodeName != d.opts.NodeName {
		return fmt.Errorf("checkpoint is for node %q, not %q", cp.NodeName, d.opts.NodeName)
	}
	if cp.Context != "" && d.opts.CheckpointContext != "" && cp.Context != d.opts.CheckpointContext {
		return fmt.Errorf("checkpoint is for kube context %q, not %q", cp.Context, d.opts.CheckpointContext)
	}
	return nil
}

// cordon marks the node unschedulable via a strategic-merge patch so new pods
// are not scheduled onto it. Using Patch (not Update) is idempotent and avoids
// resourceVersion conflicts with concurrent controllers.
//
// Returns (true, nil) when this call performed the cordon, (false, nil) when
// the node was already cordoned or when running in dry-run mode. The boolean
// is used by Run to decide whether to schedule an uncordon on failure.
func (d *Drainer) cordon(ctx context.Context, node *corev1.Node) (cordonedByUs bool, err error) {
	out := d.opts.Out
	if node.Spec.Unschedulable {
		out.Info(d.opts.NodeName, "Already cordoned")
		if d.opts.UncordonOnFailure {
			out.Info(d.opts.NodeName, "NOTE: --uncordon-on-failure has no effect (node was already cordoned before this drain)")
		}
		return false, nil
	}

	if d.opts.DryRun {
		out.DryRunf(d.opts.NodeName, "Would cordon %q", d.opts.NodeName)
		return false, nil
	}

	out.Infof(d.opts.NodeName, "Cordoning %q...", d.opts.NodeName)
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err = d.client.CoreV1().Nodes().Patch(
		ctx, d.opts.NodeName,
		types.StrategicMergePatchType, patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		return false, fmt.Errorf("cordoning node %q: %w", d.opts.NodeName, err)
	}
	out.Donef(d.opts.NodeName, "Cordoned %q", d.opts.NodeName)
	return true, nil
}

// uncordon marks the node schedulable again. It uses a fresh context so it
// still runs even when the drain context has already been cancelled (e.g. on
// --timeout expiry). Best-effort: errors are logged but not propagated.
func (d *Drainer) uncordon(out *Printer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	patch := []byte(`{"spec":{"unschedulable":false}}`)
	_, err := d.client.CoreV1().Nodes().Patch(
		ctx, d.opts.NodeName,
		types.StrategicMergePatchType, patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		out.Infof(d.opts.NodeName, "WARNING: failed to uncordon %q after drain failure: %v", d.opts.NodeName, err)
		return
	}
	out.Donef(d.opts.NodeName, "Uncordoned %q (drain failed, --uncordon-on-failure is set)", d.opts.NodeName)
}

// rollingRestart triggers a rolling restart for w, waits for the rollout to
// complete cluster-wide, then verifies all of w's pods have left this node.
// Start/complete announcements and step counters are handled by the caller
// (runWorkloads) so this method stays usable in both sequential and concurrent
// contexts without duplicating log lines.
func (d *Drainer) rollingRestart(ctx context.Context, w workload.Workload) error {
	if d.opts.DryRun {
		d.opts.Out.DryRun(wSubject(w), "Would rolling-restart")
		return nil
	}

	d.events.WorkloadEvent(ctx, w, "RollingRestartTriggered",
		fmt.Sprintf("kubectl-safed: rolling restart triggered on node drain of %q", d.opts.NodeName),
		corev1.EventTypeNormal)

	switch w.Kind {
	case workload.KindDeployment:
		preGen, err := d.restartDeployment(ctx, w.Namespace, w.Name)
		if err != nil {
			return err
		}
		if err := d.waitForDeploymentRollout(ctx, w.Namespace, w.Name, preGen); err != nil {
			return fmt.Errorf("deployment %s/%s rollout failed: %w", w.Namespace, w.Name, err)
		}

	case workload.KindStatefulSet:
		preGen, err := d.restartStatefulSet(ctx, w.Namespace, w.Name)
		if err != nil {
			return err
		}
		if err := d.waitForStatefulSetRollout(ctx, w.Namespace, w.Name, preGen); err != nil {
			return fmt.Errorf("StatefulSet %s/%s rollout failed: %w", w.Namespace, w.Name, err)
		}

	default:
		return fmt.Errorf("unsupported workload kind %q", w.Kind)
	}

	// Verify the node is clear of this workload's pods before moving on.
	if err := d.waitForPodsOffNode(ctx, w); err != nil {
		return fmt.Errorf("%s pods did not leave node %q: %w", w, d.opts.NodeName, err)
	}

	d.events.WorkloadEvent(ctx, w, "RollingRestartComplete",
		fmt.Sprintf("kubectl-safed: rolling restart complete, pods cleared from %q", d.opts.NodeName),
		corev1.EventTypeNormal)

	return nil
}

// restartDeployment patches the Deployment pod template with a restartedAt
// annotation (identical to `kubectl rollout restart`). Returns the Deployment's
// generation from the PATCH response so the rollout wait can anchor on the
// exact revision this drain triggered, not a stale pre-patch snapshot.
func (d *Drainer) restartDeployment(ctx context.Context, namespace, name string) (int64, error) {
	patch, err := buildRestartPatch()
	if err != nil {
		return 0, err
	}
	updated, err := d.client.AppsV1().Deployments(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("patching Deployment %s/%s: %w", namespace, name, err)
	}
	d.opts.Out.Infof(depSubject(namespace, name), "Restart patch applied (targetGen=%d)", updated.Generation)
	return updated.Generation, nil
}

// restartStatefulSet patches the StatefulSet pod template with a restartedAt
// annotation. Returns the StatefulSet's generation from the PATCH response.
func (d *Drainer) restartStatefulSet(ctx context.Context, namespace, name string) (int64, error) {
	patch, err := buildRestartPatch()
	if err != nil {
		return 0, err
	}
	updated, err := d.client.AppsV1().StatefulSets(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("patching StatefulSet %s/%s: %w", namespace, name, err)
	}
	d.opts.Out.Infof(stsSubject(namespace, name), "Restart patch applied (targetGen=%d)", updated.Generation)
	return updated.Generation, nil
}

// buildRestartPatch constructs the strategic-merge patch that sets the
// kubectl.kubernetes.io/restartedAt annotation on the pod template.
func buildRestartPatch() ([]byte, error) {
	ts := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"kubectl.kubernetes.io/restartedAt": ts,
					},
				},
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshalling restart patch: %w", err)
	}
	return data, nil
}

// waitForDeploymentRollout polls until the Deployment's rollout is complete.
//
// targetGeneration is the Generation from the PATCH response, so it reflects
// exactly the revision this drain triggered. The gate `ObservedGeneration <
// targetGeneration` ensures we don't read stale status from a prior reconcile
// cycle and avoids false-completion if a concurrent change incremented
// Generation before our patch was applied.
//
// Fail-fast paths:
//   - ProgressDeadlineExceeded condition on the Deployment itself.
//   - Any pod matching the workload selector stuck in CrashLoopBackOff,
//     ImagePullBackOff, or ErrImagePull.
func (d *Drainer) waitForDeploymentRollout(ctx context.Context, namespace, name string, targetGeneration int64) error {
	subj := depSubject(namespace, name)
	d.opts.Out.Pollf(subj, "Waiting for rollout (targetGen=%d)", targetGeneration)

	// Single timeout via PollUntilContextTimeout — no manual context wrapper.
	// The caller's ctx already carries the global --timeout deadline; RolloutTimeout
	// is the per-workload budget on top of that. Zero means no per-workload limit
	// (only the global --timeout applies).
	rolloutCtx := ctx
	if d.opts.RolloutTimeout > 0 {
		var cancel context.CancelFunc
		rolloutCtx, cancel = context.WithTimeout(ctx, d.opts.RolloutTimeout)
		defer cancel()
	}
	// Cache the new ReplicaSet's selector so we only list ReplicaSets once
	// per revision instead of on every poll tick. The revision annotation on a
	// Deployment does not change mid-rollout; if a concurrent restart bumps it
	// we refresh automatically by detecting the changed revision string.
	var (
		cachedDepRevision string
		cachedNewSel      *metav1.LabelSelector
	)
	return wait.PollUntilContextCancel(rolloutCtx, d.pollInterval(), true,
		func(ctx context.Context) (bool, error) {
			dep, err := d.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if isTransientAPIError(err) {
					return false, nil // retry on next tick
				}
				return false, err
			}
			s := dep.Status

			// Gate: wait for the controller to observe the generation we patched.
			if s.ObservedGeneration < targetGeneration {
				d.opts.Out.Pollf(subj, "waiting for controller (observedGen=%d need >=%d)",
					s.ObservedGeneration, targetGeneration)
				return false, nil
			}

			// Fail fast on ProgressDeadlineExceeded.
			for _, c := range s.Conditions {
				if c.Type == appsv1.DeploymentProgressing &&
					c.Status == corev1.ConditionFalse &&
					c.Reason == deploymentProgressDeadlineExceeded {
					return false, fmt.Errorf("rollout stalled (ProgressDeadlineExceeded): %s", c.Message)
				}
			}

			desired := int32(1)
			if dep.Spec.Replicas != nil {
				desired = *dep.Spec.Replicas
			}

			d.opts.Out.Pollf(subj, "rollout updated=%d/%d ready=%d/%d available=%d/%d unavail=%d",
				s.UpdatedReplicas, desired,
				s.ReadyReplicas, desired,
				s.AvailableReplicas, desired,
				s.UnavailableReplicas,
			)

			// Fail fast on unrecoverable pod states — much faster than waiting
			// for ProgressDeadlineExceeded (cluster default: 600 s).
			// Use the new ReplicaSet's selector so we only check new-revision
			// pods and avoid false-positives on old pods being replaced.
			depRevision := dep.Annotations["deployment.kubernetes.io/revision"]
			if depRevision != cachedDepRevision {
				cachedNewSel, err = d.newReplicaSetSelector(ctx, dep)
				if err != nil {
					if isTransientAPIError(err) {
						return false, nil
					}
					return false, fmt.Errorf("resolving new ReplicaSet selector: %w", err)
				}
				cachedDepRevision = depRevision
			}
			if reason, pod, err := d.findBadPodState(ctx, namespace, cachedNewSel); err != nil {
				if isTransientAPIError(err) {
					return false, nil
				}
				return false, fmt.Errorf("checking pod states: %w", err)
			} else if reason != "" {
				return false, fmt.Errorf("pod %s/%s stuck in %s — rollout will not complete", pod.Namespace, pod.Name, reason)
			}

			return s.UpdatedReplicas == desired &&
				s.ReadyReplicas == desired &&
				s.AvailableReplicas == desired &&
				s.UnavailableReplicas == 0 &&
				s.ObservedGeneration >= dep.Generation, nil
		},
	)
}

// waitForStatefulSetRollout polls until the StatefulSet's rollout is complete.
//
// targetGeneration is the Generation from the PATCH response.
//
// The correct terminal condition requires all of:
//   - ObservedGeneration >= targetGeneration   (controller processed our patch)
//   - UpdateRevision == CurrentRevision        (all pods at the new revision)
//   - UpdatedReplicas == desired               (controller updated all pods)
//   - CurrentReplicas == desired               (controller set CurrentRevision on all)
//   - ReadyReplicas == desired                 (all pods passing readiness probes)
//
// Note: CurrentReplicas counts pods at CurrentRevision (the OLD revision during
// a rolling update). Once all pods are updated, CurrentRevision flips to equal
// UpdateRevision and CurrentReplicas reaches desired. Checking both
// UpdatedReplicas and CurrentReplicas prevents false completion during the
// brief window when UpdateRevision == CurrentRevision but the controller hasn't
// yet reconciled all status fields.
//
// StatefulSets have no ProgressDeadlineExceeded condition, so bad pod states
// (CrashLoopBackOff, ImagePullBackOff, ErrImagePull) are the primary fail-fast
// mechanism here.
func (d *Drainer) waitForStatefulSetRollout(ctx context.Context, namespace, name string, targetGeneration int64) error {
	subj := stsSubject(namespace, name)
	d.opts.Out.Pollf(subj, "Waiting for rollout (targetGen=%d)", targetGeneration)

	rolloutCtx := ctx
	if d.opts.RolloutTimeout > 0 {
		var cancel context.CancelFunc
		rolloutCtx, cancel = context.WithTimeout(ctx, d.opts.RolloutTimeout)
		defer cancel()
	}
	// Cache the revision selector to avoid allocating a new map on every tick.
	// UpdateRevision is stable once set for the duration of a rollout; we only
	// rebuild when it changes (empty → hash at rollout start).
	var (
		cachedUpdateRevision string
		cachedRevSel         *metav1.LabelSelector
	)
	return wait.PollUntilContextCancel(rolloutCtx, d.pollInterval(), true,
		func(ctx context.Context) (bool, error) {
			sts, err := d.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if isTransientAPIError(err) {
					return false, nil // retry on next tick
				}
				return false, err
			}
			s := sts.Status

			if s.ObservedGeneration < targetGeneration {
				d.opts.Out.Pollf(subj, "waiting for controller (observedGen=%d need >=%d)",
					s.ObservedGeneration, targetGeneration)
				return false, nil
			}

			desired := int32(1)
			if sts.Spec.Replicas != nil {
				desired = *sts.Spec.Replicas
			}

			d.opts.Out.Pollf(subj, "rollout updated=%d/%d current=%d/%d ready=%d/%d (updateRev=%s currentRev=%s)",
				s.UpdatedReplicas, desired,
				s.CurrentReplicas, desired,
				s.ReadyReplicas, desired,
				s.UpdateRevision, s.CurrentRevision,
			)

			// Actively check pod states — StatefulSets have no
			// ProgressDeadlineExceeded equivalent.
			// Restrict to new-revision pods (controller-revision-hash =
			// UpdateRevision) so we don't false-positive on old pods that
			// are still being replaced. Fall back to the broad selector
			// if UpdateRevision is not yet set.
			if s.UpdateRevision != cachedUpdateRevision {
				cachedRevSel = newRevisionSelector(sts.Spec.Selector, s.UpdateRevision)
				cachedUpdateRevision = s.UpdateRevision
			}
			if reason, pod, err := d.findBadPodState(ctx, namespace, cachedRevSel); err != nil {
				if isTransientAPIError(err) {
					return false, nil
				}
				return false, fmt.Errorf("checking pod states: %w", err)
			} else if reason != "" {
				return false, fmt.Errorf("pod %s/%s stuck in %s — rollout will not complete", pod.Namespace, pod.Name, reason)
			}

			// Guard against two empty strings matching before the controller
			// has set UpdateRevision.
			return s.UpdateRevision != "" &&
				s.UpdateRevision == s.CurrentRevision &&
				s.UpdatedReplicas == desired &&
				s.CurrentReplicas == desired &&
				s.ReadyReplicas == desired, nil
		},
	)
}

// waitForPodsOffNode waits until no active (non-terminal, non-terminating) pods
// belonging to w remain on the node being drained.
//
// Terminating pods (DeletionTimestamp set) are excluded from the count because
// the workload is already healthy on other nodes — the termination cleanup is
// kubelet's responsibility and does not block the drain.
func (d *Drainer) waitForPodsOffNode(ctx context.Context, w workload.Workload) error {
	subj := wSubject(w)
	vacate := d.podVacateTimeout()
	d.opts.Out.Pollf(subj, "Verifying pods have left node %q (timeout=%s)", d.opts.NodeName, vacate)

	selectorStr, err := buildLabelSelectorString(w.Selector)
	if err != nil {
		return fmt.Errorf("building pod selector for %s: %w", w, err)
	}

	return wait.PollUntilContextTimeout(ctx, d.pollInterval(), vacate, true,
		func(ctx context.Context) (bool, error) {
			pods, err := d.client.CoreV1().Pods(w.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selectorStr,
				FieldSelector: "spec.nodeName=" + d.opts.NodeName,
			})
			if err != nil {
				if isTransientAPIError(err) {
					return false, nil // retry on next tick
				}
				return false, err
			}

			active := 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				// Terminating pods are already on their way out.
				if pod.DeletionTimestamp != nil {
					continue
				}
				// Terminal pods (Succeeded/Failed) need no migration.
				if workload.IsTerminalPod(pod) {
					continue
				}
				active++
			}

			if active > 0 {
				d.opts.Out.Pollf(subj, "%d active pod(s) still on %q, waiting", active, d.opts.NodeName)
				return false, nil
			}
			return true, nil
		},
	)
}

// newReplicaSetSelector returns the label selector for the current (new)
// ReplicaSet of dep. Using the new RS's selector instead of the Deployment's
// broad selector prevents findBadPodState from flagging old-revision pods that
// are still being replaced during a rolling update.
//
// The current RS is identified by matching the deployment.kubernetes.io/revision
// annotation on both the Deployment and its owned ReplicaSets. Falls back to
// dep.Spec.Selector if the new RS cannot be resolved (e.g. not yet created).
func (d *Drainer) newReplicaSetSelector(ctx context.Context, dep *appsv1.Deployment) (*metav1.LabelSelector, error) {
	depRevision := dep.Annotations["deployment.kubernetes.io/revision"]
	if depRevision == "" {
		return dep.Spec.Selector, nil
	}

	// Use the deployment's selector as a label filter to avoid fetching every
	// RS in the namespace (important in namespaces with many deployments).
	selStr, err := buildLabelSelectorString(dep.Spec.Selector)
	if err != nil {
		return dep.Spec.Selector, nil // fall back gracefully
	}
	rsList, err := d.client.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selStr,
	})
	if err != nil {
		return nil, fmt.Errorf("listing ReplicaSets: %w", err)
	}

	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !isOwnedByUID(rs.OwnerReferences, dep.UID) {
			continue
		}
		if rs.Annotations["deployment.kubernetes.io/revision"] == depRevision {
			return rs.Spec.Selector, nil
		}
	}

	// New RS not yet visible — fall back to the broad deployment selector.
	return dep.Spec.Selector, nil
}

// isOwnedByUID reports whether any owner reference in refs matches uid.
func isOwnedByUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// newRevisionSelector builds a copy of base that additionally requires
// controller-revision-hash == revisionHash. Used during StatefulSet rollout
// monitoring to restrict bad-pod checks to new-revision pods only.
// Returns base unmodified when revisionHash is empty (revision not yet assigned).
func newRevisionSelector(base *metav1.LabelSelector, revisionHash string) *metav1.LabelSelector {
	if base == nil || revisionHash == "" {
		return base
	}
	labels := make(map[string]string, len(base.MatchLabels)+1)
	for k, v := range base.MatchLabels {
		labels[k] = v
	}
	labels["controller-revision-hash"] = revisionHash
	return &metav1.LabelSelector{
		MatchLabels:      labels,
		MatchExpressions: base.MatchExpressions,
	}
}

// findBadPodState lists pods matching sel in namespace and returns the first
// container waiting reason that indicates the rollout will never recover:
// CrashLoopBackOff, ImagePullBackOff, or ErrImagePull.
//
// API errors are returned so callers can distinguish "unknown state" from "no
// bad state". A persistent API error (e.g. RBAC misconfiguration) should abort
// the rollout rather than silently wait for the full timeout.
func (d *Drainer) findBadPodState(ctx context.Context, namespace string, sel *metav1.LabelSelector) (reason string, badPod *corev1.Pod, err error) {
	if sel == nil {
		return "", nil, nil
	}
	selectorStr, err := buildLabelSelectorString(sel)
	if err != nil {
		return "", nil, fmt.Errorf("building selector: %w", err)
	}
	pods, err := d.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorStr})
	if err != nil {
		return "", nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, cs := range pod.Status.InitContainerStatuses {
			if r := badWaitingReason(cs); r != "" {
				return r, pod, nil
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if r := badWaitingReason(cs); r != "" {
				return r, pod, nil
			}
		}
	}
	return "", nil, nil
}

// badWaitingReason returns the container's Waiting.Reason if it indicates an
// unrecoverable state, or "".
//
// CrashLoopBackOff: gated on LastTerminationState.Terminated being non-nil,
// which the kubelet sets after the container's first exit. This avoids
// false-positives on slow-starting containers (e.g. heavy init containers)
// that are in Waiting briefly before their first run.
//
// ImagePullBackOff / ErrImagePull: reported immediately — a bad image reference
// will not self-heal without a spec change.
func badWaitingReason(cs corev1.ContainerStatus) string {
	if cs.State.Waiting == nil {
		return ""
	}
	switch cs.State.Waiting.Reason {
	case "CrashLoopBackOff":
		// LastTerminationState is set after the container has exited at least once.
		if cs.LastTerminationState.Terminated != nil {
			return "CrashLoopBackOff"
		}
	case "ImagePullBackOff", "ErrImagePull":
		return cs.State.Waiting.Reason
	}
	return ""
}

// isTransientAPIError reports whether err is a transient Kubernetes API error
// that may resolve on the next poll tick: server timeouts, internal errors, or
// temporary rate limiting. Transient errors in poll condition functions should
// return (false, nil) so the poll retries rather than aborting the drain.
func isTransientAPIError(err error) bool {
	return k8serrors.IsInternalError(err) ||
		k8serrors.IsServerTimeout(err) ||
		k8serrors.IsTimeout(err) ||
		k8serrors.IsTooManyRequests(err)
}

// retryTransient calls fn up to 3 times, retrying after interval when the
// returned error is a transient Kubernetes API error. The first non-transient
// error is returned immediately. Context cancellation stops the retry loop.
func retryTransient(ctx context.Context, interval time.Duration, fn func() error) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil || !isTransientAPIError(lastErr) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(interval):
		}
	}
	return lastErr
}

// buildLabelSelectorString converts a *metav1.LabelSelector to the string form
// accepted by ListOptions.LabelSelector.
func buildLabelSelectorString(sel *metav1.LabelSelector) (string, error) {
	if sel == nil {
		return "", fmt.Errorf("nil label selector")
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", fmt.Errorf("parsing label selector: %w", err)
	}
	return s.String(), nil
}

// evictRemaining evicts pods left on the node after rolling restarts complete.
// What gets evicted is controlled by the SkipDaemonSets, Force, and
// DeleteEmptyDir options.
func (d *Drainer) evictRemaining(ctx context.Context) error {
	out := d.opts.Out

	pods, err := d.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + d.opts.NodeName,
	})
	if err != nil {
		return fmt.Errorf("listing remaining pods on %q: %w", d.opts.NodeName, err)
	}

	blocked := blockedEvictionPods(pods.Items, d.opts.SkipDaemonSets, d.opts.Force, d.opts.DeleteEmptyDir)
	if len(blocked) > 0 {
		for _, b := range blocked {
			out.Warnf(d.opts.NodeName, "Cannot evict %s/%s: %s", b.pod.Namespace, b.pod.Name, b.reason)
		}
		if !d.opts.DryRun {
			first := blocked[0]
			return fmt.Errorf("remaining pod %s/%s cannot be evicted: %s", first.pod.Namespace, first.pod.Name, first.reason)
		}
	}

	evictable := filterEvictable(pods.Items, d.opts.SkipDaemonSets, d.opts.Force, d.opts.DeleteEmptyDir)
	if len(evictable) == 0 {
		out.Infof(d.opts.NodeName, "No remaining pods to evict on %q", d.opts.NodeName)
		return nil
	}

	out.Infof(d.opts.NodeName, "Evicting %d remaining pod(s) on %q:", len(evictable), d.opts.NodeName)
	for i := range evictable {
		pod := &evictable[i]
		out.Infof(d.opts.NodeName, "  · %s/%s [owner: %s]", pod.Namespace, pod.Name, podOwnerKind(pod))
	}

	for i := range evictable {
		pod := &evictable[i]
		podSubj := fmt.Sprintf("Pod/%s/%s", pod.Namespace, pod.Name)
		if d.opts.DryRun {
			if d.opts.ForceDeleteStandalone && len(pod.OwnerReferences) == 0 {
				out.DryRunf(podSubj, "Would force-delete (standalone, no owner)")
			} else {
				out.DryRunf(podSubj, "Would evict (owner: %s)", podOwnerKind(pod))
			}
			continue
		}

		// Standalone pods with ForceDeleteStandalone: bypass the eviction API
		// (which respects PDB) and issue a direct delete with gracePeriodSeconds=0.
		if d.opts.ForceDeleteStandalone && len(pod.OwnerReferences) == 0 {
			gp := int64(0)
			if err := d.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
				GracePeriodSeconds: &gp,
			}); err != nil && !k8serrors.IsNotFound(err) {
				return fmt.Errorf("force-deleting pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			out.Done(podSubj, "Force-deleted (standalone)")
			continue
		}

		if err := d.evictWithPDBRetry(ctx, out, podSubj, pod); err != nil {
			return err
		}
	}

	return nil
}

// evictWithPDBRetry calls EvictV1 and retries when the eviction is temporarily
// blocked by a PodDisruptionBudget (HTTP 429) or quota (HTTP 503).
//
// Retries use exponential backoff (base = PDBRetryInterval, cap = 60 s) and
// are bounded by EvictionTimeout. This prevents the drain from hanging
// indefinitely on a misconfigured PDB.
func (d *Drainer) evictWithPDBRetry(ctx context.Context, out *Printer, subj string, pod *corev1.Pod) error {
	evictCtx, cancel := context.WithTimeout(ctx, d.evictionTimeout())
	defer cancel()

	interval := d.pdbRetryInterval()
	const maxInterval = 60 * time.Second

	for attempt := 1; ; attempt++ {
		err := d.client.CoreV1().Pods(pod.Namespace).EvictV1(evictCtx, buildEviction(pod, d.opts.GracePeriod))
		if err == nil {
			out.Done(subj, "Evicted")
			return nil
		}

		// PDB temporarily blocks the eviction — back off and retry.
		if k8serrors.IsTooManyRequests(err) || k8serrors.IsServiceUnavailable(err) {
			out.Pollf(subj, "eviction blocked by PodDisruptionBudget (attempt %d), retrying in %s", attempt, interval)
			select {
			case <-evictCtx.Done():
				return fmt.Errorf("evicting pod %s/%s: timed out waiting for PDB after %d attempt(s): %w",
					pod.Namespace, pod.Name, attempt, evictCtx.Err())
			case <-time.After(interval):
				// Exponential backoff capped at maxInterval.
				interval *= 2
				if interval > maxInterval {
					interval = maxInterval
				}
				continue
			}
		}

		// Pod already gone — treat as success.
		if k8serrors.IsNotFound(err) {
			out.Done(subj, "Already gone")
			return nil
		}

		return fmt.Errorf("evicting pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
}

// filterEvictable returns the subset of pods eligible for conventional eviction.
//
//   - Already terminating or terminal pods are skipped (no-op).
//   - Mirror (static) pods cannot be evicted via the API.
//   - DaemonSet pods are skipped when SkipDaemonSets is true.
//   - Pods with emptyDir volumes lose data on eviction; skipped unless
//     deleteEmptyDir or force is set.
//   - Standalone pods (no owner) and Job-owned pods require force.
func filterEvictable(pods []corev1.Pod, skipDaemonSets, force, deleteEmptyDir bool) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		pod := &pods[i]

		// Already being deleted — kubelet will finish cleanup.
		if pod.DeletionTimestamp != nil {
			continue
		}
		// Terminal pods need no action.
		if workload.IsTerminalPod(pod) {
			continue
		}
		// Mirror (static) pods are owned by kubelet; cannot be evicted via API.
		if isMirrorPod(pod) {
			continue
		}
		// DaemonSet pods: skip per flag.
		if isDaemonSetPod(pod) && skipDaemonSets {
			continue
		}
		// Pods using emptyDir lose data on eviction; require explicit opt-in.
		if hasEmptyDir(pod) && !deleteEmptyDir && !force {
			continue
		}
		// Standalone pods require --force.
		if len(pod.OwnerReferences) == 0 && !force {
			continue
		}
		// Job-owned pods are not managed by rolling restarts; require --force.
		if isJobPod(pod) && !force {
			continue
		}

		out = append(out, *pod)
	}
	return out
}

type blockedEvictionPod struct {
	pod    corev1.Pod
	reason string
}

func blockedEvictionPods(pods []corev1.Pod, skipDaemonSets, force, deleteEmptyDir bool) []blockedEvictionPod {
	var out []blockedEvictionPod
	for i := range pods {
		pod := &pods[i]

		if pod.DeletionTimestamp != nil || workload.IsTerminalPod(pod) || isMirrorPod(pod) {
			continue
		}
		if isDaemonSetPod(pod) && skipDaemonSets {
			continue
		}
		if len(pod.OwnerReferences) == 0 && !force {
			out = append(out, blockedEvictionPod{pod: *pod, reason: "standalone pods require --force"})
			continue
		}
		if isJobPod(pod) && !force {
			out = append(out, blockedEvictionPod{pod: *pod, reason: "Job-owned pods require --force"})
			continue
		}
		if hasEmptyDir(pod) && !deleteEmptyDir && !force {
			out = append(out, blockedEvictionPod{pod: *pod, reason: "emptyDir pods require --delete-emptydir-data or --force"})
			continue
		}
	}
	return out
}

func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isJobPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return true
		}
	}
	return false
}

func hasEmptyDir(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}

func podOwnerKind(pod *corev1.Pod) string {
	if len(pod.OwnerReferences) == 0 {
		return "standalone"
	}
	return pod.OwnerReferences[0].Kind
}

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func buildEviction(pod *corev1.Pod, gracePeriod int32) *policyv1.Eviction {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if gracePeriod >= 0 {
		gp := int64(gracePeriod)
		eviction.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: &gp}
	}
	return eviction
}
