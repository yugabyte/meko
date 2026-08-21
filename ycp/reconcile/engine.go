// Package reconcile is the generic, generation-diffing engine the reconcile
// agent runs to converge a target system to YCP's desired state. It is
// deliberately kind-agnostic: the only per-resource-type knowledge lives behind
// the Backend interface (one implementation per target — the amp controller's
// REST API today, meko's k8s/api-server later), so adding a resource type is a
// case in a backend switch, not a new reconciler.
//
// The loop is level-triggered. Correctness comes from re-reading actual state
// every tick and diffing it against desired, not from state held between ticks:
// a restarted agent rebuilds everything from the next List() + desired-state
// fetch. The only in-process state is a soft cache of last-applied generations,
// used to skip no-op ticks; losing it costs at most one redundant reconcile
// pass, never correctness.
package reconcile

import (
	"context"
	"errors"

	"meko/ycp/internal/api"
)

// ResourceKey identifies a desired/actual resource within an environment. Kind
// plus Name is unique because YCP names are unique within a kind for an env.
type ResourceKey struct {
	Kind string
	Name string
}

func (k ResourceKey) zero() bool { return k.Kind == "" && k.Name == "" }

func (k ResourceKey) String() string { return k.Kind + "/" + k.Name }

// Actual is one resource as the backend currently observes it. Only
// YCP-owned resources are reported — YCP is authoritative, so anything the
// backend lists is a candidate for deletion if it is not also desired.
type Actual struct {
	Key ResourceKey
	// Parent is the resource this one hangs off (cluster's pool, branch's
	// cluster, endpoint's branch), or the zero key for a root. Used to gate
	// child creates on parent readiness and to delete only the top of a
	// withdrawn subtree (the backend cascades the rest).
	Parent ResourceKey
	// ObservedGen is the generation the backend last applied for this resource,
	// echoed back so the engine can tell an up-to-date resource from a stale
	// one without its own store. Zero if the backend does not echo it, in which
	// case the engine falls back to its in-process last-applied cache.
	ObservedGen int64
	// Ready is true when the resource is healthy/usable and its children may be
	// created. State is the backend's free-form status for reporting.
	Ready bool
	State string
	// Attrs are backend-observed, kind-specific details (pg_version, role,
	// compute, extensions, fork_lsn, addresses) reported to YCP verbatim in the
	// resource's status inventory. Nil when the backend has none.
	Attrs map[string]string
}

// Backend is the pluggable target the engine drives. Implementations translate
// the engine's generic verbs into a concrete system's API (amp: REST -> tasks).
// All methods must be safe to retry: Create/Update/Delete are issued at most
// once per tick but may be re-issued on later ticks, so they must be idempotent
// against a resource that already exists / is already gone / is mid-flight.
type Backend interface {
	// List returns every YCP-owned resource the backend currently holds. A
	// failed List aborts the tick (the engine will not delete on stale data).
	List(ctx context.Context) ([]Actual, error)
	// Create/Update/Delete may be asynchronous: the engine issues them once and
	// re-checks via List on the next tick, so an in-flight create is seen as
	// existing and never issued twice.
	Create(ctx context.Context, r api.DesiredResource) error
	Update(ctx context.Context, r api.DesiredResource) error
	Delete(ctx context.Context, key ResourceKey) error
	// Parent returns the desired resource's parent key and whether it has one.
	// Roots return (_, false). The engine gates a create on the parent being
	// present and Ready, and issues creates parent-first.
	Parent(r api.DesiredResource) (ResourceKey, bool)
}

// PermanentError wraps a backend error the engine must not retry: the verb can
// never succeed for this resource as declared — a kind with no delete path
// (storage pools) or no update path (pools/clusters/branches). The engine
// surfaces it as drift on the resource and stops re-issuing the call, instead of
// flapping the environment to DEGRADED on an endless backoff of a call that will
// always fail. Contrast a transient failure (task rejected, controller down),
// which is retried.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as non-retryable. Backends return it for a verb a kind
// structurally does not support.
func Permanent(err error) error { return &PermanentError{Err: err} }

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// ActionType is the verb the engine chose for a resource this tick.
type ActionType int

const (
	ActionNoop ActionType = iota
	ActionCreate
	ActionUpdate
	ActionDelete
)

func (a ActionType) String() string {
	switch a {
	case ActionCreate:
		return "create"
	case ActionUpdate:
		return "update"
	case ActionDelete:
		return "delete"
	default:
		return "noop"
	}
}

// Action is one planned change (or no-op) for a single resource.
type Action struct {
	Type ActionType
	Key  ResourceKey
	// Resource carries the desired spec for create/update; empty for delete.
	Resource api.DesiredResource
}

// Diff computes the per-resource plan purely from desired, actual, and the
// engine's last-applied generation cache — no ordering, gating, or I/O. This is
// the heart of the engine and is unit-tested in isolation.
//
//   - desired, not in actual                -> Create
//   - desired, in actual, effGen < desiredGen -> Update
//   - desired, in actual, effGen >= desiredGen -> Noop (the cheap path)
//   - in actual, not desired                -> Delete
//
// effGen is max(actual.ObservedGen, cached), so the engine converges whether or
// not the backend echoes an observed generation.
func Diff(desired []api.DesiredResource, actual []Actual, cache map[ResourceKey]int64) []Action {
	desiredByKey := make(map[ResourceKey]api.DesiredResource, len(desired))
	for _, d := range desired {
		desiredByKey[ResourceKey{d.Kind, d.Name}] = d
	}
	actualByKey := make(map[ResourceKey]Actual, len(actual))
	for _, a := range actual {
		actualByKey[a.Key] = a
	}

	var actions []Action
	for _, d := range desired {
		k := ResourceKey{d.Kind, d.Name}
		a, ok := actualByKey[k]
		if !ok {
			actions = append(actions, Action{Type: ActionCreate, Key: k, Resource: d})
			continue
		}
		effGen := a.ObservedGen
		if c, ok := cache[k]; ok && c > effGen {
			effGen = c
		}
		if effGen < d.Generation {
			actions = append(actions, Action{Type: ActionUpdate, Key: k, Resource: d})
		} else {
			actions = append(actions, Action{Type: ActionNoop, Key: k, Resource: d})
		}
	}
	for _, a := range actual {
		if _, ok := desiredByKey[a.Key]; !ok {
			actions = append(actions, Action{Type: ActionDelete, Key: a.Key})
		}
	}
	return actions
}
