package drain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

type fixedClock struct {
	t time.Time
}

func (f fixedClock) Now() time.Time { return f.t }

func (f fixedClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time)
	return ch
}

func TestBuildRestartPatchUsesProvidedTime(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 34, 56, 0, time.UTC)
	patch, err := buildRestartPatch(now)
	if err != nil {
		t.Fatalf("buildRestartPatch: %v", err)
	}
	if !strings.Contains(string(patch), `"kubectl.kubernetes.io/restartedAt":"2026-05-08T12:34:56Z"`) {
		t.Fatalf("restart patch did not contain fixed timestamp: %s", patch)
	}
}

func TestCheckpointMarkDoneAtUsesProvidedTime(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 34, 56, 0, time.UTC)
	cp := newCheckpoint()
	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}

	cp.MarkDoneAt(w, now)

	work, ok := cp.Work(w)
	if !ok {
		t.Fatal("checkpoint work metadata missing")
	}
	if !work.CompletedAt.Equal(now) {
		t.Fatalf("CompletedAt = %s, want %s", work.CompletedAt, now)
	}
}

func TestEventEmitterUsesProvidedTime(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 34, 56, 123, time.UTC)
	emitter := NewEventEmitterWithClock(fake.NewClientset(), NewPrinterTo(&bytes.Buffer{}), true, fixedClock{t: now})

	event := emitter.build("Node", "worker-1", "", "Draining", "message", corev1.EventTypeNormal)

	wantName := fmt.Sprintf("worker-1.%016x", now.UnixNano())
	if event.Name != wantName {
		t.Fatalf("event name = %q, want deterministic timestamp suffix", event.Name)
	}
	if !event.FirstTimestamp.Time.Equal(now) || !event.LastTimestamp.Time.Equal(now) {
		t.Fatalf("event timestamps = %s/%s, want %s", event.FirstTimestamp.Time, event.LastTimestamp.Time, now)
	}
}

func TestPrinterElapsedUsesProvidedNow(t *testing.T) {
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	p := NewPrinterWithFormat(&buf, LogFormatJSON)
	p.now = func() time.Time { return start.Add(1500 * time.Millisecond) }

	p.Elapsed(start, "node-a", "Complete")

	var rec map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode json log: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "Complete (1.5s)" {
		t.Fatalf("elapsed message = %q, want Complete (1.5s)", rec["msg"])
	}
}
