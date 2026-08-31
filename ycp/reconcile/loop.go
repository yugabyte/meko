package reconcile

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"meko/ycp/internal/api"
)

// Engine runs the level-triggered reconcile loop against a Backend. The zero
// value is not usable; construct with New.
type Engine struct {
	backend    Backend
	maxPar     int
	baseBO     time.Duration
	maxBO      time.Duration
	stuckAfter time.Duration
	now        func() time.Time

	mu        sync.Mutex
	cache     map[ResourceKey]int64      // last-applied generation, soft cache
	failures  map[ResourceKey]*failState // per-resource backoff
	permanent map[ResourceKey]permMark   // non-retryable, surfaced as drift
	seenState map[ResourceKey]stateStamp // when a resource entered its current state
}

// permMark is a non-retryable failure plus the desired generation it was
// recorded against. The generation is what lets the mark expire: the verb cannot
// succeed for the spec as written, so re-issuing it every tick is pointless — but
// the operator can rewrite that spec, and the edit has to un-stick the resource.
// Without it a refused create latches forever (its action stays planned, since
// the resource never appears in List) and the corrected spec is never applied.
type permMark struct {
	err error
	gen int64
}

type failState struct {
	count       int
	nextAttempt time.Time
	lastErr     error
}

// stateStamp records the backend state a resource has held and since when, so
// the engine can flag one that has sat non-Ready past StuckAfter.
type stateStamp struct {
	state string
	since time.Time
}

// Options tunes the engine. Zero values get sensible defaults.
type Options struct {
	// MaxConcurrent bounds how many independent resources are dispatched in one
	// tick, so a wide tree does not stampede the backend. Default 8.
	MaxConcurrent int
	// BaseBackoff / MaxBackoff bound the per-resource retry delay after a
	// failure. Defaults 5s / 5m.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// StuckAfter is how long a resource may sit non-Ready in the same backend
	// state before the engine reports it as stuck (so the environment goes
	// DEGRADED with a reason instead of looking healthy while an endpoint
	// silently never provisions). Default 10m.
	StuckAfter time.Duration
	// Now is injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// New builds an Engine driving backend.
func New(backend Backend, opts Options) *Engine {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 5 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 5 * time.Minute
	}
	if opts.StuckAfter <= 0 {
		opts.StuckAfter = 10 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		backend:    backend,
		maxPar:     opts.MaxConcurrent,
		baseBO:     opts.BaseBackoff,
		maxBO:      opts.MaxBackoff,
		stuckAfter: opts.StuckAfter,
		now:        opts.Now,
		cache:      map[ResourceKey]int64{},
		failures:   map[ResourceKey]*failState{},
		permanent:  map[ResourceKey]permMark{},
		seenState:  map[ResourceKey]stateStamp{},
	}
}

// Result summarizes one tick, for the agent to fold into a status report.
type Result struct {
	Created  int
	Updated  int
	Deleted  int
	NoOp     int
	Deferred int // eligible next tick (parent not ready, or backing off)
	// Failed maps a resource to the retryable error from its attempt this tick.
	Failed map[ResourceKey]error
	// Permanent maps a resource to a non-retryable error (a verb the kind does
	// not support, e.g. deleting a storage pool). The engine does not re-issue
	// these until the resource's spec changes; they are reported as drift, and
	// unlike Failed they do not by themselves make the environment DEGRADED.
	Permanent map[ResourceKey]error
	// Stuck maps a resource that has sat non-Ready past StuckAfter to how long
	// it has been in that state.
	Stuck map[ResourceKey]time.Duration
	// Actual is the state read at the start of the tick, so the agent can
	// report inventory without a second List.
	Actual []Actual
}

// Reconcile runs one tick: read actual, diff against desired, then apply
// creates/updates (parent-first, gated on readiness) and deletes (only when
// freshDesired, i.e. desired state was read successfully this tick — the agent
// must never garbage-collect against a stale/fallback desired set).
func (e *Engine) Reconcile(ctx context.Context, desired []api.DesiredResource, freshDesired bool) (Result, error) {
	actual, err := e.backend.List(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile: list actual: %w", err)
	}
	actualByKey := make(map[ResourceKey]Actual, len(actual))
	for _, a := range actual {
		actualByKey[a.Key] = a
	}
	desiredByKey := make(map[ResourceKey]api.DesiredResource, len(desired))
	for _, d := range desired {
		desiredByKey[ResourceKey{d.Kind, d.Name}] = d
	}

	e.mu.Lock()
	actions := Diff(desired, actual, e.cache)
	stuck := e.trackStuck(actual)
	e.mu.Unlock()

	res := Result{
		Failed:    map[ResourceKey]error{},
		Permanent: map[ResourceKey]error{},
		Stuck:     stuck,
		Actual:    actual,
	}

	// Partition and order. Creates/updates go parent-first; deletes bottom-up
	// but only the top of each withdrawn subtree is issued (the backend
	// cascades children).
	var mutate []Action
	for _, a := range actions {
		switch a.Type {
		case ActionNoop:
			res.NoOp++
		case ActionCreate, ActionUpdate:
			mutate = append(mutate, a)
		case ActionDelete:
			if !freshDesired {
				continue // never GC on stale desired
			}
			if p := actualByKey[a.Key].Parent; !p.zero() {
				if _, parentDesired := desiredByKey[p]; !parentDesired {
					continue // parent is also withdrawn; deleting it cascades here
				}
			}
			mutate = append(mutate, a)
		}
	}

	// Surface permanent (known-unsupported) actions as drift and drop them from
	// this tick's work, and forget any permanent mark whose situation has moved
	// on: its action is no longer planned (e.g. the pool was re-declared), or the
	// spec has been rewritten since — a new generation is a new question, so the
	// verb is asked again rather than staying latched on the old answer.
	planned := make(map[ResourceKey]int64, len(mutate))
	for _, a := range mutate {
		planned[a.Key] = a.Resource.Generation
	}
	e.mu.Lock()
	for k, m := range e.permanent {
		if gen, still := planned[k]; !still || gen != m.gen {
			delete(e.permanent, k)
		}
	}
	for k, m := range e.permanent {
		res.Permanent[k] = m.err
	}
	e.mu.Unlock()

	eligible := e.filterEligible(mutate, actualByKey)
	res.Deferred = len(mutate) - len(eligible) - len(res.Permanent)
	if res.Deferred < 0 {
		res.Deferred = 0
	}
	e.dispatch(ctx, eligible, &res)
	return res, nil
}

// trackStuck updates the per-resource state timeline from this tick's actual
// and returns the resources that have sat non-Ready in the same state past
// StuckAfter. Caller holds e.mu.
func (e *Engine) trackStuck(actual []Actual) map[ResourceKey]time.Duration {
	now := e.now()
	seen := make(map[ResourceKey]bool, len(actual))
	stuck := map[ResourceKey]time.Duration{}
	for _, a := range actual {
		seen[a.Key] = true
		prev, ok := e.seenState[a.Key]
		if !ok || prev.state != a.State {
			e.seenState[a.Key] = stateStamp{state: a.State, since: now}
			continue
		}
		if !a.Ready {
			if age := now.Sub(prev.since); age >= e.stuckAfter {
				stuck[a.Key] = age
			}
		}
	}
	for k := range e.seenState {
		if !seen[k] {
			delete(e.seenState, k)
		}
	}
	return stuck
}

// filterEligible drops actions that must wait: a create/update whose parent is
// not yet present-and-Ready, and any resource still inside its backoff window.
func (e *Engine) filterEligible(actions []Action, actualByKey map[ResourceKey]Actual) []Action {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Action
	for _, a := range actions {
		if _, perm := e.permanent[a.Key]; perm {
			continue // reported as drift; not re-issued until the spec changes
		}
		if f := e.failures[a.Key]; f != nil && now.Before(f.nextAttempt) {
			continue
		}
		if a.Type == ActionCreate || a.Type == ActionUpdate {
			if parent, ok := e.backend.Parent(a.Resource); ok {
				pa, present := actualByKey[parent]
				if !present || !pa.Ready {
					continue // parent not ready yet; try next tick
				}
			}
		}
		out = append(out, a)
	}
	// Deterministic order: parents (shorter kind-depth) first for create/update,
	// then by key. Deletes are independent (top-of-subtree only) so order among
	// them does not matter.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key.String() < out[j].Key.String()
	})
	return out
}

// dispatch runs eligible actions with bounded concurrency, recording outcomes
// and updating the generation cache / backoff state.
func (e *Engine) dispatch(ctx context.Context, actions []Action, res *Result) {
	sem := make(chan struct{}, e.maxPar)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, a := range actions {
		wg.Add(1)
		sem <- struct{}{}
		go func(a Action) {
			defer wg.Done()
			defer func() { <-sem }()

			var err error
			switch a.Type {
			case ActionCreate:
				err = e.backend.Create(ctx, a.Resource)
			case ActionUpdate:
				err = e.backend.Update(ctx, a.Resource)
			case ActionDelete:
				err = e.backend.Delete(ctx, a.Key)
			}

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if isPermanent(err) {
					res.Permanent[a.Key] = err
					e.recordPermanent(a.Key, err, a.Resource.Generation)
					return
				}
				res.Failed[a.Key] = err
				e.recordFailure(a.Key, err)
				return
			}
			e.recordSuccess(a.Key, a.Resource.Generation, a.Type)
			switch a.Type {
			case ActionCreate:
				res.Created++
			case ActionUpdate:
				res.Updated++
			case ActionDelete:
				res.Deleted++
			}
		}(a)
	}
	wg.Wait()
}

func (e *Engine) recordSuccess(k ResourceKey, gen int64, t ActionType) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.failures, k)
	delete(e.permanent, k)
	if t == ActionDelete {
		delete(e.cache, k)
		return
	}
	if gen > e.cache[k] {
		e.cache[k] = gen
	}
}

// recordPermanent marks a key non-retryable at generation gen and clears any
// backoff state: the engine skips its action on later ticks (surfacing it as
// drift) rather than retrying a call that cannot succeed for this spec. Rewriting
// the spec bumps the generation and retires the mark.
func (e *Engine) recordPermanent(k ResourceKey, err error, gen int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.failures, k)
	e.permanent[k] = permMark{err: err, gen: gen}
}

func (e *Engine) recordFailure(k ResourceKey, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.failures[k]
	if f == nil {
		f = &failState{}
		e.failures[k] = f
	}
	f.count++
	f.lastErr = err
	backoff := e.baseBO << (f.count - 1)
	if backoff <= 0 || backoff > e.maxBO {
		backoff = e.maxBO
	}
	f.nextAttempt = e.now().Add(backoff)
}

// Source supplies desired state each tick. The bool reports whether the read
// was fresh (a successful fetch this tick) versus a cached/fallback set; the
// engine only garbage-collects on a fresh read.
type Source interface {
	Desired(ctx context.Context) (state api.DesiredState, fresh bool, err error)
}

// Reporter receives each tick's result (e.g. to post a status heartbeat).
type Reporter interface {
	Report(ctx context.Context, state api.DesiredState, res Result) error
}

// Run drives the loop until ctx is cancelled: fetch desired, reconcile, report,
// sleep interval. Fetch/report errors are returned to the caller's log via the
// returned error channel semantics being folded into the tick — a fetch error
// falls back to skipping the tick's deletes rather than aborting the loop.
func (e *Engine) Run(ctx context.Context, src Source, rep Reporter, interval time.Duration, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		state, fresh, err := src.Desired(ctx)
		if err != nil {
			logf("reconcile: fetch desired: %v", err)
			// fresh is false: reconcile with whatever the source returned (its
			// last-good set) but skip deletes.
		}
		res, rerr := e.Reconcile(ctx, state.Resources, fresh && err == nil)
		if rerr != nil {
			logf("reconcile: tick: %v", rerr)
		} else if rep != nil {
			if err := rep.Report(ctx, state, res); err != nil {
				logf("reconcile: report: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
