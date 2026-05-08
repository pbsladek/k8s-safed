// Package workload resolves pod owners (Deployment, StatefulSet) on a node.
package workload

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	Kind       Kind
	Namespace  string
	Name       string
	UID        types.UID
	Generation int64
	Selector   *metav1.LabelSelector
	// Priority controls drain order. Lower values are restarted first.
	// Populated from the kubectl.safed.io/drain-priority annotation;
	// defaults to DefaultDrainPriority (100) when the annotation is absent.
	Priority                  int
	PriorityAnnotationValue   string
	PriorityAnnotationInvalid bool
}

// parseDrainPriority reads the drain-priority annotation and returns its
// integer value, or DefaultDrainPriority if the annotation is absent or
// cannot be parsed.
func parseDrainPriority(annotations map[string]string) (int, string, bool) {
	v, ok := annotations[DrainPriorityAnnotation]
	if !ok {
		return DefaultDrainPriority, "", false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return DefaultDrainPriority, v, true
	}
	return n, v, false
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
	// Caches are request-scoped. Workloads and owner chains can change during a
	// long-running process, so each discovery pass starts fresh.
	f.rsCache = make(map[string]*appsv1.ReplicaSet)
	f.wlCache = make(map[string]Workload)

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
	ref, ok := controllerOwnerRef(pod.OwnerReferences)
	if !ok {
		return Workload{}, false, nil
	}
	switch ref.Kind {
	case "ReplicaSet":
		return f.resolveReplicaSet(ctx, pod.Namespace, ref)
	case "StatefulSet":
		return f.resolveStatefulSet(ctx, pod.Namespace, ref)
	case "DaemonSet", "Job", "CronJob":
		// Not managed via rolling restarts; evictRemaining handles these.
		return Workload{}, false, nil
	default:
		return Workload{}, false, nil
	}
}

func controllerOwnerRef(refs []metav1.OwnerReference) (metav1.OwnerReference, bool) {
	for _, ref := range refs {
		if ref.Controller != nil && !*ref.Controller {
			continue
		}
		if ref.Controller == nil {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
			return ref, true
		}
	}
	return metav1.OwnerReference{}, false
}

// resolveReplicaSet fetches the ReplicaSet (cached), then walks its owner
// references to find a Deployment. Returns (_, false, nil) if the ReplicaSet
// has no Deployment owner (standalone RS).
func (f *Finder) resolveReplicaSet(ctx context.Context, namespace string, ref metav1.OwnerReference) (Workload, bool, error) {
	rsKey := namespace + "/" + ref.Name
	rs, ok := f.rsCache[rsKey]
	if !ok {
		var err error
		rs, err = f.client.AppsV1().ReplicaSets(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			// RS was deleted between the pod LIST and this GET (terminating pod
			// from a previous rollout). Treat as orphaned — skip.
			return Workload{}, false, nil
		}
		if err != nil {
			return Workload{}, false, fmt.Errorf("getting ReplicaSet %s/%s: %w", namespace, ref.Name, err)
		}
		f.rsCache[rsKey] = rs
	}
	if ref.UID != "" && rs.UID != ref.UID {
		return Workload{}, false, nil
	}

	depRef, ok := controllerOwnerRef(rs.OwnerReferences)
	if ok && depRef.Kind == "Deployment" {
		return f.resolveDeployment(ctx, namespace, depRef)
	}
	// Standalone ReplicaSet (no Deployment owner) — skip.
	return Workload{}, false, nil
}

// resolveDeployment fetches the Deployment and returns a Workload with its pod
// selector. Results are cached so multiple RS owners of the same Deployment
// only incur one API call.
func (f *Finder) resolveDeployment(ctx context.Context, namespace string, ref metav1.OwnerReference) (Workload, bool, error) {
	name := ref.Name
	wlKey := fmt.Sprintf("Deployment/%s/%s", namespace, name)
	if w, ok := f.wlCache[wlKey]; ok {
		return w, true, nil
	}

	dep, err := f.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return Workload{}, false, nil
	}
	if err != nil {
		return Workload{}, false, fmt.Errorf("getting Deployment %s/%s: %w", namespace, name, err)
	}
	if ref.UID != "" && dep.UID != ref.UID {
		return Workload{}, false, nil
	}

	priority, priorityValue, priorityInvalid := parseDrainPriority(dep.Annotations)
	w := Workload{
		Kind:                      KindDeployment,
		Namespace:                 namespace,
		Name:                      name,
		UID:                       dep.UID,
		Generation:                dep.Generation,
		Selector:                  dep.Spec.Selector,
		Priority:                  priority,
		PriorityAnnotationValue:   priorityValue,
		PriorityAnnotationInvalid: priorityInvalid,
	}
	f.wlCache[wlKey] = w
	return w, true, nil
}

// resolveStatefulSet fetches the StatefulSet and returns a Workload with its
// pod selector. Results are cached.
func (f *Finder) resolveStatefulSet(ctx context.Context, namespace string, ref metav1.OwnerReference) (Workload, bool, error) {
	name := ref.Name
	wlKey := fmt.Sprintf("StatefulSet/%s/%s", namespace, name)
	if w, ok := f.wlCache[wlKey]; ok {
		return w, true, nil
	}

	sts, err := f.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return Workload{}, false, nil
	}
	if err != nil {
		return Workload{}, false, fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, name, err)
	}
	if ref.UID != "" && sts.UID != ref.UID {
		return Workload{}, false, nil
	}

	priority, priorityValue, priorityInvalid := parseDrainPriority(sts.Annotations)
	w := Workload{
		Kind:                      KindStatefulSet,
		Namespace:                 namespace,
		Name:                      name,
		UID:                       sts.UID,
		Generation:                sts.Generation,
		Selector:                  sts.Spec.Selector,
		Priority:                  priority,
		PriorityAnnotationValue:   priorityValue,
		PriorityAnnotationInvalid: priorityInvalid,
	}
	f.wlCache[wlKey] = w
	return w, true, nil
}
