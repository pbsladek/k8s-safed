package drain

import "time"

// pollInterval returns the configured poll interval, falling back to 5 s.
func (d *Drainer) pollInterval() time.Duration {
	if d.opts.PollInterval > 0 {
		return d.opts.PollInterval
	}
	return 5 * time.Second
}

// podVacateTimeout returns the per-workload deadline for pod departure, falling
// back to 2 min.
func (d *Drainer) podVacateTimeout() time.Duration {
	if d.opts.PodVacateTimeout > 0 {
		return d.opts.PodVacateTimeout
	}
	return 2 * time.Minute
}

// evictionTimeout returns the per-pod PDB-retry deadline, falling back to 5 min.
func (d *Drainer) evictionTimeout() time.Duration {
	if d.opts.EvictionTimeout > 0 {
		return d.opts.EvictionTimeout
	}
	return 5 * time.Minute
}

// pdbRetryInterval returns the base backoff interval for PDB-blocked evictions,
// falling back to 5 s.
func (d *Drainer) pdbRetryInterval() time.Duration {
	if d.opts.PDBRetryInterval > 0 {
		return d.opts.PDBRetryInterval
	}
	return 5 * time.Second
}
