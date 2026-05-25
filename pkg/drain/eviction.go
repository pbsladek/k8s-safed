package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// evictRemaining evicts pods left on the node after rolling restarts complete.
// What gets evicted is controlled by the SkipDaemonSets, Force, and
// DeleteEmptyDir options.
func (d *Drainer) evictRemaining(ctx context.Context, state *runState) error {
	out := d.opts.Out

	pods, err := d.pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + d.opts.NodeName,
	})
	if err != nil {
		return fmt.Errorf("listing remaining pods on %q: %w", d.opts.NodeName, err)
	}
	candidates := d.excludeProtectedPods(state, pods.Items)

	blocked := blockedEvictionPods(candidates, d.opts.SkipDaemonSets, d.opts.Force, d.opts.DeleteEmptyDir)
	if len(blocked) > 0 {
		for _, b := range blocked {
			out.Warnf(d.opts.NodeName, "Cannot evict %s/%s: %s", b.pod.Namespace, b.pod.Name, b.reason)
		}
		if !d.opts.DryRun {
			first := blocked[0]
			return fmt.Errorf("remaining pod %s/%s cannot be evicted: %s", first.pod.Namespace, first.pod.Name, first.reason)
		}
	}

	evictable := filterEvictable(candidates, d.opts.SkipDaemonSets, d.opts.Force, d.opts.DeleteEmptyDir)
	waiting := podsPendingDeletion(candidates, d.opts.SkipDaemonSets)
	if len(evictable) == 0 {
		if len(waiting) > 0 && !d.opts.DryRun {
			return d.waitForPodsDeleted(ctx, waiting, d.podVacateTimeout())
		}
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
			if err := d.pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
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

	if d.opts.DryRun {
		return nil
	}
	return d.waitForPodsDeleted(ctx, append(waiting, evictable...), d.podVacateTimeout())
}

func (d *Drainer) excludeProtectedPods(state *runState, pods []corev1.Pod) []corev1.Pod {
	if len(state.protectedWorkloads) == 0 {
		return pods
	}
	out := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if protected := d.protectedWorkloadForPod(state, pod); protected != "" {
			d.opts.Out.Infof(d.opts.NodeName, "Leaving %s/%s untouched (%s is filtered)", pod.Namespace, pod.Name, protected)
			continue
		}
		out = append(out, *pod)
	}
	return out
}

func (d *Drainer) protectedWorkloadForPod(state *runState, pod *corev1.Pod) string {
	for _, w := range state.protectedWorkloads {
		if pod.Namespace != w.Namespace || w.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(w.Selector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			return wSubject(w)
		}
	}
	return ""
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
		err := d.pods(pod.Namespace).EvictV1(evictCtx, buildEviction(pod, d.opts.GracePeriod))
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
			case <-d.after(interval):
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

func podsPendingDeletion(pods []corev1.Pod, skipDaemonSets bool) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp == nil || workload.IsTerminalPod(pod) || isMirrorPod(pod) {
			continue
		}
		if isDaemonSetPod(pod) && skipDaemonSets {
			continue
		}
		out = append(out, *pod)
	}
	return out
}

func (d *Drainer) waitForPodsDeleted(ctx context.Context, pods []corev1.Pod, timeout time.Duration) error {
	if len(pods) == 0 {
		return nil
	}
	seen := make(map[types.UID]corev1.Pod, len(pods))
	for i := range pods {
		pod := pods[i]
		if pod.UID != "" {
			seen[pod.UID] = pod
			continue
		}
		// Tests sometimes omit UIDs; synthesize a stable key from namespace/name.
		pod.UID = types.UID(pod.Namespace + "/" + pod.Name)
		seen[pod.UID] = pod
	}
	d.opts.Out.Pollf(d.opts.NodeName, "Waiting for %d pod(s) to be deleted from %q (timeout=%s)", len(seen), d.opts.NodeName, timeout)
	return wait.PollUntilContextTimeout(ctx, d.pollInterval(), timeout, true,
		func(ctx context.Context) (bool, error) {
			remaining := 0
			for uid, pod := range seen {
				current, err := d.pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
				if k8serrors.IsNotFound(err) {
					delete(seen, uid)
					continue
				}
				if err != nil {
					if isTransientAPIError(err) {
						return false, nil
					}
					return false, err
				}
				if current.UID != "" && pod.UID != "" && current.UID != pod.UID {
					delete(seen, uid)
					continue
				}
				if workload.IsTerminalPod(current) {
					delete(seen, uid)
					continue
				}
				remaining++
			}
			if remaining > 0 {
				d.opts.Out.Pollf(d.opts.NodeName, "%d pod(s) still deleting on %q", remaining, d.opts.NodeName)
				return false, nil
			}
			return true, nil
		},
	)
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
