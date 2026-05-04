// Package framework provides helpers for k8s-safed e2e tests.
package framework

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DeploymentManifestOptions describes a small Deployment used by e2e tests.
type DeploymentManifestOptions struct {
	Namespace               string
	Name                    string
	Priority                int
	Replicas                int32
	MinReadySeconds         int32
	ProgressDeadlineSeconds int32
	Recreate                bool
	Image                   string
	Command                 string
	ReadinessCommand        string
}

// --------------------------------------------------------------------------
// Templated manifests — applied with kubectl, not helm
// --------------------------------------------------------------------------

// DeploymentManifest returns a small busybox Deployment with app=<name>.
func DeploymentManifest(opts DeploymentManifestOptions) string {
	data := deploymentTemplateData{
		Namespace:               namespaceOrDefault(opts.Namespace),
		Name:                    opts.Name,
		IncludePriority:         true,
		Priority:                opts.Priority,
		Replicas:                defaultInt32(opts.Replicas, 1),
		MinReadySeconds:         opts.MinReadySeconds,
		ProgressDeadlineSeconds: opts.ProgressDeadlineSeconds,
		Recreate:                opts.Recreate,
		ContainerName:           "app",
		TerminationGraceSeconds: 2,
		Image:                   defaultString(opts.Image, busyboxImage),
		Command:                 defaultString(opts.Command, sleepCommand),
		MemoryRequest:           "16Mi",
		ReadinessCommand:        opts.ReadinessCommand,
	}
	return renderManifest(deploymentTemplate, data)
}

// StandalonePodManifest returns an unmanaged Pod with app=<name>.
func StandalonePodManifest(namespace, name string, emptyDir bool) string {
	return renderManifest(podTemplate, podTemplateData{
		Namespace: namespaceOrDefault(namespace),
		Name:      name,
		App:       name,
		Image:     busyboxImage,
		Command:   sleepCommand,
		EmptyDir:  emptyDir,
	})
}

// ReplicaSetManifest returns a standalone ReplicaSet with app=<name>.
func ReplicaSetManifest(namespace, name string, emptyDir bool) string {
	return renderManifest(replicaSetTemplate, replicaSetTemplateData{
		Namespace: namespaceOrDefault(namespace),
		Name:      name,
		App:       name,
		Image:     busyboxImage,
		Command:   sleepCommand,
		EmptyDir:  emptyDir,
	})
}

// JobManifest returns a long-running Job with app=<name>.
func JobManifest(namespace, name string) string {
	return renderManifest(jobTemplate, jobTemplateData{
		Namespace: namespaceOrDefault(namespace),
		Name:      name,
		App:       name,
		Image:     busyboxImage,
		Command:   sleepCommand,
	})
}

// PDBManifest returns a PodDisruptionBudget selecting app=<app>.
func PDBManifest(namespace, name, app string, maxUnavailable int) string {
	return renderManifest(pdbTemplate, pdbTemplateData{
		Namespace:      namespaceOrDefault(namespace),
		Name:           name,
		App:            app,
		MaxUnavailable: maxUnavailable,
	})
}

func namespaceOrDefault(ns string) string {
	if ns != "" {
		return ns
	}
	return E2ENamespace
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func defaultInt32(value, fallback int32) int32 {
	if value != 0 {
		return value
	}
	return fallback
}

const (
	busyboxImage = "mirror.gcr.io/library/busybox:1.36"
	sleepCommand = "while true; do sleep 3600; done"
)

type deploymentTemplateData struct {
	Namespace               string
	Name                    string
	IncludePriority         bool
	Priority                int
	Replicas                int32
	MinReadySeconds         int32
	ProgressDeadlineSeconds int32
	Recreate                bool
	ContainerName           string
	TerminationGraceSeconds int
	Image                   string
	Command                 string
	MemoryRequest           string
	ReadinessCommand        string
}

type podTemplateData struct {
	Namespace string
	Name      string
	App       string
	Image     string
	Command   string
	EmptyDir  bool
}

type replicaSetTemplateData struct {
	Namespace string
	Name      string
	App       string
	Image     string
	Command   string
	EmptyDir  bool
}

type jobTemplateData struct {
	Namespace string
	Name      string
	App       string
	Image     string
	Command   string
}

type pdbTemplateData struct {
	Namespace      string
	Name           string
	App            string
	MaxUnavailable int
}

type daemonSetTemplateData struct {
	Namespace string
	Name      string
	App       string
	Image     string
	Command   string
}

func renderManifest(tpl *template.Template, data any) string {
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		panic(fmt.Sprintf("render e2e manifest template %s: %v", tpl.Name(), err))
	}
	return buf.String()
}

//go:embed testdata/manifests/*.yaml.tmpl
var manifestTemplateFS embed.FS

func mustManifestTemplate(name string) *template.Template {
	path := "testdata/manifests/" + name + ".yaml.tmpl"
	content, err := manifestTemplateFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("read e2e manifest template %s: %v", path, err))
	}
	return template.Must(template.New(name).Parse(string(content)))
}

func joinManifestDocuments(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return "\n" + strings.Join(cleaned, "\n---\n") + "\n"
}

var deploymentTemplate = mustManifestTemplate("deployment")
var podTemplate = mustManifestTemplate("pod")
var replicaSetTemplate = mustManifestTemplate("replicaset")
var jobTemplate = mustManifestTemplate("job")
var pdbTemplate = mustManifestTemplate("pdb")
var daemonSetTemplate = mustManifestTemplate("daemonset")

// WorkerManifest is a single-replica Deployment used only by preflight tests
// to trigger the "single-replica risk" detection.
var WorkerManifest = renderManifest(deploymentTemplate, deploymentTemplateData{
	Namespace:               E2ENamespace,
	Name:                    "worker",
	Replicas:                1,
	ContainerName:           "worker",
	TerminationGraceSeconds: 2,
	Image:                   busyboxImage,
	Command:                 sleepCommand,
	MemoryRequest:           "16Mi",
})

// DaemonSetManifest is a DaemonSet deployed to verify that safed never adds a
// restartedAt annotation to DaemonSet pod templates.
var DaemonSetManifest = renderManifest(daemonSetTemplate, daemonSetTemplateData{
	Namespace: E2ENamespace,
	Name:      "node-agent",
	App:       "node-agent",
	Image:     busyboxImage,
	Command:   sleepCommand,
})

// BlockingPDBManifest creates a Deployment + PodDisruptionBudget whose
// maxUnavailable=0 blocks all evictions. Used to verify evictWithPDBRetry
// respects the eviction-timeout. The PDB is deliberately misconfigured so
// the drain must time out cleanly rather than hang.
var BlockingPDBManifest = joinManifestDocuments(
	renderManifest(deploymentTemplate, deploymentTemplateData{
		Namespace:               E2ENamespace,
		Name:                    "pdb-target",
		Replicas:                1,
		ContainerName:           "app",
		TerminationGraceSeconds: 2,
		Image:                   busyboxImage,
		Command:                 sleepCommand,
		MemoryRequest:           "16Mi",
	}),
	PDBManifest(E2ENamespace, "pdb-target", "pdb-target", 0),
)

// CrashingDeploymentManifest is a single-replica Deployment whose container
// exits immediately (exit 1). It enters CrashLoopBackOff, allowing e2e tests
// to verify that kubectl-safed aborts a drain when a fail-fast condition is detected.
var CrashingDeploymentManifest = renderManifest(deploymentTemplate, deploymentTemplateData{
	Namespace:               E2ENamespace,
	Name:                    "crasher",
	Replicas:                1,
	ContainerName:           "crasher",
	TerminationGraceSeconds: 1,
	Image:                   busyboxImage,
	Command:                 "exit 1",
	MemoryRequest:           "8Mi",
})

// StandalonePodWithPDBManifest is a standalone (unowned) Pod plus a
// PodDisruptionBudget with maxUnavailable=0. Used to verify that
// evictWithPDBRetry correctly times out when a PDB permanently blocks eviction.
var StandalonePodWithPDBManifest = joinManifestDocuments(
	StandalonePodManifest(E2ENamespace, "pdb-standalone", false),
	PDBManifest(E2ENamespace, "pdb-standalone", "pdb-standalone", 0),
)

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
	WorkerPodSelector        = "app=worker"
	CrasherPodSelector       = "app=crasher"
	StandalonePDBPodSelector = "app=pdb-standalone"
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
