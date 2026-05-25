package cmd

// drainOptionSpec records public drain option metadata that should stay
// synchronized across Cobra flags, config/profile support, and docs.
type drainOptionSpec struct {
	Name      string
	ConfigKey string
	Hidden    bool
}

var drainOptionSpecs = []drainOptionSpec{
	{Name: "dry-run", ConfigKey: "dry-run"},
	{Name: "timeout", ConfigKey: "timeout"},
	{Name: "ignore-daemonsets", ConfigKey: "ignore-daemonsets"},
	{Name: "skip-daemon-sets", Hidden: true},
	{Name: "delete-emptydir-data", ConfigKey: "delete-emptydir-data"},
	{Name: "grace-period"},
	{Name: "rollout-timeout", ConfigKey: "rollout-timeout"},
	{Name: "pod-vacate-timeout", ConfigKey: "pod-vacate-timeout"},
	{Name: "eviction-timeout", ConfigKey: "eviction-timeout"},
	{Name: "pdb-retry-interval", ConfigKey: "pdb-retry-interval"},
	{Name: "poll-interval", ConfigKey: "poll-interval"},
	{Name: "force", ConfigKey: "force"},
	{Name: "force-delete-standalone", ConfigKey: "force-delete-standalone"},
	{Name: "max-concurrency", ConfigKey: "max-concurrency"},
	{Name: "log-format", ConfigKey: "log-format"},
	{Name: "uncordon-on-failure", ConfigKey: "uncordon-on-failure"},
	{Name: "selector"},
	{Name: "node-concurrency", ConfigKey: "node-concurrency"},
	{Name: "preflight", ConfigKey: "preflight"},
	{Name: "skip-workload"},
	{Name: "only-workload"},
	{Name: "profile"},
	{Name: "config"},
	{Name: "mode"},
	{Name: "stateful-name-pattern", ConfigKey: "stateful-name-patterns"},
	{Name: "emit-events", ConfigKey: "emit-events"},
	{Name: "resume"},
	{Name: "checkpoint-path"},
}

func publicDrainOptionNames(includeHidden bool) map[string]bool {
	names := make(map[string]bool, len(drainOptionSpecs))
	for _, spec := range drainOptionSpecs {
		if spec.Hidden && !includeHidden {
			continue
		}
		names[spec.Name] = true
	}
	return names
}
