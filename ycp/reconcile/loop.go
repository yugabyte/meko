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
	onTick     func(Result, error, time.Duration)

	mu        sync.Mutex
	cache     map[ResourceKey]int64      // last-applied generation; fallback for a backend that cannot echo
	failures  map[ResourceKey]*failState // per-resource backoff
	permanent map[ResourceKey]permMark   // non-retryable; the reason picks the state
	updating  map[ResourceKey]updateMark // update issued, not yet confirmed applied
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
	state    string
	progress string
	since    time.Time
}

// updateMark records that a resource's update for generation gen is in flight in
// the backend, and since when. The timestamp is what lets work that never
// terminates — a wedged worker whose task stays `running` forever, the one case
// reading the task's status cannot resolve — be reported as stuck rather than
// pending indefinitely. A new generation restarts the clock.
type updateMark struct {
	gen   int64
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
	// OnTick, when set, is called by Run after every Reconcile with its result,
	// its error and how long it took — an observer for the caller's metrics, so
	// this package carries no metrics dependency of its own.
	OnTick func(res Result, err error, took time.Duration)
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
		onTick:     opts.OnTick,
		cache:      map[ResourceKey]int64{},
		failures:   map[ResourceKey]*failState{},
		permanent:  map[ResourceKey]permMark{},
		updating:   map[ResourceKey]updateMark{},
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
	// Failing maps every resource whose last attempt failed and has not succeeded
	// since to that error: Failed plus those waiting out their backoff, which this
	// tick did not retry. Failed alone sees a resource only on the ticks its retry
	// lands on, so it cannot say how many are failing.
	Failing map[ResourceKey]error
	// Unreadable maps a resource the backend could not read this tick
	// (Actual.DiscoveryError) to why. Nothing was issued for it — not a create,
	// which would duplicate whatever is really there; not an update, which would
	// be written without knowing the current state; and not a delete, even for one
	// desired state has withdrawn, since the row is no evidence it is still there.
	//
	// Reported separately from Failed because nothing was attempted: the
	// environment was discovered in part, and these are the parts missing from
	// that reading. A tick with any of these has not converged the environment,
	// whatever it managed to do to the rest.
	Unreadable map[ResourceKey]error
	// Permanent maps a resource to a non-retryable error (a verb the kind does
	// not support, a declaration the backend refused, a call it is not permitted).
	// The engine does not re-issue these until the resource's spec changes, and
	// unlike Failed they do not by themselves make the environment DEGRADED. Which
	// state each is reported in comes off the error — see PermanentReason.
	Permanent map[ResourceKey]error
	// Updating maps a resource whose update the backend has issued but cannot yet
	// confirm — it just planned the work, or the work it planned earlier is still
	// running. Neither applied nor failed: it does not back off, does not degrade
	// the environment, and is reported so an operator sees why a generation has not
	// landed yet.
	Updating map[ResourceKey]error
	// Stuck maps a resource making no progress to how long it has been that way:
	// one that has sat non-Ready in the same backend state past StuckAfter, or one
	// whose update has been Updating that long (a task that never terminates).
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
	if r, ok := e.backend.(Reconsiderer); ok {
		for _, k := range r.Reconsider() {
			delete(e.permanent, k)
		}
	}
	actions := Diff(desired, actual, e.cache)
	// Which updates this tick still wants, so trackStuck can drop the in-flight
	// mark of one that has since converged (the echo caught up, so no update is
	// planned and dispatch will never clear the mark itself) or whose spec has been
	// rewritten (a new generation is a new wait).
	plannedUpdates := map[ResourceKey]int64{}
	for _, a := range actions {
		if a.Type == ActionUpdate {
			plannedUpdates[a.Key] = a.Resource.Generation
		}
	}
	stuck := e.trackStuck(actual, plannedUpdates, desiredByKey)
	e.mu.Unlock()

	res := Result{
		Failed:     map[ResourceKey]error{},
		Failing:    map[ResourceKey]error{},
		Permanent:  map[ResourceKey]error{},
		Updating:   map[ResourceKey]error{},
		Unreadable: map[ResourceKey]error{},
		Stuck:      stuck,
		Actual:     actual,
	}

	// Partition and order. Creates/updates go parent-first; deletes bottom-up
	// but only the top of each withdrawn subtree is issued (the backend
	// cascades children).
	var mutate []Action
	for _, a := range actions {
		switch a.Type {
		case ActionNoop:
			res.NoOp++
		case ActionUnreadable:
			res.Unreadable[a.Key] = actualByKey[a.Key].DiscoveryError
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

	// Surface permanent actions on the resource and drop them from this tick's
	// work, and forget any permanent mark whose situation has moved
	// on: its action is no longer planned (e.g. the pool was re-declared), or the
	// spec has been rewritten since — a new generation is a new question, so the
	// verb is asked again rather than staying latched on the old answer.
	planned := make(map[ResourceKey]int64, len(mutate))
	for _, a := range mutate {
		planned[a.Key] = a.Resource.Generation
	}
	// An unreadable resource has nothing planned, but it has not converged either
	// — it was never looked at. All three prunes below read "nothing planned" as
	// "done with", so each has to hold these keys out or an unread tick silently
	// resets state that is still live:
	//
	//   - failures: the outage producing these rows is the outage that just failed
	//     the in-flight verbs, so wiping the ladder pins a repeatedly-restarting
	//     remote at rung 1 and re-issues its work at full rate (meko#985).
	//   - updating: dropping the mark restarts the wedged-task clock, so a task
	//     that never terminates can never reach StuckAfter if an unread tick lands
	//     in between.
	//   - permanent: the mark would retire without the spec's generation changing,
	//     against permMark's contract, so a refused declaration is re-issued on the
	//     next readable tick.
	unread := func(k ResourceKey) bool { _, ok := res.Unreadable[k]; return ok }
	e.mu.Lock()
	for k, m := range e.permanent {
		if unread(k) {
			continue
		}
		if gen, still := planned[k]; !still || gen != m.gen {
			delete(e.permanent, k)
		}
	}
	for k, m := range e.permanent {
		res.Permanent[k] = m.err
	}
	// A resource with nothing planned has converged (or is gone), so its backoff
	// ladder and in-flight mark are spent. Dropping them here is what clears them on
	// an async path at all: the success signal there is the echo advancing, which
	// makes the resource a no-op and so never reaches dispatch. Left behind, a stale
	// ladder would delay the resource's NEXT update by up to MaxBackoff.
	for k := range e.failures {
		if unread(k) {
			continue
		}
		if _, still := planned[k]; !still {
			delete(e.failures, k)
		}
	}
	for k := range e.updating {
		if unread(k) {
			continue
		}
		if _, still := planned[k]; !still {
			delete(e.updating, k)
		}
	}
	e.mu.Unlock()

	eligible := e.filterEligible(mutate, actualByKey)
	res.Deferred = len(mutate) - len(eligible) - len(res.Permanent)
	if res.Deferred < 0 {
		res.Deferred = 0
	}
	e.dispatch(ctx, eligible, &res)
	e.mu.Lock()
	for k, f := range e.failures {
		res.Failing[k] = f.lastErr
	}
	e.mu.Unlock()
	return res, nil
}

// trackStuck updates the per-resource state timeline from this tick's actual and
// returns everything making no progress past StuckAfter: a resource sitting
// non-Ready in the same backend state, and one whose update has been in flight
// that long. plannedUpdates is this tick's wanted updates, used to retire the
// in-flight mark of a resource that has converged or had its spec rewritten.
// Caller holds e.mu.
func (e *Engine) trackStuck(actual []Actual, plannedUpdates map[ResourceKey]int64,
	desired map[ResourceKey]api.DesiredResource) map[ResourceKey]time.Duration {
	now := e.now()
	seen := make(map[ResourceKey]bool, len(actual))
	progressed := map[ResourceKey]bool{}
	stuck := map[ResourceKey]time.Duration{}
	for _, a := range actual {
		seen[a.Key] = true
		if a.DiscoveryError != nil {
			// Unread, so its State says nothing to time. Every such row — declared or
			// withdrawn — is reported through Result.Unreadable instead, which names
			// the right cause: "could not be read" rather than "has not progressed".
			continue
		}
		if _, want := desired[a.Key]; a.Preexisting && !want {
			// Reported but not managed: nobody asked YCP for this resource's state, so
			// there is no progress it owes anyone and a timer on it is a false alarm.
			// A pre-existing resource that IS declared stays timed — that one is
			// converging, or failing to.
			continue
		}
		prev, ok := e.seenState[a.Key]
		if !ok || prev.state != a.State || prev.progress != a.Progress {
			e.seenState[a.Key] = stateStamp{state: a.State, progress: a.Progress, since: now}
			if ok && prev.progress != a.Progress {
				progressed[a.Key] = true
			}
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
	// An update the backend keeps reporting in flight is progress as far as its
	// task's status goes, but past StuckAfter it is a wedged worker, not a slow one
	// — the one no-progress case reading the task cannot detect. Reported through
	// the same Stuck channel, so it degrades the environment with a reason instead
	// of sitting "updating" forever.
	for k, m := range e.updating {
		gen, planned := plannedUpdates[k]
		if !seen[k] || !planned || gen != m.gen {
			delete(e.updating, k)
			continue
		}
		if progressed[k] {
			m.since = now
			e.updating[k] = m
			continue
		}
		if age := now.Sub(m.since); age >= e.stuckAfter {
			stuck[k] = age
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
			continue // reported on the resource; not re-issued until the spec changes
		}
		if f := e.failures[a.Key]; f != nil && now.Before(f.nextAttempt) {
			continue
		}
		if a.Type == ActionCreate || a.Type == ActionUpdate {
			if parent, ok := e.backend.Parent(a.Resource); ok {
				pa, present := actualByKey[parent]
				if !present || !pa.Ready || pa.DiscoveryError != nil {
					// Unread counts as not ready: a parent nobody could read cannot be
					// said to be healthy, whatever Ready carries on a row like that.
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
				if isPending(err) {
					// Neither applied nor failed: the backend has work in flight for this
					// resource. Record nothing that pretends otherwise — no cache write
					// (nothing has landed) and no backoff (nothing failed) — and let the
					// next tick ask the backend again.
					res.Updating[a.Key] = err
					e.recordUpdating(a.Key, a.Resource.Generation)
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
	delete(e.updating, k)
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
	delete(e.updating, k)
	e.permanent[k] = permMark{err: err, gen: gen}
}

// recordUpdating stamps a resource's in-flight update, keeping the existing
// timestamp while the generation is unchanged so trackStuck measures the whole
// wait rather than restarting it each tick.
//
// It deliberately does NOT clear the backoff ladder. On an async path every
// failure is preceded by a Pending — the tick that issues reports pending, and only
// a later tick reads the task's failure — so clearing here would reset the count
// on every attempt and pin the backoff at its first rung forever. A resize the
// controller can never satisfy would then plan a task every BaseBackoff, and each
// add_branch_extensions task redeploys each endpoint on its branch that is behind. The ladder is
// cleared by success, by a permanent mark, and by the resource going quiet (see
// Reconcile's pruning).
func (e *Engine) recordUpdating(k ResourceKey, gen int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.updating[k]; ok && m.gen == gen {
		return
	}
	e.updating[k] = updateMark{gen: gen, since: e.now()}
}

func (e *Engine) recordFailure(k ResourceKey, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.updating, k)
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
		start := e.now()
		res, rerr := e.Reconcile(ctx, state.Resources, fresh && err == nil)
		if e.onTick != nil {
			e.onTick(res, rerr, e.now().Sub(start))
		}
		if rerr != nil {
			logf("reconcile: tick: %v", rerr)
		} else {
			// Outside the reporter branch: a run without one still has an operator
			// reading its log, and this is the tick saying it converged only part of
			// the environment. The cause also reaches the inventory as the resource's
			// own error, so this is the local copy rather than the only record.
			for k, uerr := range res.Unreadable {
				logf("reconcile: %s could not be read, so nothing was issued for it: %v", k, uerr)
			}
			// The other two conditions that degrade the environment. Without these
			// the metric says one resource is stuck and nothing says which
			// (CLOUDGA-37501).
			for k, d := range res.Stuck {
				logf("reconcile: %s is stuck: no progress for %s", k, d.Round(time.Second))
			}
			for k, ferr := range res.Failed {
				logf("reconcile: %s failed: %v", k, ferr)
			}
		}
		if rerr == nil && rep != nil {
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
