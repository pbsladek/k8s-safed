//go:build e2e

package framework

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes client from the kubeconfig file at path.
func NewClient(kubeconfigPath string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("new kubernetes client: %w", err)
	}
	return client, nil
}

// EnsureNamespace creates the namespace if it does not already exist.
func EnsureNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}
	return nil
}

// NodeHasActivePods returns true if there are non-DaemonSet, non-terminal,
// non-mirror pods in ns on nodeName.
func NodeHasActivePods(ctx context.Context, client kubernetes.Interface, nodeName, ns string) (bool, error) {
	list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return false, err
	}
	for _, p := range list.Items {
		if isTerminalPod(p) || isDaemonSetPod(p) || isMirrorPod(p) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// EventsForNode returns Kubernetes Events on the given node (in "default"
// namespace, where node events are recorded).
func EventsForNode(ctx context.Context, client kubernetes.Interface, nodeName string) ([]corev1.Event, error) {
	list, err := client.CoreV1().Events("default").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName),
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// --------------------------------------------------------------------------
// Wait helpers
// --------------------------------------------------------------------------

// WaitForDeploymentReady polls until all replicas of the deployment are ready.
func WaitForDeploymentReady(ctx context.Context, client kubernetes.Interface, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		dep, err := client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		s := dep.Status
		if s.ReadyReplicas == desired && s.AvailableReplicas == desired && s.UnavailableReplicas == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s not ready after %v (ready=%d/%d unavail=%d)",
				ns, name, timeout, s.ReadyReplicas, desired, s.UnavailableReplicas)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// WaitForStatefulSetReady polls until all replicas of the StatefulSet are ready.
func WaitForStatefulSetReady(ctx context.Context, client kubernetes.Interface, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		desired := int32(1)
		if sts.Spec.Replicas != nil {
			desired = *sts.Spec.Replicas
		}
		if sts.Status.ReadyReplicas == desired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("statefulset %s/%s not ready after %v (ready=%d/%d)",
				ns, name, timeout, sts.Status.ReadyReplicas, desired)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// WaitForNoActivePodsOnNode polls until the given node has no non-DaemonSet,
// non-terminal pods in ns.
func WaitForNoActivePodsOnNode(ctx context.Context, client kubernetes.Interface, nodeName, ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		has, err := NodeHasActivePods(ctx, client, nodeName, ns)
		if err != nil {
			return err
		}
		if !has {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for no active pods on node %s", nodeName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// NodeWithPodForLabel returns the first node that has a Running pod matching
// labelSelector in ns, waiting up to timeout.
func NodeWithPodForLabel(ctx context.Context, client kubernetes.Interface, ns, labelSelector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return "", err
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
				return p.Spec.NodeName, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no running pod with label %s in %s within %v", labelSelector, ns, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// NodeHasActivePodsWithSelector returns true if the node has at least one
// non-DaemonSet, non-terminal pod in ns matching labelSelector.
func NodeHasActivePodsWithSelector(ctx context.Context, client kubernetes.Interface, nodeName, ns, labelSelector string) (bool, error) {
	list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return false, err
	}
	for _, p := range list.Items {
		if isTerminalPod(p) || isDaemonSetPod(p) || isMirrorPod(p) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// WaitForPodsOnAgentNodes polls until at least atLeast Running pods matching
// labelSelector are found on nodes in agentNames. Use this after uncordoning
// to ensure pods have rescheduled back onto agent nodes before the next test.
func WaitForPodsOnAgentNodes(ctx context.Context, client kubernetes.Interface, agentNames []string, ns, labelSelector string, atLeast int, timeout time.Duration) error {
	agentSet := make(map[string]bool, len(agentNames))
	for _, a := range agentNames {
		agentSet[a] = true
	}
	deadline := time.Now().Add(timeout)
	for {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return err
		}
		count := 0
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && agentSet[p.Spec.NodeName] {
				count++
			}
		}
		if count >= atLeast {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out: only %d/%d pods with %q running on agent nodes after %v", count, atLeast, labelSelector, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// WaitForCrashingPod polls until at least one pod matching labelSelector has a
// container with LastTerminationState.Terminated set (i.e. has crashed at least
// once). Returns the node name where the crashing pod is running.
func WaitForCrashingPod(ctx context.Context, client kubernetes.Interface, ns, labelSelector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return "", err
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == "" {
				continue
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated != nil {
					return p.Spec.NodeName, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no crashing pod with selector %q found within %v", labelSelector, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// --------------------------------------------------------------------------
// internal helpers
// --------------------------------------------------------------------------

func isTerminalPod(p corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func isDaemonSetPod(p corev1.Pod) bool {
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(p corev1.Pod) bool {
	_, ok := p.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}
