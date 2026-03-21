package workload_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// --------------------------------------------------------------------------
// IsTerminalPod
// --------------------------------------------------------------------------

func TestIsTerminalPod(t *testing.T) {
	tests := []struct {
		phase corev1.PodPhase
		want  bool
	}{
		{corev1.PodRunning, false},
		{corev1.PodPending, false},
		{corev1.PodSucceeded, true},
		{corev1.PodFailed, true},
		{"", false},
	}
	for _, tc := range tests {
		pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tc.phase}}
		if got := workload.IsTerminalPod(pod); got != tc.want {
			t.Errorf("phase %q: got %v, want %v", tc.phase, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// Workload.String
// --------------------------------------------------------------------------

func TestWorkload_String(t *testing.T) {
	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "prod", Name: "api"}
	if got := w.String(); got != "Deployment prod/api" {
		t.Errorf("unexpected String(): %q", got)
	}
}

// --------------------------------------------------------------------------
// FindForNode helpers
// --------------------------------------------------------------------------

// makePod returns a Running pod in ns with the given ownerReferences.
func makePod(ns, name string, owners []metav1.OwnerReference) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			OwnerReferences: owners,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func ownerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name}
}

func makeRS(ns, name, depName string) appsv1.ReplicaSet {
	rs := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if depName != "" {
		rs.OwnerReferences = []metav1.OwnerReference{ownerRef("Deployment", depName)}
	}
	return rs
}

func makeDeployment(ns, name string) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
	}
}

func makeStatefulSet(ns, name string) appsv1.StatefulSet {
	return appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
	}
}

// --------------------------------------------------------------------------
// FindForNode tests
// --------------------------------------------------------------------------

func TestFinder_FindForNode_Empty(t *testing.T) {
	client := fake.NewSimpleClientset()
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads, got %d", len(wls))
	}
}

func TestFinder_FindForNode_Deployment(t *testing.T) {
	dep := makeDeployment("default", "api")
	rs := makeRS("default", "api-rs1", "api")
	pod := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	client := fake.NewSimpleClientset(&dep, &rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(wls))
	}
	if wls[0].Kind != workload.KindDeployment {
		t.Errorf("expected Deployment, got %s", wls[0].Kind)
	}
	if wls[0].Name != "api" {
		t.Errorf("expected name 'api', got %s", wls[0].Name)
	}
	if wls[0].Selector == nil {
		t.Error("expected Selector to be set")
	}
}

func TestFinder_FindForNode_StatefulSet(t *testing.T) {
	sts := makeStatefulSet("default", "db")
	pod := makePod("default", "db-0", []metav1.OwnerReference{ownerRef("StatefulSet", "db")})

	client := fake.NewSimpleClientset(&sts, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(wls))
	}
	if wls[0].Kind != workload.KindStatefulSet {
		t.Errorf("expected StatefulSet, got %s", wls[0].Kind)
	}
	if wls[0].Name != "db" {
		t.Errorf("expected name 'db', got %s", wls[0].Name)
	}
}

func TestFinder_FindForNode_SkipsDaemonSet(t *testing.T) {
	pod := makePod("default", "ds-pod", []metav1.OwnerReference{ownerRef("DaemonSet", "node-agent")})

	client := fake.NewSimpleClientset(&pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads (DaemonSet pods skipped), got %d", len(wls))
	}
}

func TestFinder_FindForNode_SkipsJob(t *testing.T) {
	pod := makePod("default", "job-pod", []metav1.OwnerReference{ownerRef("Job", "batch-job")})

	client := fake.NewSimpleClientset(&pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads (Job pods skipped), got %d", len(wls))
	}
}

func TestFinder_FindForNode_SkipsStandalone(t *testing.T) {
	pod := makePod("default", "standalone-pod", nil)

	client := fake.NewSimpleClientset(&pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads (standalone pods skipped), got %d", len(wls))
	}
}

func TestFinder_FindForNode_SkipsTerminalPod(t *testing.T) {
	dep := makeDeployment("default", "api")
	rs := makeRS("default", "api-rs1", "api")
	pod := makePod("default", "api-pod-done", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})
	pod.Status.Phase = corev1.PodSucceeded

	client := fake.NewSimpleClientset(&dep, &rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads (terminal pods skipped), got %d", len(wls))
	}
}

func TestFinder_FindForNode_Deduplication(t *testing.T) {
	// Two pods from the same Deployment should yield a single Workload entry.
	dep := makeDeployment("default", "api")
	rs := makeRS("default", "api-rs1", "api")
	pod1 := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})
	pod2 := makePod("default", "api-pod2", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	client := fake.NewSimpleClientset(&dep, &rs, &pod1, &pod2)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Errorf("expected 1 deduplicated workload, got %d", len(wls))
	}
}

func TestFinder_FindForNode_StandaloneRS(t *testing.T) {
	// A ReplicaSet with no Deployment owner should be skipped.
	rs := makeRS("default", "standalone-rs", "") // no Deployment owner
	pod := makePod("default", "rs-pod", []metav1.OwnerReference{ownerRef("ReplicaSet", "standalone-rs")})

	client := fake.NewSimpleClientset(&rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 0 {
		t.Errorf("expected 0 workloads (standalone RS skipped), got %d", len(wls))
	}
}

func TestFinder_FindForNode_MultipleWorkloads(t *testing.T) {
	dep := makeDeployment("default", "api")
	rs := makeRS("default", "api-rs1", "api")
	depPod := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	sts := makeStatefulSet("default", "db")
	stsPod := makePod("default", "db-0", []metav1.OwnerReference{ownerRef("StatefulSet", "db")})

	client := fake.NewSimpleClientset(&dep, &rs, &depPod, &sts, &stsPod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 2 {
		t.Errorf("expected 2 workloads, got %d", len(wls))
	}

	kinds := map[workload.Kind]bool{}
	for _, w := range wls {
		kinds[w.Kind] = true
	}
	if !kinds[workload.KindDeployment] {
		t.Error("expected a Deployment workload")
	}
	if !kinds[workload.KindStatefulSet] {
		t.Error("expected a StatefulSet workload")
	}
}

// --------------------------------------------------------------------------
// parseDrainPriority (via FindForNode — priority flows from annotation)
// --------------------------------------------------------------------------

func makeDeploymentWithAnnotation(ns, name, annotationKey, annotationValue string) appsv1.Deployment {
	dep := makeDeployment(ns, name)
	dep.Annotations = map[string]string{annotationKey: annotationValue}
	return dep
}

func TestFinder_FindForNode_Priority_Default(t *testing.T) {
	// A Deployment without the annotation should get DefaultDrainPriority.
	dep := makeDeployment("default", "api")
	rs := makeRS("default", "api-rs1", "api")
	pod := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	client := fake.NewSimpleClientset(&dep, &rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(wls))
	}
	if wls[0].Priority != workload.DefaultDrainPriority {
		t.Errorf("Priority = %d, want DefaultDrainPriority (%d)", wls[0].Priority, workload.DefaultDrainPriority)
	}
}

func TestFinder_FindForNode_Priority_FromAnnotation(t *testing.T) {
	dep := makeDeploymentWithAnnotation("default", "api", workload.DrainPriorityAnnotation, "10")
	rs := makeRS("default", "api-rs1", "api")
	pod := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	client := fake.NewSimpleClientset(&dep, &rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(wls))
	}
	if wls[0].Priority != 10 {
		t.Errorf("Priority = %d, want 10", wls[0].Priority)
	}
}

func TestFinder_FindForNode_Priority_InvalidAnnotation_UsesDefault(t *testing.T) {
	dep := makeDeploymentWithAnnotation("default", "api", workload.DrainPriorityAnnotation, "not-a-number")
	rs := makeRS("default", "api-rs1", "api")
	pod := makePod("default", "api-pod1", []metav1.OwnerReference{ownerRef("ReplicaSet", "api-rs1")})

	client := fake.NewSimpleClientset(&dep, &rs, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if wls[0].Priority != workload.DefaultDrainPriority {
		t.Errorf("invalid annotation should fall back to DefaultDrainPriority, got %d", wls[0].Priority)
	}
}

func TestFinder_FindForNode_StatefulSet_Priority(t *testing.T) {
	sts := makeStatefulSet("default", "db")
	sts.Annotations = map[string]string{workload.DrainPriorityAnnotation: "200"}
	pod := makePod("default", "db-0", []metav1.OwnerReference{ownerRef("StatefulSet", "db")})

	client := fake.NewSimpleClientset(&sts, &pod)
	f := workload.NewFinder(client)

	wls, err := f.FindForNode(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(wls))
	}
	if wls[0].Priority != 200 {
		t.Errorf("Priority = %d, want 200", wls[0].Priority)
	}
}
