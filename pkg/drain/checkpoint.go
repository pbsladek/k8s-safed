package drain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pbsladek/k8s-safed/pkg/workload"
)

// Checkpoint records which workloads have completed rolling restarts so an
// interrupted drain can be resumed without re-restarting already-done workloads.
//
// The checkpoint is written atomically (temp file + rename) after each
// successful workload so a crash mid-write never leaves a corrupt file. It is
// deleted on successful drain completion and left in place on failure so the
// operator can resume.
//
// File location: ~/.kube/safed-checkpoints/<context>-<node>.json
type Checkpoint struct {
	NodeName  string                    `json:"nodeName"`
	Context   string                    `json:"context"`
	Completed map[string]bool           `json:"completed,omitempty"`
	Workloads map[string]CheckpointWork `json:"workloads,omitempty"`
}

// CheckpointWork records the workload identity observed when a checkpoint entry
// was written. Completed is kept separate from the legacy Completed map so older
// checkpoint files can still be read.
type CheckpointWork struct {
	Kind        string    `json:"kind"`
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	UID         string    `json:"uid,omitempty"`
	Generation  int64     `json:"generation,omitempty"`
	CompletedAt time.Time `json:"completedAt"`
}

// CheckpointPath returns the default path for a checkpoint file, namespaced
// by kubeconfig context and node name so different clusters/nodes don't collide.
func CheckpointPath(kubeContext, nodeName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	dir := filepath.Join(home, ".kube", "safed-checkpoints")
	filename := sanitizeFilename(kubeContext) + "-" + sanitizeFilename(nodeName) + ".json"
	return filepath.Join(dir, filename), nil
}

// sanitizeFilename replaces characters that are problematic in filenames with
// hyphens, producing a safe single-component name.
func sanitizeFilename(s string) string {
	return strings.NewReplacer("/", "-", ":", "-", " ", "-", "\\", "-").Replace(s)
}

// LoadCheckpoint reads an existing checkpoint from path. If the file does not
// exist, an empty checkpoint is returned (not an error) so the caller can
// treat first-run and resume identically.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newCheckpoint(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading checkpoint %q: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("parsing checkpoint %q: %w", path, err)
	}
	if cp.Completed == nil {
		cp.Completed = make(map[string]bool)
	}
	if cp.Workloads == nil {
		cp.Workloads = make(map[string]CheckpointWork)
	}
	return &cp, nil
}

func newCheckpoint() *Checkpoint {
	return &Checkpoint{
		Completed: make(map[string]bool),
		Workloads: make(map[string]CheckpointWork),
	}
}

// Save writes the checkpoint to path atomically. The directory is created if
// it does not exist.
func (c *Checkpoint) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating checkpoint directory: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling checkpoint: %w", err)
	}
	// Atomic write: write to a temp file then rename so readers never see a
	// partial file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing checkpoint: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("committing checkpoint: %w", err)
	}
	return nil
}

// MarkDone records w as completed in the checkpoint.
func (c *Checkpoint) MarkDone(w workload.Workload) {
	c.MarkDoneAt(w, time.Now().UTC())
}

// MarkDoneAt records w as completed with an explicit timestamp.
func (c *Checkpoint) MarkDoneAt(w workload.Workload, completedAt time.Time) {
	key := workloadKey(w)
	if c.Completed == nil {
		c.Completed = make(map[string]bool)
	}
	if c.Workloads == nil {
		c.Workloads = make(map[string]CheckpointWork)
	}
	c.Completed[key] = true
	c.Workloads[key] = CheckpointWork{
		Kind:        string(w.Kind),
		Namespace:   w.Namespace,
		Name:        w.Name,
		UID:         string(w.UID),
		Generation:  w.Generation,
		CompletedAt: completedAt.UTC(),
	}
}

// IsDone reports whether w was previously completed.
func (c *Checkpoint) IsDone(w workload.Workload) bool {
	return c.Completed[workloadKey(w)]
}

// Work returns checkpoint metadata for w. The boolean is false for legacy
// checkpoint entries that only have the Completed map.
func (c *Checkpoint) Work(w workload.Workload) (CheckpointWork, bool) {
	work, ok := c.Workloads[workloadKey(w)]
	return work, ok
}

// DeleteCheckpoint removes the checkpoint file. A missing file is not an error.
func DeleteCheckpoint(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func workloadKey(w workload.Workload) string {
	return fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
}
