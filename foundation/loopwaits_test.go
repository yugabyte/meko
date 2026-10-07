package foundation

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTheLoopWaitsForYBAAndNothingElse pins the wiring that was missing.
//
// The design is that the pod blocks on YBA being up and usable, and on nothing
// after it: the loop re-reads every CR's status each poll and reports it, so a
// wait inside a converge buys a slower converge and no more knowledge.
//
// It was documented that way and not implemented — the loop built its Deployer
// without WithoutWaits, so every converge ran the one-shot caller's per-CR
// gates and the universe wait. Nothing caught it because every other loop test
// replaces the whole Deployer with a fake converger, which never exercises the
// real one's configuration. This reaches into the production wiring on purpose.
func TestTheLoopWaitsForYBAAndNothingElse(t *testing.T) {
	r := NewReconciler((&Recorder{}).Runner(), "amp", "foundation", "")
	d, ok := r.deployer(loadExample(t, "foundation-cli.yaml"), nil).(*Deployer)
	if !ok {
		t.Fatal("the loop no longer builds a *Deployer; this check needs rewriting")
	}
	if !d.noWaits {
		t.Error("the loop waits inside a converge. The per-CR gates are the worst of it: " +
			"they run inside a step that WRITES, so a provider that never registers holds " +
			"the loop for the universe timeout and a newer spec cannot interrupt it")
	}

	ids := stepIDs(d)
	// installYBA's own wait is NOT dropped, and must not be: every CR is
	// rejected until YBA is serving and has its single customer, so skipping it
	// would only move the failure.
	if !containsStep(ids, StepYBA) {
		t.Error("the loop skips the YBA install, which is the one thing it has to wait for")
	}
	// And prune still runs: it does not need the universe settled.
	if !containsStep(ids, StepPrune) {
		t.Error("the loop no longer prunes, so removing a key from the spec stops deleting " +
			"the object — and omission-is-deletion is what makes the spec the source of truth")
	}
}

// Prune survives WithoutWaits, because the ordering it used to wait for is
// YBA's and YBA does it.
//
// A single edit can both stop the universe using a KMSConfig and drop the
// config. Deleting the config first is safe: KMSConfigReconciler refuses while
// EncryptionAtRestUtil.configInUse, writes `InUse` to the CR, and requeues the
// delete with incrementRetry false — forever. The operator's finalizer holds
// the object until the disable task has actually released it.
func TestWithoutWaitsKeepsPrune(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	ids := stepIDs(New(spec, nil).WithoutWaits())

	if !containsStep(ids, StepPrune) {
		t.Error("WithoutWaits dropped prune; the ordering it was waiting for is enforced by " +
			"YBA's own finalizer, not by us")
	}
	for _, unwanted := range []StepID{StepWaitUniverse, StepTServerService} {
		if containsStep(ids, unwanted) {
			t.Errorf("WithoutWaits kept %q", unwanted)
		}
	}
}

// With the gates gone, a CR whose reference has not registered is applied
// anyway — which is the behaviour the design turns on.
func TestWithoutWaitsAppliesPastAnUnregisteredProvider(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		// Never leaves Bootstrapping. Under the gates this polls to the timeout.
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Bootstrapping"}}`}},
		{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"success":true}}`}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.3.0-b21\n"}},
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
	}}
	spec := loadExample(t, "foundation-cli.yaml")
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, 3*time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithoutWaits()

	start := time.Now()
	if err := d.applyCRs(context.Background()); err != nil {
		t.Fatalf("applyCRs: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("applyCRs took %v — it waited on something", elapsed)
	}

	var applied []string
	for _, c := range rec.Calls {
		if c.Stdin == "" || !strings.Contains(strings.Join(c.Argv, " "), "apply") {
			continue
		}
		for _, k := range []string{"YBCertificate", "YBProvider", "YBUniverse"} {
			if strings.Contains(c.Stdin, `"kind": "`+k+`"`) {
				applied = append(applied, k)
			}
		}
	}
	if len(applied) != 3 {
		t.Fatalf("applied %v, want all three — the universe must be reached even though the "+
			"provider it names has not registered", applied)
	}
	if applied[len(applied)-1] != "YBUniverse" {
		t.Errorf("applied %v; order still has to be the spec's", applied)
	}
}

// Prune ASKS and does not wait when nothing else waits either. It has already
// issued every delete, and a CR YBA cannot release yet is requeued by YBA
// forever — so waiting would mean waiting out the universe's own task, on the
// one caller that exists in order not to wait.
func TestPruneDoesNotWaitForDeletesWhenNothingElseWaits(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	// The existence probe keeps reporting it, so any wait runs to the timeout.
	d, rec := pruneDeployer(t, spec, []Response{
		{Match: "get -f -", Result: Result{Stdout: "ybprovider.operator.yugabyte.io/gone"}},
	}, "YBProvider/gone")
	d.WithTimeouts(time.Second, time.Minute, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithoutWaits()

	start := time.Now()
	if err := d.pruneDeclared(context.Background()); err != nil {
		t.Fatalf("pruneDeclared: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("prune waited %v for a deletion it had already asked for", elapsed)
	}
	// It still ASKED — not waiting is not the same as not pruning.
	if !strings.Contains(strings.Join(rec.Argvs(), "\n"), "delete") {
		t.Error("prune did not delete anything")
	}
}

// And when it does wait, a superseded converge gives it up rather than holding
// the loop for the universe timeout. The deletes are already issued; the
// cluster finishes them whether or not anything is watching.
func TestPrunesWaitIsAbandonedWhenTheSpecIsSuperseded(t *testing.T) {
	interrupt := make(chan struct{})
	spec := loadExample(t, "foundation-cli.yaml")
	d, _ := pruneDeployer(t, spec, []Response{
		{Match: "get -f -", Result: Result{Stdout: "ybprovider.operator.yugabyte.io/gone"}},
	}, "YBProvider/gone")
	d.WithTimeouts(time.Second, time.Minute, time.Millisecond).
		WithLogger(func(string, ...any) {}).
		WithInterrupt(interrupt)

	close(interrupt)
	start := time.Now()
	_ = d.pruneDeclared(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("prune waited %v on a superseded converge", elapsed)
	}
}

func stepIDs(d *Deployer) []StepID {
	var out []StepID
	for _, s := range d.convergeSteps() {
		out = append(out, s.id)
	}
	return out
}

func containsStep(ids []StepID, want StepID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
