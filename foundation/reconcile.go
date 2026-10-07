package foundation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reconcile loop defaults.
const (
	// DefaultReconcilePoll is how often the object is re-read. It doubles as
	// the debounce: a burst of edits inside one interval converges once, from
	// the last of them.
	DefaultReconcilePoll = 30 * time.Second
	// DefaultMinBackoff and DefaultMaxBackoff bound the wait after a FAILED
	// converge, measured from when that converge ended rather than from when it
	// started — a converge that fails forty minutes in has already waited.
	DefaultMinBackoff = time.Minute
	DefaultMaxBackoff = 30 * time.Minute
	// DefaultTeardownGrace bounds how long a Delete-policy teardown is retried
	// before the finalizer is released anyway. See Reconciler.finalize.
	DefaultTeardownGrace = time.Hour
)

// Reconciler converges a cluster onto one Foundation object and keeps that
// object's status current.
//
// It is level-triggered on metadata.generation and re-reads the object on an
// interval rather than holding a watch: there is no stream to lose, no resync
// to arrange, and the interval is the debounce. The cost is noticing an edit up
// to one interval late, which is nothing against a bring-up measured in tens of
// minutes.
//
// A converge runs in the background while the loop keeps reading and reporting,
// which is what makes the status continuous instead of one line every
// three-quarters of an hour. Only one runs at a time.
type Reconciler struct {
	cr  *crClient
	log func(format string, args ...any)

	poll          time.Duration
	minBackoff    time.Duration
	maxBackoff    time.Duration
	resync        time.Duration
	teardownGrace time.Duration

	// deployer builds the converger a pass runs through. The channel is the
	// interrupt; a nil one is for the read-only uses.
	deployer func(*Spec, <-chan struct{}) converger
	now      func() time.Time

	// State carried between ticks. All of it belongs to ONE object, identified
	// by uid — see rebind.
	uid         string
	active      *convergeRun
	lastGen     int64
	failures    int
	nextAttempt time.Time
	lastSuccess time.Time
	lastErr     error
	published   string
	// lastEvent is the condition an event was last emitted for, so a steady
	// state stays silent. See emitOnChange.
	lastEvent string
}

// converger is what the loop needs from a Deployer.
//
// An interface because the loop's own decisions — when to start one, when to
// back off, when to release the finalizer — are worth testing on their own,
// and behind a concrete Deployer every one of them needs a cluster's worth of
// canned kubectl output to reach.
type converger interface {
	Apply(context.Context) (*Info, error)
	Status(context.Context) (*Status, error)
	PublishConn(context.Context, string, *Info) error
	Down(context.Context, DownOptions) error
}

// convergeRun is one converge in flight.
type convergeRun struct {
	// uid of the object it was started for, so its result is discarded rather
	// than attributed to a replacement that arrived while it ran.
	uid        string
	generation int64
	interrupt  chan struct{}
	done       chan struct{}
	err        error
	stopped    bool
}

// stop signals the converge that its spec is stale. It is not a cancel: the
// step in flight finishes if it writes. See Deployer.WithInterrupt.
func (c *convergeRun) stop() {
	if !c.stopped {
		c.stopped = true
		close(c.interrupt)
	}
}

// NewReconciler builds the loop for the Foundation named name in namespace.
// kubeContext is empty in a pod, where there is no kubeconfig to name one.
func NewReconciler(run Runner, namespace, name, kubeContext string) *Reconciler {
	if run == nil {
		run = Exec
	}
	r := &Reconciler{
		cr:            &crClient{run: run, namespace: namespace, name: name, kubeContext: kubeContext},
		log:           func(string, ...any) {},
		poll:          DefaultReconcilePoll,
		minBackoff:    DefaultMinBackoff,
		maxBackoff:    DefaultMaxBackoff,
		teardownGrace: DefaultTeardownGrace,
		now:           time.Now,
	}
	r.deployer = func(spec *Spec, interrupt <-chan struct{}) converger {
		// The pod waits for YBA and nothing else, which is the whole design:
		// the loop re-reads every CR's status each poll and reports it, so
		// every wait inside a converge buys a slower converge and no more
		// knowledge. Prune still runs — it does not need the universe settled.
		return New(spec, run).
			WithoutWaits().
			WithLogger(r.log).
			WithInterrupt(interrupt)
	}
	return r
}

// WithLogger sends progress to log.
func (r *Reconciler) WithLogger(log func(format string, args ...any)) *Reconciler {
	r.log = log
	return r
}

// WithIntervals overrides the loop's timings. Zero leaves a default in place,
// except resync, where zero is the default and means a settled foundation is
// never re-converged on its own.
func (r *Reconciler) WithIntervals(poll, minBackoff, maxBackoff, resync time.Duration) *Reconciler {
	if poll > 0 {
		r.poll = poll
	}
	if minBackoff > 0 {
		r.minBackoff = minBackoff
	}
	if maxBackoff > 0 {
		r.maxBackoff = maxBackoff
	}
	r.resync = resync
	return r
}

// Run reconciles until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	r.log("reconciling %s %s in %s every %s", FoundationKind, r.cr.name, r.cr.namespace, r.poll)
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			r.drain()
			return ctx.Err()
		case <-time.After(r.poll):
		}
	}
}

// drain lets a converge in flight finish instead of killing it, because the
// steps that write are exactly the ones that must not be cut off: `helm upgrade
// --install` killed partway leaves the release in pending-install. What remains
// is bounded by the pod's terminationGracePeriodSeconds, and the SIGKILL at the
// end of it is the wedge this is avoiding — so that grace period is sized for a
// helm install, not left at the 30-second default.
func (r *Reconciler) drain() {
	if r.active == nil {
		return
	}
	r.log("shutting down: letting the converge of generation %d finish", r.active.generation)
	r.active.stop()
	<-r.active.done
	r.active = nil
}

// tick is one pass: read the object, decide, report.
func (r *Reconciler) tick(ctx context.Context) {
	cr, err := r.cr.get(ctx)
	if err != nil {
		r.log("read the Foundation: %v", err)
		return
	}
	if cr == nil {
		r.log("no %s named %q in %s yet", FoundationKind, r.cr.name, r.cr.namespace)
		return
	}

	r.rebind(cr)

	spec, specErr := loadCRSpec(cr)

	if cr.Metadata.DeletionTimestamp != "" {
		r.finalize(ctx, cr, spec)
		return
	}

	r.reap()
	if specErr != nil {
		// Nothing to converge and nothing to retry: only an edit fixes a spec
		// the loader rejected, and an edit bumps the generation, which starts
		// one by itself.
		r.reportSpecError(ctx, cr, specErr)
		return
	}
	r.ensureFinalizer(ctx, cr, spec)
	r.supersede(cr)
	r.start(ctx, cr, spec)
	r.report(ctx, cr, spec)
}

// rebind drops every piece of state belonging to a previous object.
//
// `kubectl delete foundation` under the default Retain policy removes the
// object and leaves the universe. Re-applying the same file then produces a NEW
// object at generation 1 — and the loop's state is all about the old one:
// lastGen is whatever the old object reached, so a re-apply at generation 1 can
// read as already converged and never start; a backoff inherited from the
// deleted object delays the new one; and published suppresses the conn
// ConfigMap write. The uid is the only thing that distinguishes the two, since
// name, namespace and generation can all repeat.
func (r *Reconciler) rebind(cr *FoundationCR) {
	uid := cr.Metadata.UID
	if uid == r.uid {
		return
	}
	if r.uid != "" {
		r.log("%s %s was replaced (uid %s -> %s); starting over", FoundationKind, r.cr.name, r.uid, uid)
		// Its converge is converging a spec nothing is reading any more. Let it
		// finish the step it is in — reap discards the result by uid — but stop
		// it from running on, so the replacement is not held behind it.
		if r.active != nil {
			r.active.stop()
		}
	}
	r.uid = uid
	r.lastGen, r.failures, r.lastErr = 0, 0, nil
	r.nextAttempt, r.lastSuccess = time.Time{}, time.Time{}
	r.published, r.lastEvent = "", ""
}

// reap collects a converge that has finished and sets the backoff from it.
func (r *Reconciler) reap() {
	if r.active == nil {
		return
	}
	select {
	case <-r.active.done:
	default:
		return
	}
	run := r.active
	r.active = nil

	if run.uid != r.uid {
		r.log("converge of generation %d belonged to the previous object; discarding its result", run.generation)
		return
	}

	switch {
	case errors.Is(run.err, ErrInterrupted):
		// Neither progress nor failure. lastGen is deliberately NOT advanced,
		// so the next tick starts a converge on the generation that superseded
		// this one, with no backoff to sit through.
		r.log("converge of generation %d was superseded", run.generation)
	case run.err != nil:
		r.failures++
		r.lastGen, r.lastErr = run.generation, run.err
		wait := r.backoff()
		r.nextAttempt = r.now().Add(wait)
		r.log("converge of generation %d failed, retrying in %s: %v", run.generation, wait, run.err)
	default:
		r.failures, r.lastErr = 0, nil
		r.lastGen, r.lastSuccess = run.generation, r.now()
		r.nextAttempt = time.Time{}
		if r.resync > 0 {
			r.nextAttempt = r.lastSuccess.Add(r.resync)
		}
		r.log("converge of generation %d succeeded", run.generation)
	}
}

// backoff is exponential in the number of consecutive failures, to a ceiling.
// There is no attempt limit: every converge is create-or-apply, so retrying
// forever costs a poll's worth of kubectl and is the only behaviour that
// recovers from a cause that clears on its own.
func (r *Reconciler) backoff() time.Duration {
	wait := r.minBackoff
	for i := 1; i < r.failures; i++ {
		wait *= 2
		if wait >= r.maxBackoff {
			return r.maxBackoff
		}
	}
	return wait
}

// supersede tells a converge still running that the spec under it has changed.
func (r *Reconciler) supersede(cr *FoundationCR) {
	if r.active == nil || r.active.generation == cr.Metadata.Generation {
		return
	}
	r.log("generation %d supersedes the converge of %d", cr.Metadata.Generation, r.active.generation)
	r.active.stop()
}

// start begins a converge if one is due.
func (r *Reconciler) start(ctx context.Context, cr *FoundationCR, spec *Spec) {
	if r.active != nil {
		return
	}
	gen := cr.Metadata.Generation
	switch {
	case gen != r.lastGen:
		// A new generation clears the backoff. The edit is the user's answer to
		// whatever the last converge failed on, so making them wait out a
		// half-hour backoff to find out whether it worked would be its own bug.
		r.failures = 0
	case r.nextAttempt.IsZero() || r.now().Before(r.nextAttempt):
		return
	}

	run := &convergeRun{uid: r.uid, generation: gen, interrupt: make(chan struct{}), done: make(chan struct{})}
	r.active = run
	d := r.deployer(spec, run.interrupt)
	go func() {
		defer close(run.done)
		// NOT the tick's context. A converge has to outlive a SIGTERM for long
		// enough to finish the step that writes; drain() is what bounds it, and
		// the interrupt is how it is told to stop early.
		_, run.err = d.Apply(context.WithoutCancel(ctx))
	}()
	r.log("converging generation %d", gen)
}

// report reads the cluster and writes what it found to the object's status.
func (r *Reconciler) report(ctx context.Context, cr *FoundationCR, spec *Spec) {
	st, err := r.deployer(spec, nil).Status(ctx)
	if err != nil {
		r.log("read the status: %v", err)
		return
	}
	// The address goes to the ConfigMap as well as the status, because a
	// consumer mounts it with configMapKeyRef and there is no equivalent for a
	// custom resource. Only on change: the apply is idempotent, but writing it
	// every poll makes the object's history useless.
	if st.Address != nil && st.Address.YSQLHostPorts != r.published {
		if err := r.deployer(spec, nil).PublishConn(ctx, spec.Namespace, st.Address); err != nil {
			r.log("publish the address: %v", err)
		} else {
			r.published = st.Address.YSQLHostPorts
			r.log("published %s to the %s ConfigMap", st.Address.YSQLHostPorts, ConnConfigMap)
		}
	}
	r.write(ctx, cr, st, r.convergedCondition(cr))
}

// reportSpecError publishes a spec the loader refused. There is no cluster
// reading to do — the spec is what says which objects to read — so this reports
// the parse failure and nothing else.
func (r *Reconciler) reportSpecError(ctx context.Context, cr *FoundationCR, err error) {
	r.log("spec is invalid: %v", err)
	r.write(ctx, cr, &Status{Namespace: cr.Metadata.Namespace, Phase: PhaseFailed}, Condition{
		Type:    ConditionConverged,
		Status:  CondFalse,
		Reason:  "SpecInvalid",
		Message: err.Error(),
	})
}

// convergedCondition says what became of the last converge. It is separate from
// Ready because they answer different questions: Ready is the cluster's state,
// this is what this loop last managed to do to it.
func (r *Reconciler) convergedCondition(cr *FoundationCR) Condition {
	c := Condition{Type: ConditionConverged, ObservedGeneration: r.lastGen}
	switch {
	case r.active != nil:
		c.Status, c.Reason = CondFalse, "Converging"
		c.Message = fmt.Sprintf("converging generation %d", r.active.generation)
	case r.lastErr != nil:
		c.Status, c.Reason = CondFalse, "ConvergeFailed"
		c.Message = r.lastErr.Error()
	case r.lastGen == 0:
		c.Status, c.Reason = CondUnknown, "NeverConverged"
	case r.lastGen != cr.Metadata.Generation:
		c.Status, c.Reason = CondFalse, "Pending"
		c.Message = fmt.Sprintf("generation %d has not been converged yet", cr.Metadata.Generation)
	default:
		c.Status, c.Reason = CondTrue, "Converged"
	}
	return c
}

// write assembles the status and patches it.
func (r *Reconciler) write(ctx context.Context, cr *FoundationCR, st *Status, converged Condition) {
	now := r.now().UTC().Format(time.RFC3339)
	ready := Condition{
		Type: ConditionReady, Status: CondFalse, Reason: string(st.Phase),
		ObservedGeneration: r.lastGen,
	}
	if st.Phase == PhaseReady {
		ready.Status = CondTrue
	}

	// observedGeneration is the generation this status DESCRIBES, which is the
	// one last converged and not the one on the object: a reader comparing the
	// two is asking exactly whether their edit has been acted on yet.
	body := map[string]any{
		"observedGeneration": r.lastGen,
		"namespace":          st.Namespace,
		"phase":              st.Phase,
		"yba":                st.YBA,
		"resources":          st.Resources,
		"resourceStates":     st.ResourceStates,
		"heartbeat":          now,
		"conditions":         mergeConditions(cr.Status.Conditions, []Condition{ready, converged}, now),
	}
	if st.Address != nil {
		body["address"] = st.Address
	}
	if !r.lastSuccess.IsZero() {
		body["lastSuccess"] = r.lastSuccess.UTC().Format(time.RFC3339)
	}
	if err := r.cr.writeStatus(ctx, body); err != nil {
		r.log("write the status: %v", err)
	}
	// After the status, not before: an event announcing a state the object does
	// not yet show sends a reader to a status that contradicts it.
	r.emitOnChange(ctx, cr, converged)
}

// mergeConditions carries lastTransitionTime over from the conditions already
// on the object, so it marks when the condition last CHANGED rather than when
// it was last written — which, at one write per poll, would make it useless.
func mergeConditions(old, want []Condition, now string) []Condition {
	previous := map[string]Condition{}
	for _, c := range old {
		previous[c.Type] = c
	}
	out := make([]Condition, 0, len(want))
	for _, c := range want {
		if prev, ok := previous[c.Type]; ok && prev.Status == c.Status && prev.LastTransitionTime != "" {
			c.LastTransitionTime = prev.LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		out = append(out, c)
	}
	return out
}

// ensureFinalizer adds or removes the finalizer to match the spec's policy.
//
// It tracks the policy rather than being set once, which is the escape hatch
// for a foundation whose teardown cannot finish: flipping deletionPolicy to
// Retain drops the finalizer on the next pass and lets the object go.
func (r *Reconciler) ensureFinalizer(ctx context.Context, cr *FoundationCR, spec *Spec) {
	want := spec.DeletionPolicy == DeletionDelete
	if want == cr.hasFinalizer() {
		return
	}
	list := cr.Metadata.Finalizers
	if want {
		list = append(list, FoundationFinalizer)
	} else {
		list = without(list, FoundationFinalizer)
	}
	if err := r.cr.setFinalizers(ctx, list); err != nil {
		r.log("update the finalizer: %v", err)
		return
	}
	r.log("deletionPolicy is %s: finalizer %s", spec.DeletionPolicy,
		map[bool]string{true: "added", false: "removed"}[want])
}

// finalize handles a Foundation being deleted.
//
// With the default Retain policy there is no finalizer and this never runs —
// the object goes and the universe keeps running, recoverable by applying the
// same spec again. With Delete, the universe is torn down first.
//
// The teardown is retried, but not forever: past teardownGrace the finalizer is
// released anyway, with what was left behind in the log. A finalizer that never
// resolves is worse than an orphaned universe, because the only thing that can
// clear it is this pod — and a namespace deletion takes the pod out at the same
// time as the object, leaving the namespace in Terminating permanently, where
// `kubectl delete` is a no-op.
func (r *Reconciler) finalize(ctx context.Context, cr *FoundationCR, spec *Spec) {
	if !cr.hasFinalizer() {
		return
	}
	// A converge writing to a foundation being deleted is pointless at best.
	r.drain()

	if spec == nil || spec.DeletionPolicy != DeletionDelete {
		// Either the spec no longer asks for a teardown, or it can no longer be
		// read — and refusing to release the object over a spec nobody can
		// parse would wedge it with no way out.
		r.release(ctx, cr, "deletionPolicy is not Delete")
		return
	}

	r.log("deletionPolicy is Delete: tearing down %q", spec.UniverseName())
	r.emit(ctx, cr, eventTypeNormal, EventReasonTearingDown,
		"deletionPolicy is Delete: tearing down "+spec.UniverseName())
	err := r.deployer(spec, nil).Down(ctx, DownOptions{})
	if err == nil {
		r.emit(ctx, cr, eventTypeNormal, EventReasonTornDown, "the universe was torn down")
		r.release(ctx, cr, "teardown finished")
		return
	}
	r.log("teardown failed: %v", err)
	if r.expired(cr) {
		left := fmt.Sprintf("teardown kept failing for over %s; releasing the object anyway. "+
			"What is left of the universe, the yugaware release and its cluster RBAC is still "+
			"in %s and has to be removed by hand", r.teardownGrace, spec.Namespace)
		r.log("%s", left)
		// A Warning, and the last thing said about this object before it goes:
		// the conditions disappear with it, so without this the fact that
		// something was orphaned survives nowhere a reader will look.
		r.emit(ctx, cr, eventTypeWarning, EventReasonTeardownKept, left)
		r.release(ctx, cr, "teardown gave up")
	}
}

// expired reports whether the teardown grace has run out. An unparseable
// deletionTimestamp counts as expired: the alternative is holding the object
// open forever on a timestamp nobody can read.
func (r *Reconciler) expired(cr *FoundationCR) bool {
	started, err := time.Parse(time.RFC3339, cr.Metadata.DeletionTimestamp)
	if err != nil {
		return true
	}
	return r.now().Sub(started) > r.teardownGrace
}

func (r *Reconciler) release(ctx context.Context, cr *FoundationCR, why string) {
	if err := r.cr.setFinalizers(ctx, without(cr.Metadata.Finalizers, FoundationFinalizer)); err != nil {
		r.log("release the finalizer: %v", err)
		return
	}
	r.log("released the finalizer: %s", why)
}

func without(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
