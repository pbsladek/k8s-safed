package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/pbsladek/k8s-safed/pkg/k8s"
	"github.com/pbsladek/k8s-safed/pkg/workload"
)

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
	Out            *Printer
}

// Drainer orchestrates the safe drain sequence.
type Drainer struct {
	opts   Options
	client kubernetes.Interface
	finder *workload.Finder
}

// NewDrainer creates a Drainer from the provided options.
func NewDrainer(opts Options) *Drainer {
	return &Drainer{
		opts:   opts,
		client: opts.Client.Kubernetes,
		finder: workload.NewFinder(opts.Client.Kubernetes),
	}
}

// Run executes the full safe-drain sequence:
//
//  1. Validate the node exists.
//  2. Cordon the node (idempotent, patch-based — no resourceVersion conflicts).
//  3. Discover all Deployments and StatefulSets with non-terminal pods on the node.
//  4. For each workload: trigger a rolling restart, wait for the rollout to
//     complete cluster-wide, then verify all pods have left this node.
//  5. Evict any remaining pods (DaemonSets, standalones, Jobs) per flags.
func (d *Drainer) Run(ctx context.Context) error {
	out := d.opts.Out
	start := time.Now()

	// Step 1: Validate node.
	out.Infof("Validating node %q...", d.opts.NodeName)
	node, err := d.client.CoreV1().Nodes().Get(ctx, d.opts.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("node %q not found: %w", d.opts.NodeName, err)
	}
	out.Infof("Node %q found (kernel: %s, ready: %v)",
		d.opts.NodeName, node.Status.NodeInfo.KernelVersion, isNodeReady(node))

	// Step 2: Cordon.
	if err := d.cordon(ctx, node); err != nil {
		return err
	}

	// Step 3: Discover workloads — single pass, selectors included.
	out.Infof("Discovering managed workloads on %q...", d.opts.NodeName)
	workloads, err := d.finder.FindForNode(ctx, d.opts.NodeName)
	if err != nil {
		return fmt.Errorf("discovering workloads: %w", err)
	}

	if len(workloads) == 0 {
		out.Infof("No managed workloads found on %q", d.opts.NodeName)
	} else {
		out.Infof("Found %d managed workload(s) to rolling-restart:", len(workloads))
		for _, w := range workloads {
			out.Infof("  - %s", w)
		}
	}

	// Step 4: Rolling restart each workload one at a time.
	// After each restart we wait for the rollout to complete globally and then
	// confirm the node is clear of that workload's pods before moving on.
	for i, w := range workloads {
		if err := d.rollingRestart(ctx, i+1, len(workloads), w); err != nil {
			return err
		}
	}

	// Step 5: Evict remaining pods.
	if err := d.evictRemaining(ctx); err != nil {
		return err
	}

	out.Elapsed(start, "Node %q drained successfully", d.opts.NodeName)
	return nil
}

// cordon marks the node unschedulable via a strategic-merge patch so new pods
// are not scheduled onto it. Using Patch (not Update) is idempotent and avoids
// resourceVersion conflicts with concurrent controllers.
func (d *Drainer) cordon(ctx context.Context, node *corev1.Node) error {
	if node.Spec.Unschedulable {
		d.opts.Out.Infof("Node %q is already cordoned", d.opts.NodeName)
		return nil
	}

	if d.opts.DryRun {
		d.opts.Out.DryRun("Would cordon node %q", d.opts.NodeName)
		return nil
	}

	d.opts.Out.Infof("Cordoning node %q...", d.opts.NodeName)
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err := d.client.CoreV1().Nodes().Patch(
		ctx, d.opts.NodeName,
		types.StrategicMergePatchType, patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("cordoning node %q: %w", d.opts.NodeName, err)
	}
	d.opts.Out.Infof("Node %q cordoned", d.opts.NodeName)
	return nil
}

// rollingRestart triggers a rolling restart for w, waits for the rollout to
// complete cluster-wide, then verifies all of w's pods have left this node.
func (d *Drainer) rollingRestart(ctx context.Context, step, total int, w workload.Workload) error {
	out := d.opts.Out

	if d.opts.DryRun {
		out.DryRun("[%d/%d] Would trigger rolling restart for %s", step, total, w)
		return nil
	}

	out.Step(step, total, "Rolling restart: %s", w)

	switch w.Kind {
	case workload.KindDeployment:
		preGen, err := d.restartDeployment(ctx, w.Namespace, w.Name)
		if err != nil {
			return err
		}
		if err := d.waitForDeploymentRollout(ctx, w.Namespace, w.Name, preGen); err != nil {
			return fmt.Errorf("Deployment %s/%s rollout failed: %w", w.Namespace, w.Name, err)
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

	out.Step(step, total, "Complete: %s", w)
	return nil
}

// restartDeployment patches the Deployment pod template with a restartedAt
// annotation (identical to `kubectl rollout restart`). Returns the Deployment's
// generation before the patch so the rollout wait can anchor on it.
func (d *Drainer) restartDeployment(ctx context.Context, namespace, name string) (int64, error) {
	dep, err := d.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting Deployment %s/%s: %w", namespace, name, err)
	}
	preGen := dep.Generation

	patch, err := buildRestartPatch()
	if err != nil {
		return 0, err
	}
	_, err = d.client.AppsV1().Deployments(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("patching Deployment %s/%s: %w", namespace, name, err)
	}
	d.opts.Out.Infof("  Restart annotation applied to Deployment %s/%s (gen %d)", namespace, name, preGen)
	return preGen, nil
}

// restartStatefulSet patches the StatefulSet pod template with a restartedAt
// annotation. Returns the StatefulSet's generation before the patch.
func (d *Drainer) restartStatefulSet(ctx context.Context, namespace, name string) (int64, error) {
	sts, err := d.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, name, err)
	}
	preGen := sts.Generation

	patch, err := buildRestartPatch()
	if err != nil {
		return 0, err
	}
	_, err = d.client.AppsV1().StatefulSets(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return 0, fmt.Errorf("patching StatefulSet %s/%s: %w", namespace, name, err)
	}
	d.opts.Out.Infof("  Restart annotation applied to StatefulSet %s/%s (gen %d)", namespace, name, preGen)
	return preGen, nil
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
// It gates on ObservedGeneration > preGeneration to avoid reading stale status
// before the controller processes the new spec. It logs rolling progress on
// every poll and fails fast if the Progressing condition indicates the
// rollout deadline was exceeded.
func (d *Drainer) waitForDeploymentRollout(ctx context.Context, namespace, name string, preGeneration int64) error {
	d.opts.Out.Infof("  Waiting for Deployment %s/%s rollout (preGen=%d)...", namespace, name, preGeneration)

	timeoutCtx, cancel := context.WithTimeout(ctx, d.opts.RolloutTimeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, d.opts.RolloutTimeout, false,
		func(ctx context.Context) (bool, error) {
			dep, err := d.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			s := dep.Status

			// Gate: wait for the controller to observe the new generation.
			if s.ObservedGeneration <= preGeneration {
				d.opts.Out.Infof("  [%s/%s] waiting for controller (observedGen=%d, need >%d)",
					namespace, name, s.ObservedGeneration, preGeneration)
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

			d.opts.Out.Infof("  [%s/%s] updated=%d/%d ready=%d/%d available=%d/%d unavailable=%d",
				namespace, name,
				s.UpdatedReplicas, desired,
				s.ReadyReplicas, desired,
				s.AvailableReplicas, desired,
				s.UnavailableReplicas,
			)

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
// The correct terminal condition is:
//   - UpdateRevision == CurrentRevision (all pods at the new revision)
//   - UpdatedReplicas == desired         (controller updated all pods)
//   - ReadyReplicas == desired           (all pods passing readiness probes)
//
// Note: CurrentReplicas counts pods at the OLD revision during a rolling update
// and must NOT be used as the completion indicator.
func (d *Drainer) waitForStatefulSetRollout(ctx context.Context, namespace, name string, preGeneration int64) error {
	d.opts.Out.Infof("  Waiting for StatefulSet %s/%s rollout (preGen=%d)...", namespace, name, preGeneration)

	timeoutCtx, cancel := context.WithTimeout(ctx, d.opts.RolloutTimeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, d.opts.RolloutTimeout, false,
		func(ctx context.Context) (bool, error) {
			sts, err := d.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			s := sts.Status

			// ObservedGeneration is int64 (not *int64) in k8s.io/api v0.31.0.
			if s.ObservedGeneration <= preGeneration {
				d.opts.Out.Infof("  [%s/%s] waiting for controller (observedGen=%d, need >%d)",
					namespace, name, s.ObservedGeneration, preGeneration)
				return false, nil
			}

			desired := int32(1)
			if sts.Spec.Replicas != nil {
				desired = *sts.Spec.Replicas
			}

			d.opts.Out.Infof("  [%s/%s] updated=%d/%d ready=%d/%d (updateRev=%s currentRev=%s)",
				namespace, name,
				s.UpdatedReplicas, desired,
				s.ReadyReplicas, desired,
				s.UpdateRevision, s.CurrentRevision,
			)

			// Guard against two empty strings matching before the controller
			// has set UpdateRevision.
			return s.UpdateRevision != "" &&
				s.UpdateRevision == s.CurrentRevision &&
				s.UpdatedReplicas == desired &&
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
	d.opts.Out.Infof("  Verifying %s pods have left node %q...", w, d.opts.NodeName)

	selectorStr, err := buildLabelSelectorString(w.Selector)
	if err != nil {
		return fmt.Errorf("building pod selector for %s: %w", w, err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, d.opts.RolloutTimeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, d.opts.RolloutTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := d.client.CoreV1().Pods(w.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selectorStr,
				FieldSelector: "spec.nodeName=" + d.opts.NodeName,
			})
			if err != nil {
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
				d.opts.Out.Infof("  [%s] %d active pod(s) still on %q, waiting...",
					w, active, d.opts.NodeName)
				return false, nil
			}
			return true, nil
		},
	)
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

	evictable := filterEvictable(pods.Items, d.opts.SkipDaemonSets, d.opts.Force, d.opts.DeleteEmptyDir)
	if len(evictable) == 0 {
		out.Infof("No remaining pods to evict on %q", d.opts.NodeName)
		return nil
	}

	out.Infof("Evicting %d remaining pod(s) on %q:", len(evictable), d.opts.NodeName)
	for i := range evictable {
		pod := &evictable[i]
		out.Infof("  - %s/%s [owner: %s]", pod.Namespace, pod.Name, podOwnerKind(pod))
	}

	for i := range evictable {
		pod := &evictable[i]
		if d.opts.DryRun {
			out.DryRun("Would evict pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		if err := d.client.CoreV1().Pods(pod.Namespace).Evict(ctx, buildEviction(pod, d.opts.GracePeriod)); err != nil {
			return fmt.Errorf("evicting pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		out.Infof("  Evicted %s/%s", pod.Namespace, pod.Name)
	}

	return nil
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
		if v.VolumeSource.EmptyDir != nil {
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

func buildEviction(pod *corev1.Pod, gracePeriod int32) *policyv1beta1.Eviction {
	eviction := &policyv1beta1.Eviction{
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
