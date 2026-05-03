package drain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// --------------------------------------------------------------------------
// sanitizeFilename
// --------------------------------------------------------------------------

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-cluster", "my-cluster"},
		{"prod/us-east-1", "prod-us-east-1"},
		{"ctx:cluster", "ctx-cluster"},
		{"node 1", "node-1"},
		{"a/b:c d", "a-b-c-d"},
	}
	for _, tc := range tests {
		got := sanitizeFilename(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// CheckpointPath
// --------------------------------------------------------------------------

func TestCheckpointPath(t *testing.T) {
	path, err := CheckpointPath("prod-ctx", "worker-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.Base(path) != "prod-ctx-worker-1.json" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}
	if filepath.Base(filepath.Dir(path)) != "safed-checkpoints" {
		t.Errorf("unexpected parent dir: %s", filepath.Dir(path))
	}
}

func TestCheckpointPath_SanitizesSpecialChars(t *testing.T) {
	path, err := CheckpointPath("ctx/cluster:1", "node/2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	base := filepath.Base(path)
	if base != "ctx-cluster-1-node-2.json" {
		t.Errorf("unexpected filename after sanitization: %s", base)
	}
}

// --------------------------------------------------------------------------
// LoadCheckpoint
// --------------------------------------------------------------------------

func TestLoadCheckpoint_MissingFile_ReturnsEmpty(t *testing.T) {
	cp, err := LoadCheckpoint("/nonexistent/path/checkpoint.json")
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if cp == nil {
		t.Fatal("expected non-nil checkpoint")
	}
	if len(cp.Completed) != 0 {
		t.Errorf("expected empty Completed map, got %d entries", len(cp.Completed))
	}
}

func TestLoadCheckpoint_CorruptFile_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCheckpoint(path)
	if err == nil {
		t.Fatal("expected error for corrupt checkpoint, got nil")
	}
}

// --------------------------------------------------------------------------
// Checkpoint.Save / LoadCheckpoint round-trip
// --------------------------------------------------------------------------

func TestCheckpoint_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")

	cp := &Checkpoint{
		NodeName: "worker-1",
		Context:  "prod",
		Completed: map[string]bool{
			"Deployment/default/api": true,
		},
	}
	if err := cp.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatalf("LoadCheckpoint failed: %v", err)
	}
	if loaded.NodeName != "worker-1" {
		t.Errorf("NodeName: got %q, want %q", loaded.NodeName, "worker-1")
	}
	if !loaded.Completed["Deployment/default/api"] {
		t.Error("expected Deployment/default/api in Completed")
	}
}

func TestCheckpoint_Save_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	// Nested path that doesn't exist yet.
	path := filepath.Join(dir, "sub1", "sub2", "checkpoint.json")

	cp := &Checkpoint{Completed: make(map[string]bool)}
	if err := cp.Save(path); err != nil {
		t.Fatalf("Save should create missing directories: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("checkpoint file not found after Save: %v", err)
	}
}

func TestCheckpoint_Save_Atomic(t *testing.T) {
	// After a successful Save there must be no leftover .tmp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")

	cp := &Checkpoint{Completed: make(map[string]bool)}
	if err := cp.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("leftover .tmp file found after successful Save")
	}
}

// --------------------------------------------------------------------------
// Checkpoint.MarkDone / IsDone
// --------------------------------------------------------------------------

func testWorkload(kind workload.Kind, ns, name string) workload.Workload {
	return workload.Workload{
		Kind:      kind,
		Namespace: ns,
		Name:      name,
		Selector:  &metav1.LabelSelector{},
	}
}

func TestCheckpoint_MarkDone_IsDone(t *testing.T) {
	cp := &Checkpoint{Completed: make(map[string]bool)}
	w := testWorkload(workload.KindDeployment, "default", "api")

	if cp.IsDone(w) {
		t.Error("IsDone should be false before MarkDone")
	}
	cp.MarkDone(w)
	if !cp.IsDone(w) {
		t.Error("IsDone should be true after MarkDone")
	}
}

func TestCheckpoint_IsDone_OtherWorkloadsUnaffected(t *testing.T) {
	cp := &Checkpoint{Completed: make(map[string]bool)}
	w1 := testWorkload(workload.KindDeployment, "default", "api")
	w2 := testWorkload(workload.KindStatefulSet, "default", "db")

	cp.MarkDone(w1)
	if cp.IsDone(w2) {
		t.Error("w2 should not be done after marking w1")
	}
}

func TestCheckpoint_MarkDone_KeyFormat(t *testing.T) {
	// The key format must be "Kind/namespace/name" matching the log subject format.
	cp := &Checkpoint{Completed: make(map[string]bool)}
	w := testWorkload(workload.KindStatefulSet, "data", "postgres")
	cp.MarkDone(w)

	if !cp.Completed["StatefulSet/data/postgres"] {
		t.Error("expected key 'StatefulSet/data/postgres' in Completed map")
	}
}

// --------------------------------------------------------------------------
// DeleteCheckpoint
// --------------------------------------------------------------------------

func TestDeleteCheckpoint_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCheckpoint(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be gone after DeleteCheckpoint")
	}
}

func TestDeleteCheckpoint_MissingFile_NoError(t *testing.T) {
	if err := DeleteCheckpoint("/nonexistent/path/checkpoint.json"); err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Integration: resume skips completed workloads in runWorkloads
// --------------------------------------------------------------------------

func TestRunWorkloads_Resume_SkipsCompletedWorkloads(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint.json")

	// Pre-populate checkpoint: api is already done.
	cp := &Checkpoint{
		NodeName: "node1",
		Completed: map[string]bool{
			"Deployment/default/api": true,
		},
	}
	if err := cp.Save(cpPath); err != nil {
		t.Fatal(err)
	}

	// A drainer with Resume=true — no real cluster needed since all workloads
	// should be skipped before any API calls.
	node := readyNode("node1")
	fakeCS := fake.NewClientset(&node)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Resume = true
		o.CheckpointPath = cpPath
	})

	wls := []workload.Workload{
		testWorkload(workload.KindDeployment, "default", "api"),
	}

	// runWorkloads should return nil (all skipped) without making any API calls.
	if err := d.runWorkloads(t.Context(), wls); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunWorkloads_WritesCheckpointWithoutResume(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint.json")

	dep := readyDeploymentForCheckpoint("default", "api")
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.CheckpointPath = cpPath
		o.CheckpointContext = "prod"
	})

	wls := []workload.Workload{
		{
			Kind:      workload.KindDeployment,
			Namespace: "default",
			Name:      "api",
			Selector:  dep.Spec.Selector,
		},
	}
	if err := d.runWorkloads(t.Context(), wls); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cp, err := LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatalf("LoadCheckpoint failed: %v", err)
	}
	if cp.NodeName != "node1" {
		t.Errorf("NodeName = %q, want node1", cp.NodeName)
	}
	if cp.Context != "prod" {
		t.Errorf("Context = %q, want prod", cp.Context)
	}
	if !cp.Completed["Deployment/default/api"] {
		t.Error("expected completed checkpoint entry for Deployment/default/api")
	}
}

func TestRunWorkloads_DryRunDoesNotWriteCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint.json")

	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.DryRun = true
		o.CheckpointPath = cpPath
	})

	wls := []workload.Workload{
		testWorkload(workload.KindDeployment, "default", "api"),
	}
	if err := d.runWorkloads(t.Context(), wls); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(cpPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not write checkpoint, stat err: %v", err)
	}
}

func TestRunWorkloads_BatchSavesCompletedWorkloadBeforeSiblingFailure(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint.json")

	ready := readyDeploymentForCheckpoint("default", "api")
	stalled := stalledDeploymentForCheckpoint("default", "stalled")
	fakeCS := fake.NewClientset(&ready, &stalled)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.CheckpointPath = cpPath
		o.CheckpointContext = "prod"
		o.MaxConcurrency = 2
		o.RolloutTimeout = 30 * time.Millisecond
		o.PollInterval = time.Millisecond
	})

	wls := []workload.Workload{
		{
			Kind:      workload.KindDeployment,
			Namespace: "default",
			Name:      "api",
			Selector:  ready.Spec.Selector,
		},
		{
			Kind:      workload.KindDeployment,
			Namespace: "default",
			Name:      "stalled",
			Selector:  stalled.Spec.Selector,
		},
	}
	if err := d.runWorkloads(t.Context(), wls); err == nil {
		t.Fatal("expected stalled workload to fail")
	}

	cp, err := LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatalf("LoadCheckpoint failed: %v", err)
	}
	if !cp.Completed["Deployment/default/api"] {
		t.Error("completed sibling was not saved before batch failure")
	}
	if cp.Completed["Deployment/default/stalled"] {
		t.Error("failed workload should not be marked complete")
	}
}

func TestRunWorkloads_ResumeRejectsCheckpointForDifferentNode(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "checkpoint.json")

	cp := &Checkpoint{
		NodeName:  "other-node",
		Context:   "prod",
		Completed: map[string]bool{},
	}
	if err := cp.Save(cpPath); err != nil {
		t.Fatal(err)
	}

	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Resume = true
		o.CheckpointPath = cpPath
		o.CheckpointContext = "prod"
	})

	wls := []workload.Workload{
		testWorkload(workload.KindDeployment, "default", "api"),
	}
	if err := d.runWorkloads(t.Context(), wls); err == nil {
		t.Fatal("expected checkpoint node mismatch error")
	}
}

func readyDeploymentForCheckpoint(ns, name string) appsv1.Deployment {
	replicas := int32(1)
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  ns,
			Name:       name,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  1,
			UpdatedReplicas:     1,
			ReadyReplicas:       1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}
}

func stalledDeploymentForCheckpoint(ns, name string) appsv1.Deployment {
	replicas := int32(1)
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  ns,
			Name:       name,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
		},
	}
}
