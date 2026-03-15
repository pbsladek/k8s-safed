package drain

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// PreflightMode controls how pre-flight issues are handled before the drain
// makes any changes to the cluster.
type PreflightMode string

const (
	// PreflightModeWarn logs all findings and continues. This is the default.
	PreflightModeWarn PreflightMode = "warn"
	// PreflightModeStrict aborts the drain if any risk-level finding is detected.
	PreflightModeStrict PreflightMode = "strict"
	// PreflightModeOff skips all pre-flight checks.
	PreflightModeOff PreflightMode = "off"
)

// preflightIssue is a single finding from the pre-flight scan.
type preflightIssue struct {
	subject string
	message string
	// risk is true when the condition is likely to cause downtime or data loss.
	// Informational findings (e.g. detected stateful service) have risk=false.
	risk bool
}

// knownStatefulPatterns is matched case-insensitively against workload names.
// A match surfaces a note prompting the operator to verify replication health
// before the drain proceeds.
var knownStatefulPatterns = []string{
	// PostgreSQL ecosystem
	"postgres", "postgresql", "pgbouncer", "pgpool", "pgpool2", "patroni",
	// MySQL ecosystem
	"mysql", "mariadb", "percona", "vitess",
	// Redis / KeyDB
	"redis", "keydb",
	// MongoDB
	"mongo", "mongodb",
	// Search
	"elasticsearch", "opensearch", "solr",
	// Kafka / streaming
	"kafka", "zookeeper", "redpanda",
	// Message brokers
	"rabbitmq", "nats",
	// Distributed stores
	"etcd", "cassandra", "scylla", "cockroach", "clickhouse", "yugabyte",
	// Object / secret storage
	"minio", "vault",
	// Caches
	"memcached",
}

// runPreflight checks discovered workloads for conditions that could cause
// downtime or require operator attention before the drain makes any changes.
//
// All issues are logged. When PreflightModeStrict is set, the function returns
// a non-nil error if any risk-level issue was found (aborting the drain before
// the cordon step). Use PreflightModeWarn to log and continue.
func (d *Drainer) runPreflight(ctx context.Context, workloads []workload.Workload) error {
	out := d.opts.Out
	subj := d.opts.NodeName

	if len(workloads) == 0 {
		return nil
	}

	out.Info(subj, "Running pre-flight checks...")

	var issues []preflightIssue

	for _, w := range workloads {
		wsubj := wSubject(w)
		switch w.Kind {
		case workload.KindDeployment:
			if depIssues, err := d.preflightDeployment(ctx, w, wsubj); err != nil {
				out.Warnf(wsubj, "pre-flight: could not inspect Deployment: %v", err)
			} else {
				issues = append(issues, depIssues...)
			}

		case workload.KindStatefulSet:
			if stsIssues, err := d.preflightStatefulSet(ctx, w, wsubj); err != nil {
				out.Warnf(wsubj, "pre-flight: could not inspect StatefulSet: %v", err)
			} else {
				issues = append(issues, stsIssues...)
			}
		}

		// Name-based stateful service detection — applies to all workload kinds.
		if pattern := matchesStatefulPattern(w.Name); pattern != "" {
			issues = append(issues, preflightIssue{
				subject: wsubj,
				message: fmt.Sprintf(
					"detected known stateful service (%q) — verify replication health and data consistency before draining",
					pattern,
				),
				risk: false,
			})
		}
	}

	// Check PDBs with zero disruptions allowed across all namespaces that
	// have workloads on this node. These may block the eviction step.
	for _, ns := range uniqueNamespaces(workloads) {
		pdbs, err := d.client.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			out.Warnf(subj, "pre-flight: could not list PDBs in %q: %v", ns, err)
			continue
		}
		for i := range pdbs.Items {
			pdb := &pdbs.Items[i]
			if pdb.Status.DisruptionsAllowed == 0 {
				issues = append(issues, preflightIssue{
					subject: fmt.Sprintf("PodDisruptionBudget/%s/%s", ns, pdb.Name),
					message: "0 disruptions currently allowed — eviction of remaining pods may be blocked until the PDB permits it",
					risk:    false,
				})
			}
		}
	}

	if len(issues) == 0 {
		out.Info(subj, "Pre-flight checks passed")
		return nil
	}

	// Emit all findings.
	riskCount := 0
	for _, issue := range issues {
		if issue.risk {
			out.Warnf(issue.subject, "RISK: %s", issue.message)
			riskCount++
		} else {
			out.Warnf(issue.subject, "note: %s", issue.message)
		}
	}

	if d.opts.Preflight == PreflightModeStrict && riskCount > 0 {
		return fmt.Errorf("pre-flight: %d downtime risk(s) found — aborting (use --preflight=warn to proceed anyway)", riskCount)
	}
	return nil
}

// preflightDeployment checks a single Deployment for risk conditions.
func (d *Drainer) preflightDeployment(ctx context.Context, w workload.Workload, subj string) ([]preflightIssue, error) {
	dep, err := d.client.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var issues []preflightIssue

	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}

	if replicas == 1 {
		issues = append(issues, preflightIssue{
			subject: subj,
			message: "single replica — rolling restart will briefly have 0 ready pods (downtime risk)",
			risk:    true,
		})
	}

	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		issues = append(issues, preflightIssue{
			subject: subj,
			message: "Recreate strategy — all pods are terminated before new ones start (guaranteed downtime)",
			risk:    true,
		})
	}

	return issues, nil
}

// preflightStatefulSet checks a single StatefulSet for risk conditions.
func (d *Drainer) preflightStatefulSet(ctx context.Context, w workload.Workload, subj string) ([]preflightIssue, error) {
	sts, err := d.client.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var issues []preflightIssue

	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}

	if replicas == 1 {
		issues = append(issues, preflightIssue{
			subject: subj,
			message: "single replica StatefulSet — rolling restart will cause downtime",
			risk:    true,
		})
	} else {
		// Multi-replica StatefulSets are informational — they restart one pod at
		// a time in reverse ordinal order, which can be slow and requires the
		// application to tolerate it.
		issues = append(issues, preflightIssue{
			subject: subj,
			message: fmt.Sprintf(
				"StatefulSet (%d replicas) — pods restart one at a time in reverse ordinal order; verify the application tolerates a rolling restart",
				replicas,
			),
			risk: false,
		})
	}

	return issues, nil
}

// matchesStatefulPattern returns the first known pattern found (case-insensitive)
// in name, or "" if none match.
func matchesStatefulPattern(name string) string {
	lower := strings.ToLower(name)
	for _, p := range knownStatefulPatterns {
		if strings.Contains(lower, p) {
			return p
		}
	}
	return ""
}

// uniqueNamespaces returns a deduplicated slice of namespaces from workloads.
func uniqueNamespaces(workloads []workload.Workload) []string {
	seen := make(map[string]struct{}, len(workloads))
	var out []string
	for _, w := range workloads {
		if _, ok := seen[w.Namespace]; !ok {
			seen[w.Namespace] = struct{}{}
			out = append(out, w.Namespace)
		}
	}
	return out
}
