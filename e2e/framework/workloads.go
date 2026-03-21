//go:build e2e

// Package framework provides helpers for k8s-safed e2e tests.
package framework

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// --------------------------------------------------------------------------
// Raw manifests — applied with kubectl, not helm
// --------------------------------------------------------------------------

// WorkerManifest is a single-replica Deployment used only by preflight tests
// to trigger the "single-replica risk" detection.
const WorkerManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
  namespace: e2e
spec:
  replicas: 1
  selector:
    matchLabels:
      app: worker
  template:
    metadata:
      labels:
        app: worker
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: worker
        image: busybox:1.36
        command: ["/bin/sh", "-c", "while true; do sleep 3600; done"]
        resources:
          requests:
            cpu: 10m
            memory: 16Mi
`

// DaemonSetManifest is a DaemonSet deployed to verify that safed never adds a
// restartedAt annotation to DaemonSet pod templates.
const DaemonSetManifest = `
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-agent
  namespace: e2e
spec:
  selector:
    matchLabels:
      app: node-agent
  template:
    metadata:
      labels:
        app: node-agent
    spec:
      terminationGracePeriodSeconds: 2
      tolerations:
      - operator: Exists
      containers:
      - name: agent
        image: busybox:1.36
        command: ["/bin/sh", "-c", "while true; do sleep 3600; done"]
        resources:
          requests:
            cpu: 10m
            memory: 16Mi
`

// BlockingPDBManifest creates a Deployment + PodDisruptionBudget whose
// maxUnavailable=0 blocks all evictions. Used to verify evictWithPDBRetry
// respects the eviction-timeout. The PDB is deliberately misconfigured so
// the drain must time out cleanly rather than hang.
const BlockingPDBManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pdb-target
  namespace: e2e
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pdb-target
  template:
    metadata:
      labels:
        app: pdb-target
    spec:
      terminationGracePeriodSeconds: 2
      containers:
      - name: app
        image: busybox:1.36
        command: ["/bin/sh", "-c", "while true; do sleep 3600; done"]
        resources:
          requests:
            cpu: 10m
            memory: 16Mi
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: pdb-target
  namespace: e2e
spec:
  maxUnavailable: 0
  selector:
    matchLabels:
      app: pdb-target
`

// CrashingDeploymentManifest is a single-replica Deployment whose container
// exits immediately (exit 1). It enters CrashLoopBackOff, allowing e2e tests
// to verify that kubectl-safed aborts a drain when a fail-fast condition is detected.
const CrashingDeploymentManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: crasher
  namespace: e2e
spec:
  replicas: 1
  selector:
    matchLabels:
      app: crasher
  template:
    metadata:
      labels:
        app: crasher
    spec:
      terminationGracePeriodSeconds: 1
      containers:
      - name: crasher
        image: busybox:1.36
        command: ["/bin/sh", "-c", "exit 1"]
        resources:
          requests:
            cpu: 10m
            memory: 8Mi
`

// StandalonePodWithPDBManifest is a standalone (unowned) Pod plus a
// PodDisruptionBudget with maxUnavailable=0. Used to verify that
// evictWithPDBRetry correctly times out when a PDB permanently blocks eviction.
const StandalonePodWithPDBManifest = `
apiVersion: v1
kind: Pod
metadata:
  name: pdb-standalone
  namespace: e2e
  labels:
    app: pdb-standalone
spec:
  terminationGracePeriodSeconds: 2
  containers:
  - name: app
    image: busybox:1.36
    command: ["/bin/sh", "-c", "while true; do sleep 3600; done"]
    resources:
      requests:
        cpu: 10m
        memory: 8Mi
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: pdb-standalone
  namespace: e2e
spec:
  maxUnavailable: 0
  selector:
    matchLabels:
      app: pdb-standalone
`

// --------------------------------------------------------------------------
// Known resource names for helm releases
// --------------------------------------------------------------------------

// Label selectors for locating pods from each helm release.
const (
	NATSPodSelector             = "app.kubernetes.io/component=nats,app.kubernetes.io/instance=nats"
	GrafanaPodSelector          = "app.kubernetes.io/name=grafana,app.kubernetes.io/instance=grafana"
	KubeStateMetricsPodSelector = "app.kubernetes.io/name=kube-state-metrics,app.kubernetes.io/instance=kube-state-metrics"
)

// Label selectors for raw-manifest workloads.
const (
	WorkerPodSelector         = "app=worker"
	CrasherPodSelector        = "app=crasher"
	StandalonePDBPodSelector  = "app=pdb-standalone"
)

// Resource names as created by the helm charts.
const (
	NATSStatefulSetName            = "nats"
	GrafanaDeploymentName          = "grafana"
	KubeStateMetricsDeploymentName = "kube-state-metrics"
)

// --------------------------------------------------------------------------
// Wait helpers
// --------------------------------------------------------------------------

// WaitForCoreWorkloads waits for NATS and Grafana to be fully ready.
func WaitForCoreWorkloads(ctx context.Context, client kubernetes.Interface, ns string, timeout time.Duration) error {
	if err := WaitForStatefulSetReady(ctx, client, ns, NATSStatefulSetName, timeout); err != nil {
		return fmt.Errorf("wait for NATS StatefulSet: %w", err)
	}
	if err := WaitForDeploymentReady(ctx, client, ns, GrafanaDeploymentName, timeout); err != nil {
		return fmt.Errorf("wait for Grafana Deployment: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------------
// Restart annotation helpers
// --------------------------------------------------------------------------

const restartAnnotationKey = "kubectl.kubernetes.io/restartedAt"

// GetRestartAnnotation returns the restartedAt annotation from the pod
// template of a StatefulSet or Deployment. Returns "" when not set.
func GetRestartAnnotation(ctx context.Context, client kubernetes.Interface, ns, kind, name string) (string, error) {
	switch kind {
	case "StatefulSet":
		obj, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return obj.Spec.Template.Annotations[restartAnnotationKey], nil
	case "Deployment":
		obj, err := client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return obj.Spec.Template.Annotations[restartAnnotationKey], nil
	default:
		return "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// --------------------------------------------------------------------------
// Pod-placement helpers
// --------------------------------------------------------------------------

// AgentNodeWithPod returns the name of an agent node that has at least one
// Running pod matching labelSelector in ns. Returns "" and calls t.Skip if
// no agent node currently has such a pod (e.g. scheduler put everything on
// the server node or the other agent).
//
// Call this at the start of every drain test so we know the target node
// actually has work to drain.
func AgentNodeWithPod(
	ctx context.Context,
	client kubernetes.Interface,
	cluster *Cluster,
	ns, labelSelector string,
) (string, error) {
	agents, err := cluster.AgentNodeNames(ctx)
	if err != nil || len(agents) == 0 {
		return "", fmt.Errorf("no agent nodes: %w", err)
	}
	agentSet := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentSet[a] = true
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return "", err
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && agentSet[p.Spec.NodeName] {
				return p.Spec.NodeName, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no running pod with %q on any agent node within 30s", labelSelector)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
