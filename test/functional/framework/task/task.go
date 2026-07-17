// Package task is the ops-routine engine (architecture §4.2 "Task",
// implementation §2/§3 M3): a runnable unit that does something rather than
// asserts something, built from the same Apps and primitives tests use.
// `ampqa task <name>` runs one directly — provisioning, log collection,
// artifact delivery, reaping — so the primitives are usable outside tests,
// exactly like the stress framework's src/tasks/.
package task

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Deps carries what a Task body needs. KISS: no environment by default — a
// task that needs one provisions or attaches to one itself (via an env
// provider or the registry); most built-ins (reap, collect-logs) don't need
// the full test lifecycle at all.
type Deps struct {
	Config *core.Config
	Seed   *core.Seed
	Sink   core.Sink
	Logf   func(format string, args ...any)
}

// Task is one runnable ops routine, registered in an init() via task.Register.
type Task struct {
	Name string
	Doc  string
	// Params documents the accepted -o keys (name → description); purely
	// informational — a task reads what it needs straight off Deps.Config, so
	// nothing here validates against it.
	Params map[string]string
	Run    func(ctx context.Context, deps Deps) error
}

// Registry is a name→Task map. The default registry backs package-level
// Register; unit tests construct their own with NewRegistry.
type Registry struct {
	mu    sync.Mutex
	tasks map[string]Task
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tasks: map[string]Task{}}
}

// Add registers a task. It errors on an empty name or a duplicate.
func (r *Registry) Add(t Task) error {
	if t.Name == "" {
		return fmt.Errorf("task has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tasks[t.Name]; dup {
		return fmt.Errorf("duplicate task name %q", t.Name)
	}
	r.tasks[t.Name] = t
	return nil
}

// Get returns a task by name.
func (r *Registry) Get(name string) (Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[name]
	return t, ok
}

// List returns all tasks sorted by name.
func (r *Registry) List() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var defaultRegistry = NewRegistry()

// Register adds a task to the default registry, panicking on error (intended
// for init()).
func Register(t Task) {
	if err := defaultRegistry.Add(t); err != nil {
		panic(err)
	}
}

// Default returns the package-level registry.
func Default() *Registry { return defaultRegistry }
