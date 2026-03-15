package k8s

import (
	"fmt"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
)

// Client wraps the Kubernetes clientset.
type Client struct {
	Kubernetes kubernetes.Interface
}

// NewClient builds a Client from the provided kubeconfig flags.
func NewClient(flags *genericclioptions.ConfigFlags) (*Client, error) {
	restConfig, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build REST config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return &Client{Kubernetes: clientset}, nil
}
