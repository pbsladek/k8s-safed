package drain

import "github.com/pbsladek/k8s-safed/pkg/workload"

type runState struct {
	protectedWorkloads []workload.Workload
}
