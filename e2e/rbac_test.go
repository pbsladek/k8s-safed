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

func restrictedReadOnlyDrainKubeconfig(t *testing.T, ctx context.Context) string {
	t.Helper()

	const name = "safed-e2e-readonly-drain"
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
		Rules: []rbacv1.PolicyRule{
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
		},
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
