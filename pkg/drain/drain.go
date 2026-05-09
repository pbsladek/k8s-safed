// Package drain implements safe Kubernetes node draining via rolling restarts.
package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// StatefulNamePatterns extends the built-in name patterns used by
	// preflight to surface known stateful workloads.
	StatefulNamePatterns []string
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
	// WorkloadCoordinator deduplicates rolling restarts across concurrent node
	// drains in the same process. Each node still verifies that its own pods left.
	WorkloadCoordinator *WorkloadCoordinator
}

// Drainer orchestrates the safe drain sequence.
type Drainer struct {
	opts               Options
	client             kubernetes.Interface
	finder             *workload.Finder
	events             *EventEmitter
	protectedWorkloads []workload.Workload
}

// NewDrainer creates a Drainer from the provided options.
func NewDrainer(opts Options) *Drainer {
	return &Drainer{
		opts:   opts,
		client: opts.Client.Kubernetes,
		finder: workload.NewFinder(opts.Client.Kubernetes),
		events: NewEventEmitter(opts.Client.Kubernetes, opts.Out, opts.EmitEvents && !opts.DryRun),
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
		node, e = d.nodes().Get(ctx, d.opts.NodeName, metav1.GetOptions{})
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
	// Excluded managed workloads are also protected from conventional eviction;
	// these flags mean "leave this workload alone", not merely "avoid restart".
	workloads = d.filterWorkloads(workloads)
	d.warnInvalidPriorityAnnotations(workloads)

	// Step 3: Pre-flight checks — surface risks before making any cluster changes.
	if d.opts.Preflight != PreflightModeOff {
		if err := d.runPreflight(ctx, workloads); err != nil {
			return err
		}
	}

	// Validate resume metadata before cordoning. A bad checkpoint is an input
	// error, not a drain failure, so it must not make the node unschedulable.
	if d.opts.Resume && d.opts.CheckpointPath != "" {
		cp, err := LoadCheckpoint(d.opts.CheckpointPath)
		if err != nil {
			return fmt.Errorf("loading checkpoint: %w", err)
		}
		if err := d.validateCheckpoint(cp); err != nil {
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
		if len(d.protectedWorkloads) > 0 {
			out.Warnf(d.opts.NodeName, "%d filtered managed workload(s) were left untouched; node may still have pods by design", len(d.protectedWorkloads))
		}
		// Use context.Background() so the event is reliably emitted even if
		// the drain context expired right as the last workload completed.
		d.events.NodeEvent(context.Background(), d.opts.NodeName, "Drained",
			fmt.Sprintf("kubectl-safed: drain of %q complete (%s)", d.opts.NodeName, elapsed),
			corev1.EventTypeNormal)
		out.Elapsed(start, d.opts.NodeName, fmt.Sprintf("Drained %q", d.opts.NodeName))
	}
	return nil
}
