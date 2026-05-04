package framework

const (
	// DefaultClusterName is the k3d cluster name used for local e2e runs.
	// Override with SAFED_E2E_CLUSTER_NAME for CI isolation.
	DefaultClusterName = "safed-e2e"
	// DefaultFlannelBackend avoids VXLAN flakiness in Docker-backed CI
	// networks while keeping the default k3s CNI.
	DefaultFlannelBackend = "host-gw"
	// E2ENamespace is the namespace where test workloads are deployed.
	E2ENamespace = "e2e"
)
