package drain

import (
	"fmt"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// filterWorkloads applies SkipWorkloads / OnlyWorkloads filtering and logs
// each exclusion. It is a no-op when both maps are empty.
func (d *Drainer) filterWorkloads(state *runState, workloads []workload.Workload) []workload.Workload {
	state.protectedWorkloads = nil
	if len(d.opts.SkipWorkloads) == 0 && len(d.opts.OnlyWorkloads) == 0 {
		return workloads
	}
	out := d.opts.Out
	filtered := workloads[:0:0] // zero-length, same backing array avoided
	for _, w := range workloads {
		key := fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
		if len(d.opts.OnlyWorkloads) > 0 && !d.opts.OnlyWorkloads[key] {
			out.Infof(d.opts.NodeName, "Skipping %s (not in --only-workload list)", wSubject(w))
			state.protectedWorkloads = append(state.protectedWorkloads, w)
			continue
		}
		if d.opts.SkipWorkloads[key] {
			out.Infof(d.opts.NodeName, "Skipping %s (--skip-workload)", wSubject(w))
			state.protectedWorkloads = append(state.protectedWorkloads, w)
			continue
		}
		filtered = append(filtered, w)
	}
	return filtered
}

func (d *Drainer) warnInvalidPriorityAnnotations(workloads []workload.Workload) {
	for _, w := range workloads {
		if !w.PriorityAnnotationInvalid {
			continue
		}
		d.opts.Out.Warnf(wSubject(w), "invalid %s=%q; using default priority %d",
			workload.DrainPriorityAnnotation, w.PriorityAnnotationValue, workload.DefaultDrainPriority)
	}
}
