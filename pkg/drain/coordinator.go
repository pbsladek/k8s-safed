package drain

import (
	"context"
	"sync"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// WorkloadCoordinator coordinates workload restarts across concurrently running
// Drainers. It is intentionally process-local; it prevents duplicate restart
// patches during a single kubectl-safed invocation.
type WorkloadCoordinator struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	done  map[string]struct{}
}

// NewWorkloadCoordinator creates a process-local workload restart coordinator.
func NewWorkloadCoordinator() *WorkloadCoordinator {
	return &WorkloadCoordinator{
		locks: make(map[string]*sync.Mutex),
		done:  make(map[string]struct{}),
	}
}

func (c *WorkloadCoordinator) Do(ctx context.Context, w workload.Workload, fn func(context.Context) error) error {
	if c == nil {
		return fn(ctx)
	}
	key := workloadKey(w)
	lock := c.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	_, alreadyDone := c.done[key]
	c.mu.Unlock()
	if alreadyDone {
		return nil
	}
	if err := fn(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	if c.done == nil {
		c.done = make(map[string]struct{})
	}
	c.done[key] = struct{}{}
	c.mu.Unlock()
	return nil
}

func (c *WorkloadCoordinator) lockFor(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = make(map[string]*sync.Mutex)
	}
	lock := c.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[key] = lock
	}
	return lock
}
