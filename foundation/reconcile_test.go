package foundation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// crSpecJSON is the spec from examples/foundation-cr.yaml as the API server
// would hand it back. Going through the real example keeps the object this
// loop is tested against the same one a user applies.
//
// A Go map loses key order, which is exactly why a Foundation declares its CRs
// as a LIST: the list survives, and nothing else in the spec is order-sensitive.
func crSpecJSON(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("examples/foundation-cr.yaml")
	if err != nil {
		t.Fatalf("read the example: %v", err)
	}
	var doc struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the example: %v", err)
	}
	out, err := json.Marshal(doc.Spec)
	if err != nil {
		t.Fatalf("re-encode the spec: %v", err)
	}
	return out
}

// The example has to be one the loader and Validate both accept, or every test
// below is testing the error path by accident.
func TestTheExampleFoundationIsValid(t *testing.T) {
	cr := &FoundationCR{Spec: crSpecJSON(t)}
	cr.Metadata.Namespace = "amp"
	spec, err := loadCRSpec(cr)
	if err != nil {
		t.Fatalf("examples/foundation-cr.yaml does not validate: %v", err)
	}
	if spec.Namespace != "amp" {
		t.Errorf("Namespace = %q, want the object's own", spec.Namespace)
	}
	if spec.DeletionPolicy != DeletionRetain {
		t.Errorf("DeletionPolicy = %q, want the Retain default", spec.DeletionPolicy)
	}
}

// crObject builds the JSON `kubectl get -o json` would print.
func crObject(t *testing.T, gen int64, mutate func(m map[string]any)) string {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "meko.yugabyte.com/v1alpha1",
		"kind":       "Foundation",
		"metadata": map[string]any{
			"name":       "foundation",
			"namespace":  "amp",
			"uid":        "uid-1",
			"generation": gen,
		},
		"spec": json.RawMessage(crSpecJSON(t)),
	}
	if mutate != nil {
		mutate(obj)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func meta(obj map[string]any) map[string]any { return obj["metadata"].(map[string]any) }

func specOf(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(obj["spec"].(json.RawMessage), &m); err != nil {
		t.Fatal(err)
	}
	obj["spec"] = m
	return m
}

// fakeConverger stands in for a Deployer.
type fakeConverger struct {
	mu        sync.Mutex
	applies   int
	downs     int
	publishes []*Info
	applyErr  error
	downErr   error
	status    *Status
	// block, when non-nil, holds Apply until it is closed or the interrupt
	// fires, which is how a converge is kept in flight across ticks.
	block     chan struct{}
	interrupt <-chan struct{}
}

func (f *fakeConverger) Apply(ctx context.Context) (*Info, error) {
	f.mu.Lock()
	f.applies++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-f.interrupt:
			return nil, ErrInterrupted
		}
	}
	return nil, f.applyErr
}

func (f *fakeConverger) Status(context.Context) (*Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != nil {
		return f.status, nil
	}
	return &Status{Namespace: "amp", Phase: PhaseConverging}, nil
}

func (f *fakeConverger) PublishConn(_ context.Context, _ string, info *Info) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, info)
	return nil
}

func (f *fakeConverger) Down(context.Context, DownOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downs++
	return f.downErr
}

func (f *fakeConverger) count() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applies, f.downs
}

// testReconciler wires a Reconciler to a scripted cluster and a fake converger.
func testReconciler(t *testing.T, rec *Recorder, f *fakeConverger) *Reconciler {
	t.Helper()
	r := NewReconciler(rec.Runner(), "amp", "foundation", "").
		WithLogger(func(string, ...any) {})
	r.deployer = func(_ *Spec, interrupt <-chan struct{}) converger {
		f.mu.Lock()
		if interrupt != nil {
			f.interrupt = interrupt
		}
		f.mu.Unlock()
		return f
	}
	return r
}

// settle waits for the converge the tick started, so an assertion about what it
// did is not racing it.
func (r *Reconciler) settle(t *testing.T) {
	t.Helper()
	if r.active == nil {
		return
	}
	select {
	case <-r.active.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the converge never finished")
	}
}

// A generation is converged once. Re-reading the same object does nothing,
// which is what stops a 30-second poll running `helm upgrade` 120 times an hour
// against a foundation with nothing to do.
func TestOneConvergePerGeneration(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)

	r.tick(context.Background())
	r.settle(t)
	for range 3 {
		r.tick(context.Background())
		r.settle(t)
	}
	if applies, _ := f.count(); applies != 1 {
		t.Errorf("converged %d times for one generation, want 1", applies)
	}
}

// A new generation converges again, even though the last one succeeded.
func TestANewGenerationConvergesAgain(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 2, nil)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)

	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background())
	r.settle(t)

	if applies, _ := f.count(); applies != 2 {
		t.Errorf("converged %d times across two generations, want 2", applies)
	}
}

// A failed converge is retried on a backoff, not on the next poll — and the
// backoff grows, so a foundation failing on something that needs a human does
// not hammer the cluster for however long that takes.
func TestAFailedConvergeBacksOff(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{applyErr: errors.New("helm timed out")}
	now := time.Now()
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }

	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background()) // reaps the failure and sets the backoff
	r.settle(t)
	if applies, _ := f.count(); applies != 1 {
		t.Fatalf("retried immediately after a failure (%d converges)", applies)
	}

	now = now.Add(DefaultMinBackoff + time.Second)
	r.tick(context.Background())
	r.settle(t)
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("did not retry after the backoff elapsed (%d converges)", applies)
	}

	// Second failure, second wait: longer than the first.
	r.tick(context.Background())
	if r.failures != 2 {
		t.Fatalf("failures = %d, want 2", r.failures)
	}
	if got := r.backoff(); got <= DefaultMinBackoff {
		t.Errorf("backoff after two failures = %s, want more than the first %s", got, DefaultMinBackoff)
	}
}

// ...and it has a ceiling, because an unbounded doubling becomes a foundation
// that notices a fixed problem days later.
func TestBackoffIsCapped(t *testing.T) {
	r := NewReconciler(nil, "amp", "foundation", "")
	for _, n := range []int{20, 100} {
		r.failures = n
		if got := r.backoff(); got != DefaultMaxBackoff {
			t.Errorf("backoff after %d failures = %s, want the %s ceiling", n, got, DefaultMaxBackoff)
		}
	}
}

// An edit is the user's answer to whatever the last converge failed on, so it
// is acted on at once rather than at the end of a backoff that may be half an
// hour long.
func TestANewGenerationClearsTheBackoff(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 2, nil)}},
	}}
	f := &fakeConverger{applyErr: errors.New("helm timed out")}
	now := time.Now()
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }

	r.tick(context.Background()) // converge generation 1
	r.settle(t)
	r.tick(context.Background()) // reap the failure, set a backoff
	r.settle(t)

	// No time passes at all, so the backoff has definitely not elapsed.
	r.tick(context.Background()) // generation 2
	r.settle(t)
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("an edit waited out the backoff (%d converges)", applies)
	}
	// And the failure count starts again from zero, so the new generation's
	// first retry is the short wait rather than wherever the old one had
	// climbed to.
	if r.failures != 0 {
		t.Errorf("failures = %d after a new generation, want the count reset", r.failures)
	}
}

// A converge still running when the spec changes is told its input is stale,
// and the loop then starts one on the new generation rather than recording the
// old one as converged.
func TestAnEditInterruptsTheConvergeInFlight(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}, Once: true},
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 2, nil)}},
	}}
	f := &fakeConverger{block: make(chan struct{})}
	r := testReconciler(t, rec, f)

	r.tick(context.Background()) // starts generation 1, which blocks
	if r.active == nil {
		t.Fatal("no converge was started")
	}
	r.tick(context.Background()) // sees generation 2 and interrupts
	r.settle(t)

	if r.lastGen == 1 {
		t.Error("recorded the interrupted generation as converged; the next tick would then " +
			"skip the edit that superseded it")
	}
	r.tick(context.Background()) // reaps the interrupt and starts generation 2
	// Checked before the counts: active is set synchronously by the tick,
	// while the converge itself runs in a goroutine that may not have reached
	// the fake yet.
	if r.active == nil || r.active.generation != 2 {
		t.Fatal("did not start a converge on the generation that superseded it")
	}
	close(f.block)
	r.settle(t)
	if applies, _ := f.count(); applies != 2 {
		t.Errorf("converged %d times, want the interrupted one and its replacement", applies)
	}
}

// The status is written on every pass, not once per converge: a bring-up takes
// tens of minutes and a status that only moves at the end of one is
// indistinguishable from a dead pod.
func TestStatusIsWrittenEveryTick(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{block: make(chan struct{})}
	r := testReconciler(t, rec, f)

	r.tick(context.Background())
	r.tick(context.Background())
	r.tick(context.Background())
	close(f.block)
	r.settle(t)

	patches := 0
	for _, c := range rec.Argvs() {
		if strings.Contains(c, "patch") && strings.Contains(c, "--subresource=status") {
			patches++
		}
	}
	if patches < 3 {
		t.Errorf("wrote the status %d times over three passes", patches)
	}
}

// The loop writes the status SUBRESOURCE and never the spec. The object is the
// user's; writing to it would mean the controller could edit the input it is
// being judged against.
func TestTheLoopNeverWritesTheSpec(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.tick(context.Background())
	r.settle(t)
	r.tick(context.Background())

	for _, c := range rec.Argvs() {
		if !strings.Contains(c, "patch") {
			continue
		}
		if !strings.Contains(c, "--subresource=status") && !strings.Contains(c, "finalizers") {
			t.Errorf("patched the object outside the status subresource: %s", c)
		}
	}
}

// observedGeneration is the generation the status DESCRIBES, so it must lag
// metadata.generation until that generation has actually been converged — that
// gap is the only way a reader can tell whether their edit has been acted on.
func TestObservedGenerationLagsUntilConverged(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 7, nil)}},
	}}
	f := &fakeConverger{block: make(chan struct{})}
	r := testReconciler(t, rec, f)

	r.tick(context.Background()) // converge of 7 is still running
	if got := lastStatusPatch(t, rec)["observedGeneration"]; got != float64(0) {
		t.Errorf("observedGeneration = %v while the converge was still running, want 0", got)
	}
	close(f.block)
	r.settle(t)
	r.tick(context.Background())
	if got := lastStatusPatch(t, rec)["observedGeneration"]; got != float64(7) {
		t.Errorf("observedGeneration = %v after converging generation 7", got)
	}
}

// lastStatusPatch decodes the most recent status patch body.
func lastStatusPatch(t *testing.T, rec *Recorder) map[string]any {
	t.Helper()
	var body string
	for _, c := range rec.Calls {
		joined := strings.Join(c.Argv, " ")
		if !strings.Contains(joined, "--subresource=status") {
			continue
		}
		for i, a := range c.Argv {
			if a == "-p" && i+1 < len(c.Argv) {
				body = c.Argv[i+1]
			}
		}
	}
	if body == "" {
		t.Fatal("no status patch was sent")
	}
	var m struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode the status patch: %v\n%s", err, body)
	}
	return m.Status
}

// lastTransitionTime marks when a condition last CHANGED. At one write per
// poll, a timestamp that moved every write would answer nothing.
func TestConditionTimestampsOnlyMoveOnChange(t *testing.T) {
	old := []Condition{{Type: ConditionReady, Status: CondFalse, LastTransitionTime: "2026-01-01T00:00:00Z"}}

	same := mergeConditions(old, []Condition{{Type: ConditionReady, Status: CondFalse}}, "2026-06-01T00:00:00Z")
	if same[0].LastTransitionTime != "2026-01-01T00:00:00Z" {
		t.Errorf("an unchanged condition moved its timestamp to %q", same[0].LastTransitionTime)
	}
	flipped := mergeConditions(old, []Condition{{Type: ConditionReady, Status: CondTrue}}, "2026-06-01T00:00:00Z")
	if flipped[0].LastTransitionTime != "2026-06-01T00:00:00Z" {
		t.Errorf("a condition that flipped kept its old timestamp %q", flipped[0].LastTransitionTime)
	}
	// A condition nobody has written yet gets now, not an empty string.
	fresh := mergeConditions(nil, []Condition{{Type: ConditionConverged, Status: CondUnknown}}, "2026-06-01T00:00:00Z")
	if fresh[0].LastTransitionTime == "" {
		t.Error("a new condition was written with no lastTransitionTime")
	}
}

// A Foundation that is not there yet is a state to sit in, not a crash: the
// chart installs this pod and the object can be applied afterwards.
func TestAMissingFoundationIsNotAnError(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{ExitCode: 1,
			Stderr: `Error from server (NotFound): foundations.meko.yugabyte.com "foundation" not found`}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.tick(context.Background())
	if applies, _ := f.count(); applies != 0 {
		t.Error("converged with no Foundation to converge onto")
	}
}

// A spec the loader refuses is reported and not retried: only an edit fixes it,
// and an edit bumps the generation, which starts a converge by itself.
func TestAnInvalidSpecIsReportedAndNotConverged(t *testing.T) {
	bad := crObject(t, 1, func(obj map[string]any) {
		spec := specOf(t, obj)
		spec["namespaec"] = "typo"
	})
	rec := &Recorder{Responses: []Response{{Match: "get foundations", Result: Result{Stdout: bad}}}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.tick(context.Background())

	if applies, _ := f.count(); applies != 0 {
		t.Error("converged a spec the loader rejected")
	}
	status := lastStatusPatch(t, rec)
	if status["phase"] != string(PhaseFailed) {
		t.Errorf("phase = %v for an unparseable spec, want %s", status["phase"], PhaseFailed)
	}
	if !strings.Contains(conditionReason(status, ConditionConverged), "SpecInvalid") {
		t.Errorf("conditions do not say the spec is the problem: %v", status["conditions"])
	}
}

func conditionReason(status map[string]any, typ string) string {
	list, _ := status["conditions"].([]any)
	for _, c := range list {
		m, _ := c.(map[string]any)
		if m["type"] == typ {
			s, _ := m["reason"].(string)
			return s
		}
	}
	return ""
}

// kubeContext cannot mean anything in a Foundation — the loop picked its
// cluster when it read the object — so it is refused rather than ignored.
// Ignoring it would leave somebody believing they had aimed a foundation at
// another cluster.
func TestAKubeContextInTheObjectIsRefused(t *testing.T) {
	obj := crObject(t, 1, func(obj map[string]any) {
		specOf(t, obj)["kubeContext"] = "some-other-cluster"
	})
	rec := &Recorder{Responses: []Response{{Match: "get foundations", Result: Result{Stdout: obj}}}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.tick(context.Background())

	if applies, _ := f.count(); applies != 0 {
		t.Fatal("converged a spec naming a cluster the loop is not on")
	}
	if !strings.Contains(conditionReason(lastStatusPatch(t, rec), ConditionConverged), "SpecInvalid") {
		t.Error("the refusal was not reported on the object")
	}
}

// finalizerPatches returns the finalizer lists the loop wrote, in order.
func finalizerPatches(t *testing.T, rec *Recorder) [][]string {
	t.Helper()
	var out [][]string
	for _, c := range rec.Calls {
		joined := strings.Join(c.Argv, " ")
		if !strings.Contains(joined, "patch") || strings.Contains(joined, "--subresource=status") {
			continue
		}
		for i, a := range c.Argv {
			if a != "-p" || i+1 >= len(c.Argv) {
				continue
			}
			var body struct {
				Metadata struct {
					Finalizers []string `json:"finalizers"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(c.Argv[i+1]), &body); err != nil {
				t.Fatalf("decode a finalizer patch: %v", err)
			}
			out = append(out, body.Metadata.Finalizers)
		}
	}
	return out
}

func withPolicy(t *testing.T, p DeletionPolicy) func(map[string]any) {
	return func(obj map[string]any) { specOf(t, obj)["deletionPolicy"] = string(p) }
}

// The default policy puts NO finalizer on the object, and that is the property
// that makes the default safe: a finalizer can only be cleared by this pod, and
// deleting the namespace takes the pod out at the same moment as the object —
// leaving the namespace in Terminating for good, where `kubectl delete` is a
// no-op. With Retain there is nothing to clear.
func TestRetainAddsNoFinalizer(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, nil)}},
	}}
	r := testReconciler(t, rec, &fakeConverger{})
	r.tick(context.Background())
	r.settle(t)

	if got := finalizerPatches(t, rec); len(got) != 0 {
		t.Errorf("wrote finalizers %v for the Retain default", got)
	}
}

// Delete does add one — there is something to do on deletion, and the object
// has to stay long enough to do it.
func TestDeleteAddsTheFinalizer(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: crObject(t, 1, withPolicy(t, DeletionDelete))}},
	}}
	r := testReconciler(t, rec, &fakeConverger{})
	r.tick(context.Background())
	r.settle(t)

	got := finalizerPatches(t, rec)
	if len(got) == 0 || !slices.Contains(got[0], FoundationFinalizer) {
		t.Fatalf("finalizer patches = %v, want one adding %s", got, FoundationFinalizer)
	}
}

// The finalizer TRACKS the policy rather than being set once, and that is the
// escape hatch for a foundation whose teardown cannot finish: flipping the
// policy back to Retain drops it on the next pass and lets the object go.
func TestFlippingToRetainDropsTheFinalizer(t *testing.T) {
	existing := crObject(t, 2, func(obj map[string]any) {
		meta(obj)["finalizers"] = []string{FoundationFinalizer}
	})
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: existing}},
	}}
	r := testReconciler(t, rec, &fakeConverger{})
	r.tick(context.Background())
	r.settle(t)

	got := finalizerPatches(t, rec)
	if len(got) == 0 {
		t.Fatal("the finalizer was left on an object whose policy no longer asks for one")
	}
	if slices.Contains(got[len(got)-1], FoundationFinalizer) {
		t.Errorf("last finalizer patch = %v, still carrying %s", got[len(got)-1], FoundationFinalizer)
	}
}

// deleting builds an object mid-deletion.
func deleting(t *testing.T, policy DeletionPolicy, since time.Duration, now time.Time) string {
	t.Helper()
	return crObject(t, 1, func(obj map[string]any) {
		m := meta(obj)
		m["finalizers"] = []string{FoundationFinalizer}
		m["deletionTimestamp"] = now.Add(-since).UTC().Format(time.RFC3339)
		specOf(t, obj)["deletionPolicy"] = string(policy)
	})
}

// A deletion whose policy has since been flipped to Retain releases the object
// and leaves the universe alone.
func TestDeletingUnderRetainTearsNothingDown(t *testing.T) {
	now := time.Now()
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: deleting(t, DeletionRetain, time.Minute, now)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }
	r.tick(context.Background())

	if _, downs := f.count(); downs != 0 {
		t.Error("tore the universe down under the Retain policy")
	}
	if got := finalizerPatches(t, rec); len(got) == 0 || slices.Contains(got[len(got)-1], FoundationFinalizer) {
		t.Errorf("did not release the object: finalizer patches %v", got)
	}
}

// Under Delete the universe goes first, and only then is the object released.
func TestDeletingUnderDeleteTearsDownThenReleases(t *testing.T) {
	now := time.Now()
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: deleting(t, DeletionDelete, time.Minute, now)}},
	}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }
	r.tick(context.Background())

	if _, downs := f.count(); downs != 1 {
		t.Errorf("tore down %d times, want 1", downs)
	}
	if got := finalizerPatches(t, rec); len(got) == 0 || slices.Contains(got[len(got)-1], FoundationFinalizer) {
		t.Errorf("did not release the object after a successful teardown: %v", got)
	}
}

// A teardown that fails is retried rather than abandoned — the object stays,
// which is what the finalizer is for.
func TestAFailedTeardownKeepsTheObject(t *testing.T) {
	now := time.Now()
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations", Result: Result{Stdout: deleting(t, DeletionDelete, time.Minute, now)}},
	}}
	f := &fakeConverger{downErr: errors.New("the universe CR will not go")}
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }

	r.tick(context.Background())
	r.tick(context.Background())

	if _, downs := f.count(); downs != 2 {
		t.Errorf("tore down %d times over two passes, want it retried", downs)
	}
	for _, list := range finalizerPatches(t, rec) {
		if !slices.Contains(list, FoundationFinalizer) {
			t.Error("released the object while the teardown was still failing")
		}
	}
}

// ...but not retried forever. A finalizer that never resolves is WORSE than an
// orphaned universe: only this pod can clear it, and a namespace deletion takes
// the pod out at the same time as the object, so the namespace stays in
// Terminating permanently. Past the grace the object is released and what is
// left behind goes in the log.
func TestATeardownThatKeepsFailingGivesUp(t *testing.T) {
	now := time.Now()
	rec := &Recorder{Responses: []Response{
		{Match: "get foundations",
			Result: Result{Stdout: deleting(t, DeletionDelete, DefaultTeardownGrace+time.Minute, now)}},
	}}
	f := &fakeConverger{downErr: errors.New("the universe CR will not go")}
	var logged strings.Builder
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }
	r.log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	r.tick(context.Background())

	got := finalizerPatches(t, rec)
	if len(got) == 0 || slices.Contains(got[len(got)-1], FoundationFinalizer) {
		t.Fatalf("held the object past the grace period: finalizer patches %v", got)
	}
	if !strings.Contains(logged.String(), "by hand") {
		t.Errorf("gave up without saying what was left behind:\n%s", logged.String())
	}
}

// A spec nobody can parse must not hold the object open either. Refusing to
// release over an unparseable spec would wedge it with no way out, since the
// only edit that could fix it is one on an object already being deleted.
func TestADeletionWithAnUnparseableSpecStillReleases(t *testing.T) {
	now := time.Now()
	obj := crObject(t, 1, func(obj map[string]any) {
		m := meta(obj)
		m["finalizers"] = []string{FoundationFinalizer}
		m["deletionTimestamp"] = now.Format(time.RFC3339)
		specOf(t, obj)["namespaec"] = "typo"
	})
	rec := &Recorder{Responses: []Response{{Match: "get foundations", Result: Result{Stdout: obj}}}}
	f := &fakeConverger{}
	r := testReconciler(t, rec, f)
	r.now = func() time.Time { return now }
	r.tick(context.Background())

	if got := finalizerPatches(t, rec); len(got) == 0 || slices.Contains(got[len(got)-1], FoundationFinalizer) {
		t.Errorf("held the object open over a spec nobody can parse: %v", got)
	}
}

// Another controller's finalizer is left alone. The whole list is sent on every
// patch, because a merge patch replaces an array wholesale, so filtering rather
// than clearing is what keeps somebody else's entry.
func TestReleasingKeepsOtherFinalizers(t *testing.T) {
	now := time.Now()
	obj := crObject(t, 1, func(obj map[string]any) {
		m := meta(obj)
		m["finalizers"] = []string{"somebody.else/cleanup", FoundationFinalizer}
		m["deletionTimestamp"] = now.Format(time.RFC3339)
	})
	rec := &Recorder{Responses: []Response{{Match: "get foundations", Result: Result{Stdout: obj}}}}
	r := testReconciler(t, rec, &fakeConverger{})
	r.now = func() time.Time { return now }
	r.tick(context.Background())

	got := finalizerPatches(t, rec)
	if len(got) == 0 {
		t.Fatal("no finalizer patch was sent")
	}
	if !slices.Contains(got[len(got)-1], "somebody.else/cleanup") {
		t.Errorf("dropped another controller's finalizer: %v", got[len(got)-1])
	}
}

// TestTheExampleFoundationIsConsumerReady.
//
// The example is what a customer copies, and the first version of it was the
// smallest spec that VALIDATED — which is a different thing, and a trap: three
// of the settings below are create-time only, so a universe built without them
// cannot be edited into having them, only rebuilt. This pins that the example
// carries them.
func TestTheExampleFoundationIsConsumerReady(t *testing.T) {
	cr := &FoundationCR{Spec: crSpecJSON(t)}
	cr.Metadata.Namespace = "amp"
	spec, err := loadCRSpec(cr)
	if err != nil {
		t.Fatalf("examples/foundation-cr.yaml does not validate: %v", err)
	}
	u := spec.universe()

	// Create-time only: both encryption flags, and the CA that turns an
	// encrypted universe into an AUTHENTICATED one (sslmode=verify-full rather
	// than require).
	for _, f := range []string{"enableNodeToNodeEncrypt", "enableClientToNodeEncrypt"} {
		if !spec.encryptionOn(f) {
			t.Errorf("%s is not set: it is create-time only, so a customer copying this gets a "+
				"universe that cannot be encrypted later, only rebuilt", f)
		}
	}
	if ca, _ := u["rootCA"].(string); ca == "" {
		t.Error("no rootCA: YBA invents a CA nothing else trusts and the best DSN available " +
			"is sslmode=require")
	}

	// Start-time gflags on BOTH processes, and the preview-flag CSV that YBA
	// requires alongside. Adding them later is a rolling restart.
	gflags, _ := u["gFlags"].(map[string]any)
	for _, side := range []string{"masterGFlags", "tserverGFlags"} {
		set, _ := gflags[side].(map[string]any)
		if set["ysql_yb_enable_mage"] != "true" {
			t.Errorf("%s does not enable MAGE: the extension will not install without it, and "+
				"turning it on later is a rolling restart", side)
		}
		csv, _ := set["allowed_preview_flags_csv"].(string)
		if !strings.Contains(csv, "ysql_yb_enable_mage") {
			t.Errorf("%s.allowed_preview_flags_csv is %q: YBA refuses a preview flag the CSV "+
				"does not name", side, csv)
		}
	}

	// The node certificates have to name the endpoint Service, which is in none
	// of the SANs the chart computes for itself — without this every
	// verify-full consumer fails the hostname check.
	overrides, _ := u["kubernetesOverrides"].(map[string]any)
	tls, _ := overrides["tls"].(map[string]any)
	if _, ok := tls["extraSANs"]; !ok {
		t.Error("no kubernetesOverrides.tls.extraSANs: the only address consumers get is in " +
			"none of the chart's own SANs, so verify-full cannot connect")
	}

	// A real placement, in the shape the provider cross-check actually walks.
	if spec.PlacementUnchecked() {
		t.Error("placementInfo is in a shape validatePlacement does not walk, so the " +
			"cross-check against the provider's zones silently did nothing")
	}
}

// The example and examples/foundation-cli.yaml are the same foundation in two forms, so
// the settings that decide what the universe IS have to agree. They are allowed
// to differ on site details — registries, passwords, volume sizes — but not on
// these.
func TestTheTwoExamplesAgreeOnWhatMatters(t *testing.T) {
	cr := &FoundationCR{Spec: crSpecJSON(t)}
	cr.Metadata.Namespace = "amp"
	fromCR, err := loadCRSpec(cr)
	if err != nil {
		t.Fatalf("load the CR example: %v", err)
	}
	fromFile := loadExample(t, "foundation-cli.yaml")

	for _, f := range []string{"enableNodeToNodeEncrypt", "enableClientToNodeEncrypt"} {
		if fromCR.encryptionOn(f) != fromFile.encryptionOn(f) {
			t.Errorf("%s differs between the two examples", f)
		}
	}
	if fromCR.SSLMode() != fromFile.SSLMode() {
		t.Errorf("sslMode differs: CR %q, file %q — the two examples describe universes a "+
			"consumer would have to connect to differently", fromCR.SSLMode(), fromFile.SSLMode())
	}
	if a, b := fromCR.universe()["gFlags"], fromFile.universe()["gFlags"]; !reflect.DeepEqual(a, b) {
		t.Errorf("gFlags differ between the two examples:\n CR:   %v\n file: %v", a, b)
	}
}

// Preflight WARNS about a placementInfo whose zones it could not find, rather
// than refusing it: which shapes the CRD accepts is YBA's to decide. Silence
// was the old behaviour and it is what let an example ship with a placement
// nothing checked.
func TestPreflightWarnsOnAnUncheckedPlacement(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	// A shape the walk does not understand: the keys are plausible and the
	// cross-check finds no zone code under any of them.
	spec.universe()["placementInfo"] = map[string]any{
		"cloudList": []any{map[string]any{"code": "kubernetes"}},
	}
	if !spec.PlacementUnchecked() {
		t.Fatal("a placementInfo with no zone codes was reported as checked")
	}

	var logged strings.Builder
	rec := &Recorder{Responses: []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{Stdout: "WaitForFirstConsumer"}},
	}}
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) {
		fmt.Fprintf(&logged, f+"\n", a...)
	})
	_ = d.Preflight(context.Background())

	if !strings.Contains(logged.String(), "placementInfo") {
		t.Errorf("preflight said nothing about an unchecked placement:\n%s", logged.String())
	}
}

// And says nothing about the example's own placement, which it does check.
func TestPreflightIsQuietOnACheckedPlacement(t *testing.T) {
	if loadExample(t, "foundation-cli.yaml").PlacementUnchecked() {
		t.Error("the shipped example's placement is not in the shape the cross-check walks")
	}
}
