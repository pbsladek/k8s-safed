// Package workload resolves pod owners (Deployment, StatefulSet) on a node.
package workload

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Kind represents the type of a managed workload.
type Kind string

const (
	KindDeployment  Kind = "Deployment"
	KindStatefulSet Kind = "StatefulSet"
)

// DrainPriorityAnnotation is the annotation key used to control drain order.
// Lower values are drained first. Workloads without this annotation use
// DefaultDrainPriority (100).
const DrainPriorityAnnotation = "kubectl.safed.io/drain-priority"

// DefaultDrainPriority is the priority assigned to unannotated workloads.
const DefaultDrainPriority = 100

// Workload holds a reference to a managed workload that owns pods on a node.
// Selector is populated by FindForNode so callers can verify pods have left
// the node without a second API call.
type Workload struct {
	Kind      Kind
	Namespace string
	Name      string
	Selector  *metav1.LabelSelector
	// Priority controls drain order. Lower values are restarted first.
	// Populated from the kubectl.safed.io/drain-priority annotation;
	// defaults to DefaultDrainPriority (100) when the annotation is absent.
	Priority int
}

// parseDrainPriority reads the drain-priority annotation and returns its
// integer value, or DefaultDrainPriority if the annotation is absent or
// cannot be parsed.
func parseDrainPriority(annotations map[string]string) int {
	v, ok := annotations[DrainPriorityAnnotation]
	if !ok {
		return DefaultDrainPriority
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return DefaultDrainPriority
	}
	return n
}

func (w Workload) String() string {
	return fmt.Sprintf("%s %s/%s", w.Kind, w.Namespace, w.Name)
}

// IsTerminalPod reports whether a pod has reached a terminal phase
// (Succeeded or Failed) and will never be rescheduled.
func IsTerminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded ||
		pod.Status.Phase == corev1.PodFailed
}

// Finder locates workloads that own pods scheduled on a given node.
// It caches ReplicaSet and workload lookups to avoid redundant API calls
// when multiple pods on the same node share an owner.
type Finder struct {
	client  kubernetes.Interface
	rsCache map[string]*appsv1.ReplicaSet // "namespace/name" → *ReplicaSet
	wlCache map[string]Workload           // "Kind/namespace/name" → Workload
}

// NewFinder creates a new Finder.
func NewFinder(client kubernetes.Interface) *Finder {
	return &Finder{
		client:  client,
		rsCache: make(map[string]*appsv1.ReplicaSet),
		wlCache: make(map[string]Workload),
	}
}

// FindForNode returns the deduplicated set of Deployments and StatefulSets that
// have at least one non-terminal pod on nodeName. Each Workload carries its pod
// label Selector so downstream callers can check node vacancy without extra API
// calls.
//
// Pod owner references are fully resolved in a single pass:
//
//	Pod → ReplicaSet → Deployment   (cached RS lookups)
//	Pod → StatefulSet               (direct)
//
// DaemonSet, Job, and standalone pods are excluded; they are handled by the
// conventional eviction path.
func (f *Finder) FindForNode(ctx context.Context, nodeName string) ([]Workload, error) {
	// List all pods on the node without a phase filter so we see terminal pods
	// explicitly and can skip them rather than accidentally triggering restarts.
	pods, err := f.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods on node %q: %w", nodeName, err)
	}

	seen := map[string]struct{}{}
	var workloads []Workload

	for i := range pods.Items {
		pod := &pods.Items[i]

		// Completed or failed pods do not need draining.
		if IsTerminalPod(pod) {
			continue
		}

		w, found, err := f.resolveOwner(ctx, pod)
		if err != nil {
			return nil, fmt.Errorf("resolving owner for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		if !found {
			continue
		}

		key := fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		workloads = append(workloads, w)
	}

	return workloads, nil
}

// resolveOwner walks the pod's owner reference chain to find the top-level
// Deployment or StatefulSet. Returns (workload, true, nil) on success,
// (_, false, nil) when the pod belongs to a non-rollable owner type,
// and (_, false, err) on API errors.
func (f *Finder) resolveOwner(ctx context.Context, pod *corev1.Pod) (Workload, bool, error) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			return f.resolveReplicaSet(ctx, pod.Namespace, ref.Name)
		case "StatefulSet":
			return f.resolveStatefulSet(ctx, pod.Namespace, ref.Name)
		case "DaemonSet", "Job", "CronJob":
			// Not managed via rolling restarts; evictRemaining handles these.
			return Workload{}, false, nil
		}
	}
	// Standalone pod — no managed owner.
	return Workload{}, false, nil
}

// resolveReplicaSet fetches the ReplicaSet (cached), then walks its owner
// references to find a Deployment. Returns (_, false, nil) if the ReplicaSet
// has no Deployment owner (standalone RS).
func (f *Finder) resolveReplicaSet(ctx context.Context, namespace, rsName string) (Workload, bool, error) {
	rsKey := namespace + "/" + rsName
	rs, ok := f.rsCache[rsKey]
	if !ok {
		var err error
		rs, err = f.client.AppsV1().ReplicaSets(namespace).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return Workload{}, false, fmt.Errorf("getting ReplicaSet %s/%s: %w", namespace, rsName, err)
		}
		f.rsCache[rsKey] = rs
	}

	for _, ref := range rs.OwnerReferences {
		if ref.Kind == "Deployment" {
			return f.resolveDeployment(ctx, namespace, ref.Name)
		}
	}
	// Standalone ReplicaSet (no Deployment owner) — skip.
	return Workload{}, false, nil
}

// resolveDeployment fetches the Deployment and returns a Workload with its pod
// selector. Results are cached so multiple RS owners of the same Deployment
// only incur one API call.
func (f *Finder) resolveDeployment(ctx context.Context, namespace, name string) (Workload, bool, error) {
	wlKey := fmt.Sprintf("Deployment/%s/%s", namespace, name)
	if w, ok := f.wlCache[wlKey]; ok {
		return w, true, nil
	}

	dep, err := f.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Workload{}, false, fmt.Errorf("getting Deployment %s/%s: %w", namespace, name, err)
	}

	w := Workload{
		Kind:      KindDeployment,
		Namespace: namespace,
		Name:      name,
		Selector:  dep.Spec.Selector,
		Priority:  parseDrainPriority(dep.Annotations),
	}
	f.wlCache[wlKey] = w
	return w, true, nil
}

// resolveStatefulSet fetches the StatefulSet and returns a Workload with its
// pod selector. Results are cached.
func (f *Finder) resolveStatefulSet(ctx context.Context, namespace, name string) (Workload, bool, error) {
	wlKey := fmt.Sprintf("StatefulSet/%s/%s", namespace, name)
	if w, ok := f.wlCache[wlKey]; ok {
		return w, true, nil
	}

	sts, err := f.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Workload{}, false, fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, name, err)
	}

	w := Workload{
		Kind:      KindStatefulSet,
		Namespace: namespace,
		Name:      name,
		Selector:  sts.Spec.Selector,
		Priority:  parseDrainPriority(sts.Annotations),
	}
	f.wlCache[wlKey] = w
	return w, true, nil
}
