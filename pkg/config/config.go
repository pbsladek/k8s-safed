// Package config loads and parses the kubectl-safed configuration file.
//
// The default config file is ~/.kube/safed.yaml. Override with the
// KUBECTL_SAFED_CONFIG environment variable or the --config flag.
//
// Example config file:
//
//	profiles:
//	  prod:
//	    preflight: strict
//	    rollout-timeout: 10m
//	    max-concurrency: 1
//	    uncordon-on-failure: true
//	  staging:
//	    preflight: warn
//	    rollout-timeout: 3m
//	    max-concurrency: 3
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sigsyaml "sigs.k8s.io/yaml"
)

// Duration is a time.Duration that unmarshals from a YAML/JSON string like "5m" or "30s".
type Duration struct {
	D time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	// sigs.k8s.io/yaml converts YAML to JSON first, so we always receive JSON here.
	// The value arrives as a quoted string (e.g. `"5m"`) or an integer nanosecond count.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.D = parsed
		return nil
	}
	// Fall back: treat as nanosecond integer.
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return fmt.Errorf("cannot parse duration from %s", b)
	}
	d.D = time.Duration(ns)
	return nil
}

// Profile holds per-profile flag overrides. Pointer fields let the profile
// system distinguish "not set in profile" from "explicitly set to zero value"
// so that CLI flags always win over profile defaults.
type Profile struct {
	Timeout               *Duration `json:"timeout,omitempty"`
	RolloutTimeout        *Duration `json:"rollout-timeout,omitempty"`
	PodVacateTimeout      *Duration `json:"pod-vacate-timeout,omitempty"`
	EvictionTimeout       *Duration `json:"eviction-timeout,omitempty"`
	PDBRetryInterval      *Duration `json:"pdb-retry-interval,omitempty"`
	PollInterval          *Duration `json:"poll-interval,omitempty"`
	MaxConcurrency        *int      `json:"max-concurrency,omitempty"`
	NodeConcurrency       *int      `json:"node-concurrency,omitempty"`
	Preflight             string    `json:"preflight,omitempty"`
	LogFormat             string    `json:"log-format,omitempty"`
	DryRun                *bool     `json:"dry-run,omitempty"`
	Force                 *bool     `json:"force,omitempty"`
	IgnoreDaemonSets      *bool     `json:"ignore-daemonsets,omitempty"`
	DeleteEmptyDir        *bool     `json:"delete-emptydir-data,omitempty"`
	ForceDeleteStandalone *bool     `json:"force-delete-standalone,omitempty"`
	UncordonOnFailure     *bool     `json:"uncordon-on-failure,omitempty"`
	EmitEvents            *bool     `json:"emit-events,omitempty"`
}

// Config is the top-level structure of the safed config file.
type Config struct {
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

// DefaultConfigPath returns ~/.kube/safed.yaml.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".kube", "safed.yaml"), nil
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	var cfg Config
	if err := sigsyaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return &cfg, nil
}

// GetProfile returns the named profile, or an error listing available profiles.
func (c *Config) GetProfile(name string) (Profile, error) {
	p, ok := c.Profiles[name]
	if !ok {
		names := make([]string, 0, len(c.Profiles))
		for k := range c.Profiles {
			names = append(names, k)
		}
		return Profile{}, fmt.Errorf("profile %q not found (available: %s)", name, strings.Join(names, ", "))
	}
	return p, nil
}
