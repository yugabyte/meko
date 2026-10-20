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
// fetch.
//
// A backend that echoes the generation it last applied (Actual.ObservedGen) makes
// that literally true for updates too, which is the point: a generation held only
// in this process and written from an accepted-but-unfinished call is a claim the
// engine cannot check, and an update whose async work then failed used to latch as
// applied forever. The in-process cache of last-applied generations remains as a
// floor for what did complete; losing it costs at most one redundant reconcile
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

// Actual is one resource as the backend currently observes it. YCP is
// authoritative over what it created, so anything the backend lists is a
// candidate for deletion if it is not also desired — unless the row says
// otherwise (Preexisting).
type Actual struct {
	Key ResourceKey
	// Parent is the resource this one hangs off (cluster's pool, branch's
	// cluster, endpoint's branch), or the zero key for a root. Used to gate
	// child creates on parent readiness and to delete only the top of a
	// withdrawn subtree (the backend cascades the rest).
	Parent ResourceKey
	// ObservedGen is the generation the backend last applied for this resource,
	// echoed back so the engine can tell an up-to-date resource from a stale one
	// without its own store. Zero if the backend does not echo it (or has applied
	// nothing yet), in which case only the engine's in-process last-applied cache
	// speaks for the resource.
	//
	// The echo is what makes an update self-heal: it is durable and written from
	// what the backend actually DID, where the cache is written from what a call
	// accepted. Those differ whenever the work is asynchronous, which is why a
	// backend whose verbs plan tasks must report them Pending rather than applied
	// (see PendingError) — otherwise the cache records a generation for work that
	// then failed, and no later tick revisits it (meko#985).
	ObservedGen int64
	// Ready is true when the resource is healthy/usable and its children may be
	// created. State is the backend's free-form status for reporting.
	Ready bool
	State string
	// Detail is the backend's own account of why a resource is not Ready — an
	// endpoint whose pod is crash-looping, say. It is the resource's error when the
	// engine has no reason of its own, and is appended to a stuck one. Empty when
	// Ready, or when the backend cannot say.
	Detail string
	// Progress is an optional marker of forward motion within State — a fleet
	// roll's done/total, say. A change restarts the engine's stuck timers, so work
	// that is long by design but still moving is not reported stuck; one that
	// stops moving still is. Leave it empty for a kind whose State says it all.
	Progress string
	// Attrs are backend-observed, kind-specific details (pg_version, role,
	// compute, extensions, fork_lsn, addresses) reported to YCP verbatim in the
	// resource's status inventory. Nil when the backend has none.
	Attrs map[string]string
	// Preexisting marks a resource the backend did not create and cannot delete:
	// it is on the remote whether or not YCP ever asked for it, and it outlives
	// the declaration that manages it. The amp controller is one — the agent runs
	// inside the release it converges.
	//
	// The engine never plans a Delete for such a row. Without that, the ordinary
	// GC rule reaches the wrong conclusion the moment nobody declares it: in
	// actual, not in desired, so collect it — and a backend that refuses (as it
	// must) latches a permanent drift row on an environment where nothing is
	// wrong.
	//
	// It is a property of the KIND, uniform and independent of what any
	// environment declares, which is why it rides on the row rather than being
	// something the backend has to be told. Everything else still applies: a
	// declared one is updated normally, and an undeclared one is simply reported,
	// which is worth more than hiding it — an operator can see the controller
	// their environment runs without YCP claiming to manage its version.
	Preexisting bool
	// DiscoveryError, when non-nil, means the backend could not read this
	// resource. The row asserts nothing about it — not its state, not its
	// generation, not even that it exists — only that the environment could not be
	// discovered in full and this is one of the parts that is missing.
	//
	// It exists because "absent" and "unread" are otherwise the same shape, and
	// the engine derives creates from absence: a backend that silently returned
	// the rows it managed to read would have every unread resource created again,
	// duplicating whatever the unreachable half still holds. So a backend that
	// cannot enumerate says so per resource rather than returning a short list.
	//
	// Naming the rows is the backend's problem, and it has two sources that do not
	// need the remote: the resources YCP declares, and the ones its own last good
	// List returned. What no such backend can name is a resource that exists but
	// is NOT declared and was never seen — and that is exactly the set a short
	// list cannot hurt, since deletes are derived from what IS in actual.
	//
	// Omitting the row instead is only safe for a resource YCP does not declare:
	// for a declared one, absence IS the create signal, which is the duplicate
	// this field exists to prevent. A backend that cannot read a declared
	// resource must say so with a row; one it cannot read and nobody declared may
	// be dropped, since nothing is derived from its absence.
	DiscoveryError error
	// Failure, when non-nil, is the backend's own observation that a declared
	// resource has failed — work it issued earlier and that ended in an error while
	// the resource stays listed, so no verb of this tick would report it. The
	// engine reports it in Failed and Failing, which degrades the environment on
	// the tick it is seen rather than once the stuck timer fires. It plans and
	// backs off nothing: the row's diff is unchanged.
	Failure error
}

// Backend is the pluggable target the engine drives. Implementations translate
// the engine's generic verbs into a concrete system's API (amp: REST -> tasks).
// All methods must be safe to retry: Create/Update/Delete are issued at most
// once per tick but may be re-issued on later ticks, so they must be idempotent
// against a resource that already exists / is already gone / is mid-flight.
type Backend interface {
	// List returns every YCP-owned resource the backend currently holds. A
	// failed List aborts the tick (the engine will not delete on stale data).
	//
	// A backend that can reach only part of its remote has a third option: return
	// the rows it did read, plus an Actual carrying a DiscoveryError for each declared
	// resource it could not, and no error. The tick then converges what was read
	// and issues nothing for the rest. Returning a SHORT list with no error is
	// the one thing a backend must not do — absence is how the engine decides to
	// create, so an unread resource reported that way is created again.
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

// Reconsiderer is implemented by a backend whose permanent verdict can stop being
// true without the resource's own spec changing: a create refused because a
// sibling holds what it needs (a branch's one read_write slot) is answerable once
// that sibling is gone, and nothing bumps the refused resource's generation. Each
// tick, right after List, the engine drops the permanent mark of every key it
// returns, so the verb is asked again.
type Reconsiderer interface {
	Reconsider() []ResourceKey
}

// PermanentReason says WHY an error is permanent. The engine treats all three
// alike — no retry, no DEGRADED — but they ask different things of whoever reads
// the resource row, so they must not arrive as one word.
//
// Collapsing them is CLOUDGA-36992: the controller refused a resize whose spec
// named cpu without the memory that cpu needs, and the console answered "cannot be
// changed in place" — true of a storage pool, false of an endpoint, and it replaced
// the message naming what to declare.
type PermanentReason int

const (
	// ReasonUnsupported: no verb exists for this resource as declared — a kind
	// with no update/delete path, or a field no verb carries. Nothing the operator
	// writes makes it work; the resource is what it is.
	ReasonUnsupported PermanentReason = iota
	// ReasonRejected: this declaration cannot be applied as written, though a
	// different one could — the remote refused these bytes (a 400), or the backend
	// did (a name it will not take, a spec that does not parse, a field that has to
	// be declared in a later write). Rewriting is the fix, and the error says how.
	ReasonRejected
	// ReasonBlocked: the agent is not permitted to do this — credentials, a scope
	// it lacks (401/403), or a quota no rewrite gets past (the free-tier cap, whose
	// counter is lifetime). Neither the spec nor the resource is wrong.
	ReasonBlocked
)

// State is the value the agent reports for a resource held up for this reason.
func (r PermanentReason) State() string {
	switch r {
	case ReasonRejected:
		return "rejected"
	case ReasonBlocked:
		return "blocked"
	default:
		return "drift"
	}
}

// PermanentError wraps a backend error the engine must not retry: the verb
// cannot succeed for this resource as declared — a kind with no delete path
// (storage pools) or no update path (pools and clusters), or a spec the
// controller rejects outright (400). The engine stops re-issuing the call instead
// of flapping the environment to DEGRADED on an endless backoff of a call that
// will always fail. "As declared" is the limit of it: a rewritten spec is a new
// question, so the next generation retires the mark and the verb is asked again.
// Contrast a transient failure (task rejected, controller down), which is retried
// on backoff.
//
// Reason decides which state the resource is reported in; see PermanentReason.
type PermanentError struct {
	Err    error
	Reason PermanentReason
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as non-retryable for want of a verb: the kind or the field
// structurally has no update/delete path. A backend whose REMOTE refused the spec
// wants Rejected, and one refused by credentials wants Blocked — the engine does
// the same thing with all three, but the resource row does not.
func Permanent(err error) error { return PermanentAs(ReasonUnsupported, err) }

// Rejected marks err as non-retryable because the remote refused the declaration
// as written. Re-declaring is what fixes it.
func Rejected(err error) error { return PermanentAs(ReasonRejected, err) }

// Blocked marks err as non-retryable because the agent is not permitted to make
// the call — credentials, scope, or an entitlement no rewrite of the spec clears.
func Blocked(err error) error { return PermanentAs(ReasonBlocked, err) }

// PermanentAs marks err non-retryable for a reason picked at runtime — for a
// caller that accumulates several failures and reports the reason they add up to.
func PermanentAs(reason PermanentReason, err error) error {
	return &PermanentError{Err: err, Reason: reason}
}

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// PermanentReasonOf is why err is permanent. An error that is not permanent at
// all reports ReasonUnsupported: there is nothing to say about it here, and a
// caller folding errors together wants the zero value.
func PermanentReasonOf(err error) PermanentReason {
	var p *PermanentError
	if !errors.As(err, &p) {
		return ReasonUnsupported
	}
	return p.Reason
}

// PendingError wraps a backend outcome that is neither success nor failure: the
// verb was not issued because the previous one for this resource is still in
// flight. The work is progressing, so the engine must not record it as applied
// (nothing has been), must not back off (nothing failed), and must not degrade
// the environment — it reports the resource as updating and re-checks next tick.
//
// It exists because every mutating controller call is asynchronous: without it the
// only way to tell "the task is still running" from "the task failed" would be a
// timer, which cannot distinguish a slow success from a fast failure.
type PendingError struct{ Err error }

func (e *PendingError) Error() string { return e.Err.Error() }
func (e *PendingError) Unwrap() error { return e.Err }

// Pending marks err as work already in flight. Backends return it when they find
// the previous attempt's task still non-terminal.
func Pending(err error) error { return &PendingError{Err: err} }

func isPending(err error) bool {
	var p *PendingError
	return errors.As(err, &p)
}

// ActionType is the verb the engine chose for a resource this tick.
type ActionType int

const (
	ActionNoop ActionType = iota
	ActionCreate
	ActionUpdate
	ActionDelete
	// ActionUnreadable is the non-verb for a resource the backend could not read
	// (Actual.DiscoveryError). Nothing is issued for it: a create would duplicate
	// what may already be there, an update would be written blind, and a delete
	// would collect a resource nobody confirmed exists. It is an action rather
	// than a silent omission so the tick can report which resources it could not
	// account for — otherwise a partial reading could be reported as converged.
	ActionUnreadable
)

func (a ActionType) String() string {
	switch a {
	case ActionCreate:
		return "create"
	case ActionUpdate:
		return "update"
	case ActionDelete:
		return "delete"
	case ActionUnreadable:
		return "unreadable"
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

// GenerationNever is the ObservedGen of a resource the backend has applied
// nothing to. Below every declared generation, including the 0 a first
// declaration carries, so the update is always planned and the backend's own
// Update decides whether there is anything to do.
//
// Distinct from the zero an echo-less backend reports: a kind that always exists
// is never Created, so with zero it would sit at effGen 0 against a first
// declaration's generation 0 and never be updated at all.
const GenerationNever int64 = -1

// Diff computes the per-resource plan purely from desired, actual, and the
// engine's last-applied generation cache — no ordering, gating, or I/O. This is
// the heart of the engine and is unit-tested in isolation.
//
//   - desired, not in actual                  -> Create
//   - desired, in actual, effGen < desiredGen  -> Update
//   - desired, in actual, effGen >= desiredGen -> Noop (the cheap path)
//   - in actual, not desired                   -> Delete
//
// A row carrying a DiscoveryError overrides all four: the backend could not read
// it, so there is no state to diff and nothing may be issued for it either way.
//
//   - desired, in actual with a DiscoveryError     -> Unreadable (not Create/Update)
//   - in actual with a DiscoveryError, not desired -> Unreadable (not Delete)
//
// And a Preexisting row is never collected, declared or not:
//
//   - in actual with Preexisting, not desired      -> nothing (not Delete)
//
// effGen is max(actual.ObservedGen, cached), so the engine converges whether or
// not the backend echoes an observed generation. The cache is safe as a floor
// because it only ever records work that completed: a backend that plans async
// work reports it Pending, which records nothing.
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
		if a.DiscoveryError != nil {
			// Unread, not absent: this row carries no state to diff against.
			actions = append(actions, Action{Type: ActionUnreadable, Key: k, Resource: d})
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
		if _, ok := desiredByKey[a.Key]; ok {
			continue // already planned by the desired loop above
		}
		if a.DiscoveryError != nil {
			// Withdrawn AND unread. Never a delete — the row does not say the resource
			// is there, and GC on a reading this uncertain is the one mistake that
			// cannot be undone. But not converged either: the collection was never
			// confirmed, so it is reported rather than dropped, or the tick would
			// claim a revision whose deletion it never issued.
			actions = append(actions, Action{Type: ActionUnreadable, Key: a.Key})
			continue
		}
		if a.Preexisting {
			continue // not YCP's to collect; reported, never deleted
		}
		actions = append(actions, Action{Type: ActionDelete, Key: a.Key})
	}
	return actions
}
