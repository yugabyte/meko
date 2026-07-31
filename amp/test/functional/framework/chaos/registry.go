package chaos

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a ChaosAction from a resolved Config (its severity/cadence
// knobs). Kept as a function so an action reads only the knobs it needs.
type Factory func(cfg Config) ChaosAction

// Registry is a plain name→factory map (mutex-guarded, per the framework's
// no-registries-beyond-a-map rule) letting a scenario select actions by name.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]Factory{}} }

// Register adds (or replaces) the factory for name.
func (r *Registry) Register(name string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = f
}

// Names returns the registered action names, sorted — a stable order so
// seed-driven selection over "all actions" is reproducible.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Build constructs the named action with cfg, erroring if it is not registered.
func (r *Registry) Build(name string, cfg Config) (ChaosAction, error) {
	r.mu.RLock()
	f, ok := r.m[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("chaos: unknown action %q", name)
	}
	return f(cfg), nil
}

// DefaultRegistry returns a registry populated with every built-in action.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(ActionPodDelete, func(c Config) ChaosAction {
		return podDelete{waitTimeout: c.WaitTimeout}
	})
	r.Register(ActionProcessKill, func(c Config) ChaosAction {
		return processKill{pattern: c.ProcessPattern, unit: c.ServiceUnit, waitTimeout: c.WaitTimeout}
	})
	r.Register(ActionProcessRestart, func(c Config) ChaosAction {
		return processRestart{pattern: c.ProcessPattern, unit: c.ServiceUnit, waitTimeout: c.WaitTimeout}
	})
	r.Register(ActionNodeRestart, func(c Config) ChaosAction {
		return nodeRestart{units: c.NodeUnits, pattern: c.ProcessPattern, waitTimeout: c.WaitTimeout}
	})
	r.Register(ActionNetworkPartition, func(c Config) ChaosAction {
		return networkPartition{device: c.NetDevice, ports: c.PartitionPorts}
	})
	r.Register(ActionNetworkSlowdown, func(c Config) ChaosAction {
		return networkSlowdown{device: c.NetDevice, delayMs: c.DelayMs}
	})
	r.Register(ActionPacketLoss, func(c Config) ChaosAction {
		return packetLoss{device: c.NetDevice, percent: c.LossPercent}
	})
	r.Register(ActionNodeStop, func(c Config) ChaosAction {
		return nodeStop{waitTimeout: c.WaitTimeout}
	})
	return r
}
