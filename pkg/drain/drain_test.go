package drain

import (
	"context"
	"io"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/k8s"
	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func discardPrinter() *Printer { return NewPrinterTo(io.Discard) }

func newTestDrainer(t *testing.T, nodeName string, fakeCS *fake.Clientset, extra ...func(*Options)) *Drainer {
	t.Helper()
	opts := Options{
		Client:         &k8s.Client{Kubernetes: fakeCS},
		NodeName:       nodeName,
		SkipDaemonSets: true,
		RolloutTimeout: 2 * time.Second,
		PollInterval:   1 * time.Millisecond,
		GracePeriod:    -1,
		Out:            discardPrinter(),
	}
	for _, fn := range extra {
		fn(&opts)
	}
	return NewDrainer(opts)
}

func ownerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name}
}

func readyNode(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// --------------------------------------------------------------------------
// filterEvictable
// --------------------------------------------------------------------------

func TestFilterEvictable(t *testing.T) {
	mirrorPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "mirror",
			Annotations: map[string]string{corev1.MirrorPodAnnotationKey: "true"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	terminatingPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	succeededPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "succeeded"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	dsPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ds-pod",
			OwnerReferences: []metav1.OwnerReference{ownerRef("DaemonSet", "node-agent")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	jobPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "job-pod",
			OwnerReferences: []metav1.OwnerReference{ownerRef("Job", "batch")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	standalonePod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	emptyDirPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "emptydir",
			OwnerReferences: []metav1.OwnerReference{ownerRef("ReplicaSet", "rs1")},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "tmp",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	normalPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "normal",
			OwnerReferences: []metav1.OwnerReference{ownerRef("ReplicaSet", "rs1")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	tests := []struct {
		name           string
		pods           []corev1.Pod
		skipDaemonSets bool
		force          bool
		deleteEmptyDir bool
		wantCount      int
		wantNames      []string
	}{
		{
			name:           "mirror pod always skipped",
			pods:           []corev1.Pod{mirrorPod},
			skipDaemonSets: true,
			wantCount:      0,
		},
		{
			name:           "terminating pod skipped",
			pods:           []corev1.Pod{terminatingPod},
			skipDaemonSets: true,
			wantCount:      0,
		},
		{
			name:           "succeeded pod skipped",
			pods:           []corev1.Pod{succeededPod},
			skipDaemonSets: true,
			wantCount:      0,
		},
		{
			name:           "daemonset pod skipped when skipDaemonSets=true",
			pods:           []corev1.Pod{dsPod},
			skipDaemonSets: true,
			wantCount:      0,
		},
		{
			name:           "daemonset pod included when skipDaemonSets=false",
			pods:           []corev1.Pod{dsPod},
			skipDaemonSets: false,
			force:          true,
			wantCount:      1,
			wantNames:      []string{"ds-pod"},
		},
		{
			name:           "job pod skipped without force",
			pods:           []corev1.Pod{jobPod},
			skipDaemonSets: true,
			force:          false,
			wantCount:      0,
		},
		{
			name:           "job pod included with force",
			pods:           []corev1.Pod{jobPod},
			skipDaemonSets: true,
			force:          true,
			wantCount:      1,
			wantNames:      []string{"job-pod"},
		},
		{
			name:           "standalone pod skipped without force",
			pods:           []corev1.Pod{standalonePod},
			skipDaemonSets: true,
			force:          false,
			wantCount:      0,
		},
		{
			name:           "standalone pod included with force",
			pods:           []corev1.Pod{standalonePod},
			skipDaemonSets: true,
			force:          true,
			wantCount:      1,
			wantNames:      []string{"standalone"},
		},
		{
			name:           "emptydir pod skipped without deleteEmptyDir or force",
			pods:           []corev1.Pod{emptyDirPod},
			skipDaemonSets: true,
			deleteEmptyDir: false,
			force:          false,
			wantCount:      0,
		},
		{
			name:           "emptydir pod included with deleteEmptyDir",
			pods:           []corev1.Pod{emptyDirPod},
			skipDaemonSets: true,
			deleteEmptyDir: true,
			wantCount:      1,
			wantNames:      []string{"emptydir"},
		},
		{
			name:           "emptydir pod included with force",
			pods:           []corev1.Pod{emptyDirPod},
			skipDaemonSets: true,
			force:          true,
			wantCount:      1,
			wantNames:      []string{"emptydir"},
		},
		{
			name:           "normal RS-owned pod always included",
			pods:           []corev1.Pod{normalPod},
			skipDaemonSets: true,
			wantCount:      1,
			wantNames:      []string{"normal"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterEvictable(tc.pods, tc.skipDaemonSets, tc.force, tc.deleteEmptyDir)
			if len(got) != tc.wantCount {
				t.Errorf("got %d pods, want %d", len(got), tc.wantCount)
			}
			for _, wantName := range tc.wantNames {
				found := false
				for _, p := range got {
					if p.Name == wantName {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected pod %q in result", wantName)
				}
			}
		})
	}
}

func TestBlockedEvictionPods(t *testing.T) {
	standalonePod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	jobPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "job-pod",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{ownerRef("Job", "batch")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	emptyDirPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "emptydir",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{ownerRef("ReplicaSet", "rs1")},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "tmp",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	tests := []struct {
		name           string
		pods           []corev1.Pod
		force          bool
		deleteEmptyDir bool
		wantCount      int
		wantReason     string
	}{
		{
			name:       "standalone pod requires force",
			pods:       []corev1.Pod{standalonePod},
			wantCount:  1,
			wantReason: "standalone pods require --force",
		},
		{
			name:       "Job pod requires force",
			pods:       []corev1.Pod{jobPod},
			wantCount:  1,
			wantReason: "Job-owned pods require --force",
		},
		{
			name:       "emptyDir pod requires explicit data deletion",
			pods:       []corev1.Pod{emptyDirPod},
			wantCount:  1,
			wantReason: "emptyDir pods require --delete-emptydir-data or --force",
		},
		{
			name:      "force allows unmanaged pods",
			pods:      []corev1.Pod{standalonePod, jobPod, emptyDirPod},
			force:     true,
			wantCount: 0,
		},
		{
			name:           "delete-emptydir-data allows ReplicaSet emptyDir pod",
			pods:           []corev1.Pod{emptyDirPod},
			deleteEmptyDir: true,
			wantCount:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := blockedEvictionPods(tc.pods, true, tc.force, tc.deleteEmptyDir)
			if len(got) != tc.wantCount {
				t.Fatalf("got %d blocked pods, want %d", len(got), tc.wantCount)
			}
			if tc.wantReason != "" && got[0].reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got[0].reason, tc.wantReason)
			}
		})
	}
}

func TestEvictRemaining_DryRunWarnsButDoesNotFailOnBlockedPods(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone",
			Namespace: "default",
		},
		Spec:   corev1.PodSpec{NodeName: "node1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fakeCS := fake.NewClientset(&pod)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.DryRun = true
	})

	if err := d.evictRemaining(t.Context()); err != nil {
		t.Fatalf("dry-run should not fail on blocked remaining pods: %v", err)
	}
}

// --------------------------------------------------------------------------
// buildEviction
// --------------------------------------------------------------------------

func TestBuildEviction_NoGracePeriod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns1"}}
	ev := buildEviction(pod, -1)
	if ev.Name != "p1" || ev.Namespace != "ns1" {
		t.Errorf("unexpected metadata: %v/%v", ev.Namespace, ev.Name)
	}
	if ev.DeleteOptions != nil {
		t.Errorf("expected nil DeleteOptions for grace period -1, got %v", ev.DeleteOptions)
	}
}

func TestBuildEviction_WithGracePeriod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns1"}}
	ev := buildEviction(pod, 30)
	if ev.DeleteOptions == nil || *ev.DeleteOptions.GracePeriodSeconds != 30 {
		t.Errorf("expected grace period 30, got %v", ev.DeleteOptions)
	}
}

// --------------------------------------------------------------------------
// Drainer.cordon
// --------------------------------------------------------------------------

func TestDrainer_Cordon_AlreadyCordoned(t *testing.T) {
	node := readyNode("node1")
	node.Spec.Unschedulable = true

	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS)

	if _, err := d.cordon(context.Background(), &node); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Node should remain unschedulable (no change made).
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node should still be unschedulable")
	}
}

func TestDrainer_Cordon_Patches(t *testing.T) {
	node := readyNode("node1")
	// node is schedulable (Unschedulable=false by default)

	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS)

	if _, err := d.cordon(context.Background(), &node); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node should be unschedulable after cordon")
	}
}

func TestDrainer_Cordon_DryRun(t *testing.T) {
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) { o.DryRun = true })

	if _, err := d.cordon(context.Background(), &node); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Error("dry-run must not patch the node")
	}
}

// --------------------------------------------------------------------------
// Drainer.Run — high-level scenarios
// --------------------------------------------------------------------------

func TestDrainer_Run_NodeNotFound(t *testing.T) {
	fakeCS := fake.NewClientset() // no node
	d := newTestDrainer(t, "node1", fakeCS)

	if err := d.Run(context.Background()); err == nil {
		t.Fatal("expected error when node doesn't exist, got nil")
	}
}

func TestDrainer_Run_DryRun(t *testing.T) {
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) { o.DryRun = true })

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Node must NOT have been patched in dry-run.
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Error("dry-run must not cordon the node")
	}
}

func TestDrainer_Run_NoWorkloads(t *testing.T) {
	// Only a node, no pods — should cordon then exit cleanly.
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS)

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node should be cordoned after successful run")
	}
}

// --------------------------------------------------------------------------
// runWorkloads — concurrency modes
// --------------------------------------------------------------------------

func TestDrainer_Run_MaxConcurrency_Parallel(t *testing.T) {
	// Two nodes' worth of workloads run with MaxConcurrency=0 (all at once).
	// With a fake client and no pods the full run should complete without error
	// and the node should be cordoned.
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.MaxConcurrency = 0 // unlimited
	})

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := fakeCS.CoreV1().Nodes().Get(context.Background(), "node1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node should be cordoned")
	}
}

func TestDrainer_Run_MaxConcurrency_Batch(t *testing.T) {
	// MaxConcurrency=2 with no workloads — exercises the batch path without
	// needing real rollout infrastructure.
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.MaxConcurrency = 2
	})

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// waitForDeploymentRollout — condition logic
// --------------------------------------------------------------------------

func TestWaitForDeploymentRollout_ImmediateComplete(t *testing.T) {
	// Deployment already in fully-rolled-out state: the first poll should succeed.
	replicas := int32(2)
	dep := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "api",
			Generation: 2,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			Replicas:            2,
			UpdatedReplicas:     2,
			ReadyReplicas:       2,
			AvailableReplicas:   2,
			UnavailableReplicas: 0,
		},
	}

	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// preGeneration=1 so ObservedGeneration(2) > 1 gate passes immediately.
	if err := d.waitForDeploymentRollout(ctx, "default", "api", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForDeploymentRollout_ProgressDeadlineExceeded(t *testing.T) {
	replicas := int32(2)
	dep := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "api",
			Generation: 2,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionFalse,
					Reason: deploymentProgressDeadlineExceeded,
				},
			},
		},
	}

	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := d.waitForDeploymentRollout(ctx, "default", "api", 1)
	if err == nil {
		t.Fatal("expected error for ProgressDeadlineExceeded, got nil")
	}
}

// --------------------------------------------------------------------------
// waitForStatefulSetRollout — condition logic
// --------------------------------------------------------------------------

func TestWaitForStatefulSetRollout_ImmediateComplete(t *testing.T) {
	replicas := int32(3)
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "db",
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdateRevision:     "db-abc123",
			CurrentRevision:    "db-abc123",
			UpdatedReplicas:    3,
			CurrentReplicas:    3, // set by controller once all pods are at the new revision
			ReadyReplicas:      3,
		},
	}

	fakeCS := fake.NewClientset(&sts)
	d := newTestDrainer(t, "node1", fakeCS)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := d.waitForStatefulSetRollout(ctx, "default", "db", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForStatefulSetRollout_EmptyRevisionNotComplete(t *testing.T) {
	// Empty UpdateRevision must not be treated as complete even if both fields match.
	replicas := int32(1)
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "db",
			Generation: 2,
		},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdateRevision:     "", // not yet set by controller
			CurrentRevision:    "",
			UpdatedReplicas:    1,
			ReadyReplicas:      1,
		},
	}

	fakeCS := fake.NewClientset(&sts)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.RolloutTimeout = 50 * time.Millisecond
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Should time out rather than falsely report completion.
	if err := d.waitForStatefulSetRollout(ctx, "default", "db", 1); err == nil {
		t.Fatal("expected timeout error for empty revision, got nil")
	}
}

// --------------------------------------------------------------------------
// filterWorkloads
// --------------------------------------------------------------------------

func makeWorkload(kind workload.Kind, ns, name string) workload.Workload {
	return workload.Workload{
		Kind:      kind,
		Namespace: ns,
		Name:      name,
		Selector:  &metav1.LabelSelector{},
	}
}

func TestFilterWorkloads_NoFilter_ReturnsAll(t *testing.T) {
	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS)

	wls := []workload.Workload{
		makeWorkload(workload.KindDeployment, "default", "api"),
		makeWorkload(workload.KindStatefulSet, "default", "db"),
	}
	got := d.filterWorkloads(wls)
	if len(got) != 2 {
		t.Errorf("expected 2 workloads with no filter, got %d", len(got))
	}
}

func TestFilterWorkloads_SkipWorkload_Removes(t *testing.T) {
	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.SkipWorkloads = map[string]bool{"Deployment/default/api": true}
	})

	wls := []workload.Workload{
		makeWorkload(workload.KindDeployment, "default", "api"),
		makeWorkload(workload.KindStatefulSet, "default", "db"),
	}
	got := d.filterWorkloads(wls)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload after skip, got %d", len(got))
	}
	if got[0].Name != "db" {
		t.Errorf("expected remaining workload to be 'db', got %q", got[0].Name)
	}
}

func TestFilterWorkloads_OnlyWorkload_KeepsOnlyNamed(t *testing.T) {
	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.OnlyWorkloads = map[string]bool{"StatefulSet/default/db": true}
	})

	wls := []workload.Workload{
		makeWorkload(workload.KindDeployment, "default", "api"),
		makeWorkload(workload.KindStatefulSet, "default", "db"),
		makeWorkload(workload.KindDeployment, "default", "worker"),
	}
	got := d.filterWorkloads(wls)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload after only filter, got %d", len(got))
	}
	if got[0].Name != "db" {
		t.Errorf("expected 'db', got %q", got[0].Name)
	}
}

func TestFilterWorkloads_SkipAll_ReturnsEmpty(t *testing.T) {
	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.SkipWorkloads = map[string]bool{
			"Deployment/default/api": true,
			"StatefulSet/default/db": true,
		}
	})

	wls := []workload.Workload{
		makeWorkload(workload.KindDeployment, "default", "api"),
		makeWorkload(workload.KindStatefulSet, "default", "db"),
	}
	got := d.filterWorkloads(wls)
	if len(got) != 0 {
		t.Errorf("expected 0 workloads after skipping all, got %d", len(got))
	}
}

// --------------------------------------------------------------------------
// badWaitingReason
// --------------------------------------------------------------------------

func TestBadWaitingReason_CrashLoopBackOff_WithTermination(t *testing.T) {
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
		},
	}
	if got := badWaitingReason(cs); got != "CrashLoopBackOff" {
		t.Errorf("got %q, want CrashLoopBackOff", got)
	}
}

func TestBadWaitingReason_CrashLoopBackOff_WithoutTermination_Ignored(t *testing.T) {
	// First crash: LastTerminationState is not yet set — should not fail fast.
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
		// LastTerminationState is zero value (no Terminated set)
	}
	if got := badWaitingReason(cs); got != "" {
		t.Errorf("expected empty reason before first termination, got %q", got)
	}
}

func TestBadWaitingReason_ImagePullBackOff(t *testing.T) {
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
		},
	}
	if got := badWaitingReason(cs); got != "ImagePullBackOff" {
		t.Errorf("got %q, want ImagePullBackOff", got)
	}
}

func TestBadWaitingReason_ErrImagePull(t *testing.T) {
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
		},
	}
	if got := badWaitingReason(cs); got != "ErrImagePull" {
		t.Errorf("got %q, want ErrImagePull", got)
	}
}

func TestBadWaitingReason_ContainerCreating_Ignored(t *testing.T) {
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
		},
	}
	if got := badWaitingReason(cs); got != "" {
		t.Errorf("ContainerCreating should not be a bad state, got %q", got)
	}
}

func TestBadWaitingReason_NotWaiting_Ignored(t *testing.T) {
	cs := corev1.ContainerStatus{
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{},
		},
	}
	if got := badWaitingReason(cs); got != "" {
		t.Errorf("running container should not produce a reason, got %q", got)
	}
}
