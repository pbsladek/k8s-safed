//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/pbsladek/k8s-safed/e2e/framework"
)

// --------------------------------------------------------------------------
// TestDrain_RBACMissingNodePatchFailsBeforeMutation
// --------------------------------------------------------------------------

func TestDrain_RBACMissingNodePatchFailsBeforeMutation(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	kubeconfigPath := restrictedReadOnlyDrainKubeconfig(t, ctx)
	restricted := &framework.Binary{
		Path:           testBinary.Path,
		KubeconfigPath: kubeconfigPath,
	}

	result := restricted.Drain(ctx, target,
		"--preflight", "off",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatalf("restricted drain should fail without nodes/patch\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "forbidden") ||
		(!strings.Contains(combined, "patch") && !strings.Contains(combined, "cordoning node")) {
		t.Fatalf("restricted drain failed for unexpected reason\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_RBACMissingDeploymentPatchUncordonsAfterFailure
// --------------------------------------------------------------------------

func TestDrain_RBACMissingDeploymentPatchUncordonsAfterFailure(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	defer uncordon(t, target)

	manifest := simpleDeploymentManifest("rbac-no-deployment-patch", 50)
	defer func() {
		_ = framework.DeleteManifest(context.Background(), testCluster.KubeconfigPath, manifest)
	}()
	deployDeploymentsOnNode(t, ctx, target, manifest, "rbac-no-deployment-patch")

	kubeconfigPath := restrictedDrainKubeconfig(t, ctx, "no-deployment-patch", []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets", "statefulsets"}, Verbs: []string{"get"}},
	})
	restricted := &framework.Binary{
		Path:           testBinary.Path,
		KubeconfigPath: kubeconfigPath,
	}

	result := restricted.Drain(ctx, target,
		"--preflight", "off",
		"--only-workload", "Deployment/e2e/rbac-no-deployment-patch",
		"--uncordon-on-failure",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatalf("restricted drain should fail without deployments/patch\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "forbidden") || !strings.Contains(combined, "patching Deployment e2e/rbac-no-deployment-patch") {
		t.Fatalf("restricted drain failed for unexpected reason\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_RBACMissingPodEvictionUncordonsAfterFailure
// --------------------------------------------------------------------------

func TestDrain_RBACMissingPodEvictionUncordonsAfterFailure(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	kubeconfigPath := restrictedDrainKubeconfig(t, ctx, "no-pod-eviction", []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list", "get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets", "statefulsets"}, Verbs: []string{"get", "patch"}},
	})
	restricted := &framework.Binary{
		Path:           testBinary.Path,
		KubeconfigPath: kubeconfigPath,
	}

	result := restricted.Drain(ctx, target,
		"--preflight", "off",
		"--only-workload", "Deployment/e2e/does-not-exist",
		"--force",
		"--uncordon-on-failure",
		"--eviction-timeout", "20s",
		"--pdb-retry-interval", "1s",
		"--poll-interval", "1s",
	)
	if result.Err == nil {
		t.Fatalf("restricted drain should fail without pods/eviction create\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	combined := result.Stdout + result.Stderr + result.Err.Error()
	if !strings.Contains(combined, "forbidden") || !strings.Contains(combined, "evicting pod") {
		t.Fatalf("restricted drain failed for unexpected reason\nerr: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
}

// --------------------------------------------------------------------------
// TestDrain_RBACMissingEventCreateIsBestEffort
// --------------------------------------------------------------------------

func TestDrain_RBACMissingEventCreateIsBestEffort(t *testing.T) {
	waitAllReady(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	target := firstAgentNode(t, ctx)
	uncordon(t, target)
	defer uncordon(t, target)

	kubeconfigPath := restrictedDrainKubeconfig(t, ctx, "no-events-create", []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list", "get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets", "statefulsets"}, Verbs: []string{"get"}},
	})
	restricted := &framework.Binary{
		Path:           testBinary.Path,
		KubeconfigPath: kubeconfigPath,
	}

	result := restricted.Drain(ctx, target,
		"--preflight", "off",
		"--dry-run",
		"--emit-events",
		"--only-workload", "Deployment/e2e/does-not-exist",
		"--poll-interval", "1s",
	)
	if result.Err != nil {
		t.Fatalf("event RBAC failure should be best-effort in dry-run: %v\nstdout: %s\nstderr: %s",
			result.Err, result.Stdout, result.Stderr)
	}
	verifyNodeNotCordoned(t, target)

	if !strings.Contains(result.Stdout+result.Stderr, `failed to emit event "Draining"`) {
		t.Fatalf("output missing best-effort event warning\nstdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	}
}

func restrictedReadOnlyDrainKubeconfig(t *testing.T, ctx context.Context) string {
	t.Helper()

	return restrictedDrainKubeconfig(t, ctx, "readonly-drain", []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"list"},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "replicasets", "statefulsets"},
			Verbs:     []string{"get"},
		},
	})
}

func restrictedDrainKubeconfig(t *testing.T, ctx context.Context, suffix string, rules []rbacv1.PolicyRule) string {
	t.Helper()

	name := k8sTestName(t, suffix)
	ns := framework.E2ENamespace
	_, err := testClient.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create restricted service account: %v", err)
	}
	t.Cleanup(func() {
		_ = testClient.RbacV1().ClusterRoleBindings().Delete(context.Background(), name, metav1.DeleteOptions{})
		_ = testClient.RbacV1().ClusterRoles().Delete(context.Background(), name, metav1.DeleteOptions{})
		_ = testClient.CoreV1().ServiceAccounts(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	})

	_, err = testClient.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      rules,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create restricted cluster role: %v", err)
	}

	_, err = testClient.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: ns,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create restricted cluster role binding: %v", err)
	}

	expirationSeconds := int64(10 * 60)
	token, err := testClient.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create restricted service account token: %v", err)
	}

	source, err := clientcmd.LoadFromFile(testCluster.KubeconfigPath)
	if err != nil {
		t.Fatalf("load source kubeconfig: %v", err)
	}
	sourceCtx := source.Contexts[source.CurrentContext]
	if sourceCtx == nil {
		t.Fatalf("source kubeconfig current context %q not found", source.CurrentContext)
	}
	sourceCluster := source.Clusters[sourceCtx.Cluster]
	if sourceCluster == nil {
		t.Fatalf("source kubeconfig cluster %q not found", sourceCtx.Cluster)
	}

	restricted := clientcmdapi.NewConfig()
	restricted.Clusters["restricted"] = sourceCluster.DeepCopy()
	restricted.AuthInfos["restricted"] = &clientcmdapi.AuthInfo{Token: token.Status.Token}
	restricted.Contexts["restricted"] = &clientcmdapi.Context{
		Cluster:   "restricted",
		AuthInfo:  "restricted",
		Namespace: ns,
	}
	restricted.CurrentContext = "restricted"

	path := filepath.Join(t.TempDir(), "restricted-kubeconfig.yaml")
	if err := clientcmd.WriteToFile(*restricted, path); err != nil {
		t.Fatalf("write restricted kubeconfig: %v", err)
	}
	return path
}

func k8sTestName(t *testing.T, suffix string) string {
	t.Helper()

	base := strings.ToLower(t.Name() + "-" + suffix)
	var b strings.Builder
	lastHyphen := false
	for _, r := range base {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 55 {
		name = strings.Trim(name[:55], "-")
	}
	if name == "" {
		return "safed-e2e"
	}
	return name
}
