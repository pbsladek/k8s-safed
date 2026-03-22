package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

const eventSourceComponent = "kubectl-safed"

// EventEmitter emits Kubernetes Events to node and workload objects during a
// drain. It is a no-op when disabled (--emit-events is false), so it is safe
// to call unconditionally throughout the drain logic.
type EventEmitter struct {
	client  kubernetes.Interface
	out     *Printer
	enabled bool
}

// NewEventEmitter creates an EventEmitter. When enabled is false all methods
// are no-ops and no API calls are made.
func NewEventEmitter(client kubernetes.Interface, out *Printer, enabled bool) *EventEmitter {
	return &EventEmitter{client: client, out: out, enabled: enabled}
}

// NodeEvent emits an Event to the node object. evType is corev1.EventTypeNormal
// or corev1.EventTypeWarning.
func (e *EventEmitter) NodeEvent(ctx context.Context, nodeName, reason, msg, evType string) {
	if !e.enabled {
		return
	}
	ev := e.build("Node", nodeName, "", reason, msg, evType)
	e.create(ctx, "default", ev)
}

// WorkloadEvent emits an Event to the Deployment or StatefulSet object.
func (e *EventEmitter) WorkloadEvent(ctx context.Context, w workload.Workload, reason, msg, evType string) {
	if !e.enabled {
		return
	}
	ev := e.build(string(w.Kind), w.Name, w.Namespace, reason, msg, evType)
	e.create(ctx, w.Namespace, ev)
}

func (e *EventEmitter) build(kind, name, namespace, reason, msg, evType string) *corev1.Event {
	now := metav1.Now()
	// Event name must be unique; combine resource name with nanosecond timestamp.
	evName := fmt.Sprintf("%s.%016x", name, time.Now().UnixNano())
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      evName,
			Namespace: coalesce(namespace, "default"),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      kind,
			Name:      name,
			Namespace: namespace,
		},
		Reason:         reason,
		Message:        msg,
		Type:           evType,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: eventSourceComponent},
	}
}

func (e *EventEmitter) create(ctx context.Context, namespace string, ev *corev1.Event) {
	_, err := e.client.CoreV1().Events(namespace).Create(ctx, ev, metav1.CreateOptions{})
	if err != nil {
		// Event emission failures are best-effort; log but don't abort the drain.
		e.out.Warnf(ev.InvolvedObject.Name, "failed to emit event %q: %v", ev.Reason, err)
	}
}

// coalesce returns s if non-empty, else fallback.
func coalesce(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
