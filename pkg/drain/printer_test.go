package drain

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrinterPlainGolden(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinterTo(&buf)
	p.now = fixedPrinterTime

	p.Start("Deployment/default/api", "Rolling restart [1/2]")
	p.Poll("Deployment/default/api", "rollout updated=1/2 ready=1/2 available=1/2")
	p.Done("Deployment/default/api", "Complete")

	assertGolden(t, "printer_plain.golden", buf.String())
}

func TestPrinterJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinterWithFormat(&buf, LogFormatJSON)
	p.now = fixedPrinterTime

	p.Start("Deployment/default/api", "Rolling restart [1/2]")
	p.Poll("Deployment/default/api", "rollout updated=1/2 ready=1/2 available=1/2")
	p.Done("Deployment/default/api", "Complete")

	assertGolden(t, "printer_json.golden", buf.String())
}

func fixedPrinterTime() time.Time {
	return time.Date(2026, 3, 15, 15, 4, 5, 0, time.UTC)
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
