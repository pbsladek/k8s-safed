package drain

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// --------------------------------------------------------------------------
// matchesStatefulPattern
// --------------------------------------------------------------------------

func TestMatchesStatefulPattern(t *testing.T) {
	tests := []struct {
		name        string
		wantPattern string
	}{
		// Direct matches
		{"postgres", "postgres"},
		{"postgresql", "postgres"},
		{"my-postgres-primary", "postgres"},
		{"pgbouncer", "pgbouncer"},
		{"pgpool2", "pgpool"},
		{"patroni-cluster", "patroni"},
		{"mysql-0", "mysql"},
		{"mariadb", "mariadb"},
		{"redis-master", "redis"},
		{"keydb", "keydb"},
		{"mongodb", "mongo"},
		{"elasticsearch", "elasticsearch"},
		{"opensearch-node", "opensearch"},
		{"kafka-broker", "kafka"},
		{"zookeeper", "zookeeper"},
		{"rabbitmq", "rabbitmq"},
		{"nats-server", "nats"},
		{"etcd", "etcd"},
		{"cassandra", "cassandra"},
		{"minio", "minio"},
		{"vault", "vault"},
		{"memcached", "memcached"},
		// Case-insensitive
		{"POSTGRES-primary", "postgres"},
		{"Redis-Cache", "redis"},
		// No match
		{"api-server", ""},
		{"frontend", ""},
		{"nginx", ""},
		{"worker", ""},
	}
	for _, tc := range tests {
		got := matchesStatefulPattern(tc.name)
		if got != tc.wantPattern {
			t.Errorf("matchesStatefulPattern(%q) = %q, want %q", tc.name, got, tc.wantPattern)
		}
	}
}

// --------------------------------------------------------------------------
// preflightDeployment
// --------------------------------------------------------------------------

func makeDeploymentForPreflight(ns, name string, replicas int32, strategy appsv1.DeploymentStrategyType) appsv1.Deployment {
	dep := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: strategy},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
	return dep
}

func TestPreflightDeployment_SingleReplica_IsRisk(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 1, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
	issues, err := d.preflightDeployment(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("expected risk issue for single-replica deployment, got none")
	}
	if !issues[0].risk {
		t.Error("expected issue to be risk=true")
	}
}

func TestPreflightDeployment_MultiReplica_NoRisk(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 3, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
	issues, err := d.preflightDeployment(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, issue := range issues {
		if issue.risk {
			t.Errorf("unexpected risk issue for multi-replica deployment: %s", issue.message)
		}
	}
}

func TestPreflightDeployment_RecreateStrategy_IsRisk(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 3, appsv1.RecreateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
	issues, err := d.preflightDeployment(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hasRecreateRisk := false
	for _, issue := range issues {
		if issue.risk {
			hasRecreateRisk = true
		}
	}
	if !hasRecreateRisk {
		t.Error("expected risk issue for Recreate strategy")
	}
}

func TestPreflightDeployment_SingleReplicaAndRecreate_TwoRisks(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 1, appsv1.RecreateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
	issues, err := d.preflightDeployment(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	riskCount := 0
	for _, i := range issues {
		if i.risk {
			riskCount++
		}
	}
	if riskCount != 2 {
		t.Errorf("expected 2 risk issues, got %d", riskCount)
	}
}

// --------------------------------------------------------------------------
// preflightStatefulSet
// --------------------------------------------------------------------------

func makeStatefulSetForPreflight(ns, name string, replicas int32) appsv1.StatefulSet {
	return appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
}

func TestPreflightStatefulSet_SingleReplica_IsRisk(t *testing.T) {
	sts := makeStatefulSetForPreflight("default", "db", 1)
	fakeCS := fake.NewClientset(&sts)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindStatefulSet, Namespace: "default", Name: "db"}
	issues, err := d.preflightStatefulSet(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) == 0 || !issues[0].risk {
		t.Error("expected risk=true for single-replica StatefulSet")
	}
}

func TestPreflightStatefulSet_MultiReplica_IsNote(t *testing.T) {
	sts := makeStatefulSetForPreflight("default", "db", 3)
	fakeCS := fake.NewClientset(&sts)
	d := newTestDrainer(t, "node1", fakeCS)

	w := workload.Workload{Kind: workload.KindStatefulSet, Namespace: "default", Name: "db"}
	issues, err := d.preflightStatefulSet(context.Background(), w, wSubject(w))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 informational issue, got %d", len(issues))
	}
	if issues[0].risk {
		t.Error("multi-replica StatefulSet issue should be risk=false")
	}
}

// --------------------------------------------------------------------------
// runPreflight — mode behaviour
// --------------------------------------------------------------------------

func TestRunPreflight_WarnMode_ReturnsNilOnRisk(t *testing.T) {
	// Single-replica Deployment is a risk but warn mode should not abort.
	dep := makeDeploymentForPreflight("default", "api", 1, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Preflight = PreflightModeWarn
	})

	wls := []workload.Workload{
		{Kind: workload.KindDeployment, Namespace: "default", Name: "api",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	}
	if err := d.runPreflight(context.Background(), wls); err != nil {
		t.Errorf("warn mode must not return error, got: %v", err)
	}
}

func TestRunPreflight_StrictMode_ReturnsErrorOnRisk(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 1, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Preflight = PreflightModeStrict
	})

	wls := []workload.Workload{
		{Kind: workload.KindDeployment, Namespace: "default", Name: "api",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	}
	if err := d.runPreflight(context.Background(), wls); err == nil {
		t.Error("strict mode must return error when risk is found")
	}
}

func TestRunPreflight_StrictMode_NoRisk_ReturnsNil(t *testing.T) {
	dep := makeDeploymentForPreflight("default", "api", 3, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Preflight = PreflightModeStrict
	})

	wls := []workload.Workload{
		{Kind: workload.KindDeployment, Namespace: "default", Name: "api",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	}
	if err := d.runPreflight(context.Background(), wls); err != nil {
		t.Errorf("strict mode with no risks must return nil, got: %v", err)
	}
}

func TestRunPreflight_EmptyWorkloads_NoOp(t *testing.T) {
	fakeCS := fake.NewClientset()
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Preflight = PreflightModeStrict
	})
	// Empty workload list must never return an error.
	if err := d.runPreflight(context.Background(), nil); err != nil {
		t.Errorf("unexpected error for empty workloads: %v", err)
	}
}

func TestRunPreflight_StatefulNameDetected(t *testing.T) {
	// A multi-replica Deployment named "postgres-api" should not be a risk but
	// should trigger the stateful service note in warn mode (no error).
	dep := makeDeploymentForPreflight("default", "postgres-api", 3, appsv1.RollingUpdateDeploymentStrategyType)
	fakeCS := fake.NewClientset(&dep)
	d := newTestDrainer(t, "node1", fakeCS, func(o *Options) {
		o.Preflight = PreflightModeStrict // strict: only errors on risk, not notes
	})

	wls := []workload.Workload{
		{Kind: workload.KindDeployment, Namespace: "default", Name: "postgres-api",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres-api"}}},
	}
	// Should not error — stateful name is a note (risk=false), not a risk.
	if err := d.runPreflight(context.Background(), wls); err != nil {
		t.Errorf("stateful name note must not cause strict abort, got: %v", err)
	}
}
