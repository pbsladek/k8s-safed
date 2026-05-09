package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// cordon marks the node unschedulable via a strategic-merge patch so new pods
// are not scheduled onto it. Using Patch (not Update) is idempotent and avoids
// resourceVersion conflicts with concurrent controllers.
//
// Returns (true, nil) when this call performed the cordon, (false, nil) when
// the node was already cordoned or when running in dry-run mode. The boolean
// is used by Run to decide whether to schedule an uncordon on failure.
func (d *Drainer) cordon(ctx context.Context, node *corev1.Node) (cordonedByUs bool, err error) {
	out := d.opts.Out
	if node.Spec.Unschedulable {
		out.Info(d.opts.NodeName, "Already cordoned")
		if d.opts.UncordonOnFailure {
			out.Info(d.opts.NodeName, "NOTE: --uncordon-on-failure has no effect (node was already cordoned before this drain)")
		}
		return false, nil
	}

	if d.opts.DryRun {
		out.DryRunf(d.opts.NodeName, "Would cordon %q", d.opts.NodeName)
		return false, nil
	}

	out.Infof(d.opts.NodeName, "Cordoning %q...", d.opts.NodeName)
	patch, err := buildNodeUnschedulablePatch(true)
	if err != nil {
		return false, err
	}
	_, err = d.nodes().Patch(
		ctx, d.opts.NodeName,
		types.StrategicMergePatchType, patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		return false, fmt.Errorf("cordoning node %q: %w", d.opts.NodeName, err)
	}
	out.Donef(d.opts.NodeName, "Cordoned %q", d.opts.NodeName)
	return true, nil
}

// uncordon marks the node schedulable again. It uses a fresh context so it
// still runs even when the drain context has already been cancelled (e.g. on
// --timeout expiry). Best-effort: errors are logged but not propagated.
func (d *Drainer) uncordon(out *Printer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	patch, err := buildNodeUnschedulablePatch(false)
	if err != nil {
		out.Infof(d.opts.NodeName, "WARNING: failed to build uncordon patch for %q: %v", d.opts.NodeName, err)
		return
	}
	_, err = d.nodes().Patch(
		ctx, d.opts.NodeName,
		types.StrategicMergePatchType, patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		out.Infof(d.opts.NodeName, "WARNING: failed to uncordon %q after drain failure: %v", d.opts.NodeName, err)
		return
	}
	out.Donef(d.opts.NodeName, "Uncordoned %q (drain failed, --uncordon-on-failure is set)", d.opts.NodeName)
}

func buildNodeUnschedulablePatch(unschedulable bool) ([]byte, error) {
	patch := struct {
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
	}{}
	patch.Spec.Unschedulable = unschedulable

	data, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("building node unschedulable patch: %w", err)
	}
	return data, nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
