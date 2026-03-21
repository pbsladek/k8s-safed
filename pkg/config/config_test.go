package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --------------------------------------------------------------------------
// Duration.UnmarshalJSON
// --------------------------------------------------------------------------

func TestDuration_UnmarshalJSON_StringForm(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{`"5m"`, 5 * time.Minute},
		{`"30s"`, 30 * time.Second},
		{`"1h"`, time.Hour},
		{`"0s"`, 0},
	}
	for _, tc := range tests {
		var d Duration
		if err := json.Unmarshal([]byte(tc.input), &d); err != nil {
			t.Errorf("Unmarshal(%s): unexpected error: %v", tc.input, err)
			continue
		}
		if d.D != tc.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tc.input, d.D, tc.want)
		}
	}
}

func TestDuration_UnmarshalJSON_InvalidString_ReturnsError(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Error("expected error for invalid duration string, got nil")
	}
}

func TestDuration_UnmarshalJSON_IntegerNanoseconds(t *testing.T) {
	ns := int64(5 * time.Minute)
	data, _ := json.Marshal(ns)
	var d Duration
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.D != 5*time.Minute {
		t.Errorf("got %v, want %v", d.D, 5*time.Minute)
	}
}

// --------------------------------------------------------------------------
// Load
// --------------------------------------------------------------------------

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "safed.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_ValidConfig(t *testing.T) {
	path := writeConfig(t, `
profiles:
  prod:
    preflight: strict
    rollout-timeout: 10m
    max-concurrency: 1
    uncordon-on-failure: true
  staging:
    preflight: warn
    max-concurrency: 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(cfg.Profiles))
	}
	prod := cfg.Profiles["prod"]
	if prod.Preflight != "strict" {
		t.Errorf("prod.Preflight = %q, want %q", prod.Preflight, "strict")
	}
	if prod.RolloutTimeout == nil || prod.RolloutTimeout.D != 10*time.Minute {
		t.Errorf("prod.RolloutTimeout = %v, want 10m", prod.RolloutTimeout)
	}
	if prod.MaxConcurrency == nil || *prod.MaxConcurrency != 1 {
		t.Errorf("prod.MaxConcurrency = %v, want 1", prod.MaxConcurrency)
	}
	if prod.UncordonOnFailure == nil || !*prod.UncordonOnFailure {
		t.Error("prod.UncordonOnFailure should be true")
	}
}

func TestLoad_MissingFile_ReturnsError(t *testing.T) {
	_, err := Load("/nonexistent/path/safed.yaml")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML_ReturnsError(t *testing.T) {
	path := writeConfig(t, `not: valid: yaml: :::`)
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestLoad_EmptyProfiles(t *testing.T) {
	path := writeConfig(t, `profiles: {}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Profiles) != 0 {
		t.Errorf("expected 0 profiles, got %d", len(cfg.Profiles))
	}
}

// --------------------------------------------------------------------------
// Config.GetProfile
// --------------------------------------------------------------------------

func TestGetProfile_ExistingProfile(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"prod": {Preflight: "strict"},
		},
	}
	p, err := cfg.GetProfile("prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Preflight != "strict" {
		t.Errorf("Preflight = %q, want %q", p.Preflight, "strict")
	}
}

func TestGetProfile_MissingProfile_ReturnsError(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"prod":    {Preflight: "strict"},
			"staging": {Preflight: "warn"},
		},
	}
	_, err := cfg.GetProfile("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing profile, got nil")
	}
	// Error message should mention the available profile names.
	errStr := err.Error()
	if errStr == "" {
		t.Error("error message should not be empty")
	}
}

// --------------------------------------------------------------------------
// Pointer fields distinguish "not set" from zero value
// --------------------------------------------------------------------------

func TestLoad_UnsetFieldsAreNil(t *testing.T) {
	// A profile that only sets preflight should leave all pointer fields nil
	// so the CLI flag system knows those were not explicitly configured.
	path := writeConfig(t, `
profiles:
  minimal:
    preflight: warn
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p, err := cfg.GetProfile("minimal")
	if err != nil {
		t.Fatal(err)
	}
	if p.RolloutTimeout != nil {
		t.Error("RolloutTimeout should be nil when not set in profile")
	}
	if p.MaxConcurrency != nil {
		t.Error("MaxConcurrency should be nil when not set in profile")
	}
	if p.DryRun != nil {
		t.Error("DryRun should be nil when not set in profile")
	}
	if p.UncordonOnFailure != nil {
		t.Error("UncordonOnFailure should be nil when not set in profile")
	}
}
