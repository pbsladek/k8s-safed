package config

import (
	"encoding/json"
	"testing"
	"time"
)

func FuzzDurationUnmarshalJSON(f *testing.F) {
	f.Add(`"5m"`)
	f.Add(`"0s"`)
	f.Add(`3000000000`)
	f.Add(`"not-a-duration"`)

	f.Fuzz(func(t *testing.T, raw string) {
		var d Duration
		err := json.Unmarshal([]byte(raw), &d)
		if err == nil {
			if _, parseErr := time.ParseDuration(d.D.String()); parseErr != nil {
				t.Fatalf("parsed duration does not round-trip through time.ParseDuration: %s", d.D)
			}
		}
	})
}

func FuzzPreflightModeUnmarshalJSON(f *testing.F) {
	f.Add(`"warn"`)
	f.Add(`"strict"`)
	f.Add(`"off"`)
	f.Add(`false`)
	f.Add(`true`)

	f.Fuzz(func(t *testing.T, raw string) {
		var p PreflightMode
		err := json.Unmarshal([]byte(raw), &p)
		if err == nil && p == "" {
			t.Fatalf("successful preflight parse returned empty mode for %s", raw)
		}
	})
}
