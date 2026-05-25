package drain

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type nodeAPI interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.Node, error)
	Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*corev1.Node, error)
}

type podAPI interface {
	List(context.Context, metav1.ListOptions) (*corev1.PodList, error)
	Get(context.Context, string, metav1.GetOptions) (*corev1.Pod, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
	EvictV1(context.Context, *policyv1.Eviction) error
}

func (d *Drainer) nodes() nodeAPI {
	return d.client.CoreV1().Nodes()
}

func (d *Drainer) pods(namespace string) podAPI {
	return d.client.CoreV1().Pods(namespace)
}
