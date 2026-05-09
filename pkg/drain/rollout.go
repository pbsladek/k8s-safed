package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/pbsladek/k8s-safed/pkg/k8s"
	"github.com/pbsladek/k8s-safed/pkg/workload"
)

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

	if err := d.opts.WorkloadCoordinator.Do(ctx, w, func(ctx context.Context) error {
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
		return nil
	}); err != nil {
		return err
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
	patch, err := buildRestartPatch(d.now())
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
	patch, err := buildRestartPatch(d.now())
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
func buildRestartPatch(now time.Time) ([]byte, error) {
	ts := now.UTC().Format(time.RFC3339)
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

// waitForPodsOffNode waits until no non-terminal pods belonging to w remain on
// the node being drained. Terminating pods still count: a node is not fully clear
// until kubelet has removed their pod objects.
func (d *Drainer) waitForPodsOffNode(ctx context.Context, w workload.Workload) error {
	subj := wSubject(w)
	vacate := d.podVacateTimeout()
	d.opts.Out.Pollf(subj, "Verifying pods have left node %q (timeout=%s)", d.opts.NodeName, vacate)

	return wait.PollUntilContextTimeout(ctx, d.pollInterval(), vacate, true,
		func(ctx context.Context) (bool, error) {
			count, err := d.workloadPodCountOnNode(ctx, w)
			if err != nil {
				if isTransientAPIError(err) {
					return false, nil // retry on next tick
				}
				return false, err
			}

			if count > 0 {
				d.opts.Out.Pollf(subj, "%d pod(s) still on %q, waiting", count, d.opts.NodeName)
				return false, nil
			}
			return true, nil
		},
	)
}

func (d *Drainer) workloadHasPodsOnNode(ctx context.Context, w workload.Workload) (bool, error) {
	count, err := d.workloadPodCountOnNode(ctx, w)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (d *Drainer) workloadPodCountOnNode(ctx context.Context, w workload.Workload) (int, error) {
	selectorStr, err := buildLabelSelectorString(w.Selector)
	if err != nil {
		return 0, fmt.Errorf("building pod selector for %s: %w", w, err)
	}
	pods, err := d.pods(w.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selectorStr,
		FieldSelector: "spec.nodeName=" + d.opts.NodeName,
	})
	if err != nil {
		return 0, err
	}

	count := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if workload.IsTerminalPod(pod) {
			continue
		}
		count++
	}
	return count, nil
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
	pods, err := d.pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selectorStr})
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
	return k8s.IsTransientAPIError(err)
}

// retryTransient calls fn up to 3 times, retrying after the drainer poll
// interval when the
// returned error is a transient Kubernetes API error. The first non-transient
// error is returned immediately. Context cancellation stops the retry loop.
func (d *Drainer) retryTransient(ctx context.Context, fn func() error) error {
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
		case <-d.after(d.pollInterval()):
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
