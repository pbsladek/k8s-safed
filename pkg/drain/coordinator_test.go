package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

func TestWorkloadCoordinator_DeduplicatesConcurrentSameWorkload(t *testing.T) {
	c := NewWorkloadCoordinator()
	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Do(context.Background(), w, func(context.Context) error {
				if atomic.AddInt32(&calls, 1) == 1 {
					close(started)
				}
				<-release
				return nil
			})
		}()
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first coordinated call did not start")
	}
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("coordinated call returned error: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("restart function called %d times, want 1", calls)
	}
}

func TestWorkloadCoordinator_DifferentWorkloadsBothRun(t *testing.T) {
	c := NewWorkloadCoordinator()
	var calls int32

	workloads := []workload.Workload{
		{Kind: workload.KindDeployment, Namespace: "default", Name: "api"},
		{Kind: workload.KindDeployment, Namespace: "default", Name: "worker"},
	}
	for _, w := range workloads {
		if err := c.Do(context.Background(), w, func(context.Context) error {
			atomic.AddInt32(&calls, 1)
			return nil
		}); err != nil {
			t.Fatalf("coordinator Do: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("restart function called %d times, want 2", calls)
	}
}

func TestWorkloadCoordinator_ErrorDoesNotMarkDone(t *testing.T) {
	c := NewWorkloadCoordinator()
	w := workload.Workload{Kind: workload.KindDeployment, Namespace: "default", Name: "api"}
	wantErr := errors.New("boom")
	var calls int32

	err := c.Do(context.Background(), w, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	err = c.Do(context.Background(), w, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("second coordinator Do: %v", err)
	}
	if calls != 2 {
		t.Fatalf("restart function called %d times, want 2", calls)
	}
}
