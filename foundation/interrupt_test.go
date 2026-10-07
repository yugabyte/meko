package foundation

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// convergeResponses is a cluster where everything a converge asks about
// answers, so a test can make exactly one thing go wrong.
func convergeResponses(universe Result) []Response {
	return []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{Stdout: "WaitForFirstConsumer"}},
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Ready"}}`}},
		{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"success":true,"resourceUUID":"c1"}}`}},
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.3.0-b21\n"}},
		{Match: "get ybuniverse", Result: universe},
	}
}

// TestAnInterruptLetsTheStepInFlightFinish is the whole point of
// WithInterrupt: the interrupt fires *during* installYBA, which writes.
//
// That step has to run to its end — cutting `helm upgrade` short is exactly
// what leaves the release in `pending-install`, which no later converge gets
// past without uninstalling it — and then the converge stops, rather than
// carrying on applying a spec that has been superseded.
func TestAnInterruptLetsTheStepInFlightFinish(t *testing.T) {
	// Never settles: Ready can be left over from the previous generation, so
	// an action still running keeps the wait polling.
	rec := &Recorder{Responses: convergeResponses(Result{
		Stdout: statusWithAction("Ready", "CreateKubernetesUniverse", "running", "")})}

	interrupt := make(chan struct{})
	var once sync.Once
	run := func(ctx context.Context, stdin []byte, argv []string) (Result, error) {
		if strings.Contains(strings.Join(argv, " "), "helm upgrade") {
			once.Do(func() { close(interrupt) })
		}
		return rec.Run(ctx, stdin, argv)
	}

	spec := loadExample(t, "foundation-cli.yaml")
	// A long universe timeout on purpose: if the interrupt does not reach the
	// wait, this test hangs instead of passing by coincidence.
	d := New(spec, run).
		WithTimeouts(time.Second, 10*time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithInterrupt(interrupt)

	start := time.Now()
	_, err := d.Up(context.Background())
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Up returned %v, want ErrInterrupted — a caller cannot otherwise tell a "+
			"superseded converge from a broken one, and would back off over it", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v to notice the interrupt; it should not have run to the universe timeout",
			elapsed)
	}

	var inc *IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("error is not an *IncompleteError: %v", err)
	}
	if inc.ID != StepYBA {
		t.Errorf("stopped at %q, want the last step that actually ran", inc.ID)
	}

	// installYBA does not end at the helm call: it then confirms yugaware is
	// up and that YBA has its single customer. Those running is what shows the
	// step was allowed to finish rather than cut off at the interrupt.
	argvs := strings.Join(rec.Argvs(), "\n")
	if !strings.Contains(argvs, "get statefulset") {
		t.Errorf("installYBA was cut short at the helm call:\n%s", argvs)
	}
	// And nothing started afterwards. The CRs are applied from stdin, which is
	// what separates that apply from the CRD bundle's, applied from a URL in an
	// earlier step.
	for _, c := range rec.Calls {
		if c.Stdin != "" && strings.Contains(strings.Join(c.Argv, " "), "apply") {
			t.Errorf("applied a CR from a spec already known to be stale:\n%s", c.Stdin)
		}
	}
	if !slices.Contains(inc.SkippedIDs, StepApplyResources) {
		t.Errorf("SkippedIDs = %v, does not report the steps the interrupt stopped", inc.SkippedIDs)
	}
}

// An interrupt that arrives before the converge starts stops it at the first
// step, which is the right answer and not a bug: there is no reason to converge
// a spec already known to be stale, and preflight writes nothing.
func TestAnInterruptBeforeTheConvergeStopsItAtOnce(t *testing.T) {
	interrupt := make(chan struct{})
	close(interrupt)
	rec := &Recorder{Responses: convergeResponses(Result{Stdout: `{"status":{"universeState":"Ready"}}`})}
	spec := loadExample(t, "foundation-cli.yaml")
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithInterrupt(interrupt)

	if _, err := d.Up(context.Background()); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Up returned %v, want ErrInterrupted", err)
	}
	if argvs := strings.Join(rec.Argvs(), "\n"); strings.Contains(argvs, "helm upgrade") {
		t.Errorf("installed YBA from a spec already known to be stale:\n%s", argvs)
	}
}

// Without WithInterrupt nothing changes — the CLI is the caller that has none.
func TestWithoutAnInterruptTheWaitRunsToTheTimeout(t *testing.T) {
	rec := &Recorder{Responses: convergeResponses(Result{
		Stdout: statusWithAction("Ready", "CreateKubernetesUniverse", "running", "")})}
	d, _ := testDeployer(t, rec)
	_, err := d.Up(context.Background())
	if errors.Is(err, ErrInterrupted) {
		t.Fatalf("a deployer with no interrupt reported one: %v", err)
	}
	if err == nil {
		t.Fatal("Up succeeded on a universe that never settled")
	}
}

// stepContext is where the mutating/abandonable split is actually made, so it
// is pinned directly: adding a step and forgetting the flag is the mistake this
// catches, and it is one a converge test would not show.
func TestStepContextOnlyCancelsTheStepsThatWriteNothing(t *testing.T) {
	interrupt := make(chan struct{})
	close(interrupt)
	d := &Deployer{interrupt: interrupt}

	mut, cancel := d.stepContext(context.Background(), step{mutating: true})
	cancel()
	if mut.Err() != nil {
		t.Error("a mutating step was handed a cancelled context; being cut off midway is how " +
			"helm is left in pending-install")
	}

	soft, cancel := d.stepContext(context.Background(), step{})
	defer cancel()
	select {
	case <-soft.Done():
	case <-time.After(2 * time.Second):
		t.Error("an abandonable step was not cancelled by the interrupt")
	}

	// Every step the converge runs has to be on one side of this or the other,
	// and the writes are the ones named here. A new step defaults to
	// abandonable, so this fails if a writing step is added without the flag.
	mutating := map[StepID]bool{
		StepCRDs: true, StepNamespace: true, StepYBA: true, StepApplyResources: true, StepPrune: true,
	}
	spec := loadExample(t, "foundation-cli.yaml")
	for _, s := range New(spec, nil).convergeSteps() {
		if s.mutating != mutating[s.id] {
			t.Errorf("step %q has mutating=%v; if it writes to the cluster an interrupt must not "+
				"cut it short, and if it does not it must not hold the loop", s.id, s.mutating)
		}
	}
}

// The other half of the split: an interrupt that arrives while a step is
// ALREADY running cuts that step short, as long as it writes nothing.
//
// Without this the loop would sit out the whole universe timeout — 45 minutes
// by default — on a spec the user has already replaced, which is the delay
// WithInterrupt exists to remove.
func TestAnInterruptCutsTheRunningWaitShort(t *testing.T) {
	rec := &Recorder{Responses: convergeResponses(Result{
		Stdout: statusWithAction("Ready", "CreateKubernetesUniverse", "running", "")})}

	interrupt := make(chan struct{})
	var once sync.Once
	run := func(ctx context.Context, stdin []byte, argv []string) (Result, error) {
		// HasSuffix, not Contains: preflight reads the universe's encryption
		// flags with `-o jsonpath=`, which `-o json` is a prefix of, and
		// matching that would fire the interrupt two steps early.
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "get ybuniverse") && strings.HasSuffix(joined, "-o json") {
			once.Do(func() { close(interrupt) })
		}
		return rec.Run(ctx, stdin, argv)
	}

	d := New(loadExample(t, "foundation-cli.yaml"), run).
		WithTimeouts(time.Second, 10*time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithInterrupt(interrupt)

	start := time.Now()
	_, err := d.Up(context.Background())
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Up returned %v, want ErrInterrupted", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the wait took %v to notice; it ran on towards the universe timeout", elapsed)
	}
	var inc *IncompleteError
	if errors.As(err, &inc) && inc.ID != StepWaitUniverse {
		t.Errorf("stopped at %q, want the wait that was interrupted", inc.ID)
	}
}
