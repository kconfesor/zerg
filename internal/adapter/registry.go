package adapter

import (
	"fmt"
	"os/exec"
	"sort"
	"sync"
)

// Registry holds the harnesses this build knows about.
//
// Keeping backends in a validation set and a case expression means adding one
// is an edit in two places and a re-read in a third. Registering an
// implementation is the whole ceremony here.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register adds an adapter, replacing any earlier one of the same name.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get returns the adapter for a harness name.
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("no adapter for harness %q; this build knows %v", name, r.namesLocked())
	}
	return a, nil
}

// Names lists registered harnesses, for the role editor's harness picker.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *Registry) namesLocked() []string {
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ── generic checks ────────────────────────────────────────────────────────

// BinaryPresent reports whether the harness executable is on PATH.
//
// This is the cheapest check and the one that turns "an agent that never
// started and never said why" into a sentence naming the missing program.
func BinaryPresent(binary string) Check {
	return Check{
		Name: "binary_present",
		Run: func(_ Ctx, _ Spec) Result {
			path, err := exec.LookPath(binary)
			if err != nil {
				return Result{
					Reason: fmt.Sprintf("%s is not on PATH", binary),
					Remedy: fmt.Sprintf("install %s, or point this role at a harness you have", binary),
				}
			}
			return Result{OK: true, Detail: path}
		},
	}
}
