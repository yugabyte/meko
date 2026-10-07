package foundation

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// realLoop wires a Reconciler to the REAL Deployer rather than the fake
// converger, so a failure travels the whole way it does in a cluster: kubectl's
// stderr -> applyCRs -> IncompleteError -> the loop -> the object's conditions.
// The fake converger cannot show that, because it never runs a step.
func realLoop(t *testing.T, run Runner) *Reconciler {
	t.Helper()
	r := NewReconciler(run, "amp", "foundation", "").WithLogger(func(string, ...any) {})
	r.deployer = func(spec *Spec, interrupt <-chan struct{}) converger {
		return New(spec, run).
			WithTimeouts(time.Second, 5*time.Second, time.Millisecond).
			WithLogger(func(string, ...any) {}).
			WithInterrupt(interrupt)
	}
	return r
}

// The API server's own words when a CR carries a field its schema does not
// declare. This is what `kubectl apply --server-side` writes to stderr, and the
// exact failure CI provokes by patching numNodez into the universe.
const undeclaredFieldStderr = `error: failed to create typed patch object ` +
	`(amp/foundation; operator.yugabyte.io/v1alpha1, Kind=YBUniverse): ` +
	`.spec.numNodez: field not declared in schema`

// TestARejectedEditIsReportedAndTheFixConverges is the loop's whole job in one
// test: a bad spec is refused without damage, said out loud on the object, and
// the correction goes through.
//
// Every assertion here mirrors one the GKE lane makes against a real cluster.
// This runs in a second and on every PR; that one needs GKE and 45 minutes.
func TestARejectedEditIsReportedAndTheFixConverges(t *testing.T) {
	var mu sync.Mutex
	rejectUniverse := true // flipped when the operator "fixes" the spec
	generation := int64(1)

	rec := &Recorder{Responses: append(convergeResponses(Result{Stdout: readyStatus()}),
		liveClusterResponses()...)}
	run := func(ctx context.Context, stdin []byte, argv []string) (Result, error) {
		joined := strings.Join(argv, " ")
		mu.Lock()
		reject, gen := rejectUniverse, generation
		mu.Unlock()

		// The object itself, at whichever generation the test is at.
		if strings.Contains(joined, "get "+FoundationKind) {
			return Result{Stdout: crObject(t, gen, nil)}, nil
		}
		// Only the universe is refused. The certificate and provider ahead of
		// it apply fine, which is what makes this harmless: nothing that
		// already exists is touched.
		if reject && strings.Contains(joined, "apply") && strings.Contains(string(stdin), "YBUniverse") {
			return Result{ExitCode: 1, Stderr: undeclaredFieldStderr}, nil
		}
		return rec.Run(ctx, stdin, argv)
	}

	r := realLoop(t, run)
	r.tick(context.Background()) // starts the converge
	r.settle(t)
	r.tick(context.Background()) // reaps it and writes the status

	status := lastStatusPatch(t, rec)
	if got := conditionReason(status, ConditionConverged); got != "ConvergeFailed" {
		t.Fatalf("Converged reason = %q, want ConvergeFailed\n%v", got, status["conditions"])
	}
	// The field the API server named has to survive the trip. Without it the
	// object says a converge failed and not which key to fix, and the reason is
	// only in a pod log.
	msg := conditionMessage(status, ConditionConverged)
	if !strings.Contains(msg, "numNodez") {
		t.Errorf("the condition does not name the refused field:\n%s", msg)
	}
	if !strings.Contains(msg, "apply the custom resources") {
		t.Errorf("the condition does not name the step that failed:\n%s", msg)
	}
	// A rejected edit must not take the address away from a consumer already
	// using it: nothing was applied, so nothing changed.
	if addr, ok := status["address"].(map[string]any); !ok || addr["ysql_host_ports"] == "" {
		t.Errorf("the address went away under a rejected edit: %v", status["address"])
	}
	// Reported, and retried — no attempt limit, because the fix is an edit and
	// the loop cannot know when it will come.
	if r.failures == 0 {
		t.Error("the failure was not counted, so nothing will retry it")
	}

	// --- the operator fixes the spec ------------------------------------
	mu.Lock()
	rejectUniverse = false
	generation = 2
	mu.Unlock()

	// A new generation clears the backoff: this must converge now, not after
	// the retry wait the failure just started.
	before := r.nextAttempt
	if before.IsZero() {
		t.Fatal("no backoff was set by the failure, so clearing it proves nothing")
	}
	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background())

	status = lastStatusPatch(t, rec)
	if got := conditionReason(status, ConditionConverged); got != "Converged" {
		t.Fatalf("after the fix, Converged reason = %q\n%v", got, status["conditions"])
	}
	if got := status["observedGeneration"]; got != float64(2) {
		t.Errorf("observedGeneration = %v, want 2 — a reader cannot otherwise tell the edit "+
			"was acted on", got)
	}
	if r.failures != 0 {
		t.Errorf("failures = %d after a successful converge, want the count cleared", r.failures)
	}
}

// A failure that persists keeps being retried, and the wait grows. The loop has
// no attempt limit on purpose — the cause is usually an edit that has not
// arrived yet — so what bounds it is the ceiling, not a count.
func TestAPersistentRejectionKeepsRetryingOnAGrowingBackoff(t *testing.T) {
	rec := &Recorder{Responses: append(convergeResponses(Result{Stdout: readyStatus()}),
		liveClusterResponses()...)}
	run := func(ctx context.Context, stdin []byte, argv []string) (Result, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "get "+FoundationKind) {
			return Result{Stdout: crObject(t, 1, nil)}, nil
		}
		if strings.Contains(joined, "apply") && strings.Contains(string(stdin), "YBUniverse") {
			return Result{ExitCode: 1, Stderr: undeclaredFieldStderr}, nil
		}
		return rec.Run(ctx, stdin, argv)
	}

	now := time.Now()
	r := realLoop(t, run)
	r.now = func() time.Time { return now }

	var waits []time.Duration
	for range 3 {
		r.tick(context.Background())
		r.settle(t)
		r.tick(context.Background()) // reap, which sets the next wait
		waits = append(waits, r.nextAttempt.Sub(now))
		now = now.Add(r.nextAttempt.Sub(now) + time.Second) // sit out the backoff
	}
	if r.failures != 3 {
		t.Fatalf("failures = %d after three rejected converges, want 3", r.failures)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Errorf("wait %d (%s) did not grow on the one before (%s): %v",
				i, waits[i], waits[i-1], waits)
		}
	}
}

// conditionMessage reads one condition's message out of a status patch.
func conditionMessage(status map[string]any, typ string) string {
	list, _ := status["conditions"].([]any)
	for _, c := range list {
		m, _ := c.(map[string]any)
		if m["type"] == typ {
			s, _ := m["message"].(string)
			return s
		}
	}
	return ""
}

// liveClusterResponses is what a converged foundation answers beyond the
// bring-up: the Service the address is read from, and the kinds prune asks the
// cluster for. Without them the loop reports no address and refuses to prune,
// which is correct behaviour against a cluster that said nothing — and would
// make these tests fail for a reason that has nothing to do with the edit.
func liveClusterResponses() []Response {
	return []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "get service " + ybaServiceName, Result: Result{Stdout: tserverServiceJSON}},
		{Match: "get endpoints", Result: Result{Stdout: ""}},
		{Match: "api-resources", Result: Result{
			Stdout: "ybuniverses\nybproviders\nybcertificates\nstorageconfigs\nkmsconfigs\n"}},
	}
}

// TestConvergedPassesThroughConvergingBeforeItSettles.
//
// Converged is False TWICE on the way to a failure: once as Converging while
// the converge is in flight, then as ConvergeFailed when it ends. A watcher
// keying on the STATUS alone cannot tell "working on it" from "gave up" — it
// has to read the reason.
//
// Pinned because it is not obvious and it is a real trap: the GKE step that
// provokes a rejection waited on the status, matched the in-flight False, and
// read a reason that had not settled yet.
func TestConvergedPassesThroughConvergingBeforeItSettles(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{
		block:    make(chan struct{}),
		applyErr: errFromApply("YBUniverse", "numNodez: field not declared in schema"),
	}
	r := testReconciler(t, rec, f)

	r.tick(context.Background()) // starts the converge; it blocks
	inFlight := lastStatusPatch(t, rec)
	if got := conditionReason(inFlight, ConditionConverged); got != "Converging" {
		t.Fatalf("while converging, reason = %q, want Converging", got)
	}
	if got := conditionStatus(inFlight, ConditionConverged); got != CondFalse {
		t.Errorf("while converging, status = %q, want False", got)
	}

	close(f.block)
	r.settle(t)
	r.tick(context.Background()) // reaps the failure

	settled := lastStatusPatch(t, rec)
	if got := conditionReason(settled, ConditionConverged); got != "ConvergeFailed" {
		t.Fatalf("after the failure, reason = %q, want ConvergeFailed", got)
	}
	// False in BOTH, which is the whole point.
	if got := conditionStatus(settled, ConditionConverged); got != CondFalse {
		t.Errorf("after the failure, status = %q, want False", got)
	}
}

func conditionStatus(status map[string]any, typ string) string {
	list, _ := status["conditions"].([]any)
	for _, c := range list {
		m, _ := c.(map[string]any)
		if m["type"] == typ {
			s, _ := m["status"].(string)
			return s
		}
	}
	return ""
}
