package drain

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

func FuzzFilterWorkloads(f *testing.F) {
	f.Add("Deployment/default/api", "Deployment/default/api", "")
	f.Add("Deployment/default/api", "", "StatefulSet/default/db")
	f.Add("StatefulSet/data/postgres", "Deployment/default/api", "StatefulSet/data/postgres")

	f.Fuzz(func(t *testing.T, workloadKey, skipKey, onlyKey string) {
		w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
		d := newTestDrainer(t, "node1", fake.NewClientset(), func(o *Options) {
			if skipKey != "" {
				o.SkipWorkloads = map[string]bool{skipKey: true}
			}
			if onlyKey != "" {
				o.OnlyWorkloads = map[string]bool{onlyKey: true}
			}
		})
		if workloadKey == "StatefulSet/data/postgres" {
			w = workload.Workload{Kind: workload.KindStatefulSet, Namespace: "data", Name: "postgres"}
		}
		state := &runState{}
		got := d.filterWorkloads(state, []workload.Workload{w})
		if len(got) > 1 {
			t.Fatalf("filter returned too many workloads: %#v", got)
		}
		if len(got) == 0 && len(state.protectedWorkloads) != 1 {
			t.Fatalf("filtered workload was not protected: got=%#v protected=%#v", got, state.protectedWorkloads)
		}
	})
}
