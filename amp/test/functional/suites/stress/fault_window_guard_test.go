package stress

import (
	"os"
	"strings"
	"testing"
)

// Both durability tests recorded acked_under_fault and never required it to be
// non-zero. A fault window nobody wrote through leaves the exact-set check
// afterwards verifying commits that never met a fault — green, and proof of
// nothing. Faults landing and commits crossing them are two separate
// preconditions and both have to be enforced.
func TestFaultWindowsRequireCommitsThroughThem(t *testing.T) {
	for _, tc := range []struct{ file, injected string }{
		{"commit_durability.go", "injects == 0"},
		{"storage_proc_nemesis.go", "len(records) == 0"},
	} {
		b, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		body := string(b)
		if !strings.Contains(body, tc.injected) {
			t.Errorf("%s no longer requires a fault to have been injected", tc.file)
		}
		if !strings.Contains(body, "ackedUnderFault == 0") {
			t.Errorf("%s records acked_under_fault but does not require it: a fault window with no commits through it passes and proves nothing", tc.file)
		}
		// It has to be the DELTA over the window. Both files seed writes before
		// the fault and refuse to proceed with none, so a cumulative total can
		// never be zero and the guard above would be unreachable.
		if !strings.Contains(body, "totalAcked(journals) - ") {
			t.Errorf("%s measures acked_under_fault cumulatively, so the guard can never fire", tc.file)
		}
	}
}

// A health check that cannot run must not report health. Returning success
// without a kube context is how "the deployment still stands" passes without
// looking at the deployment.
func TestHealthCheckWithoutKubeIsNotSuccess(t *testing.T) {
	b, err := os.ReadFile("multitenant_helper.go")
	if err != nil {
		t.Fatalf("read multitenant_helper.go: %v", err)
	}
	i := strings.Index(string(b), "func assertAmpHealthy(")
	if i < 0 {
		t.Fatal("assertAmpHealthy is gone")
	}
	fn := string(b)[i : i+700]
	if !strings.Contains(fn, "core.AsEnvFailure") {
		t.Error("a missing kube context no longer fails: the pod-health assertion silently passes without checking any pod")
	}
}

// bulk_load_kill's equivalent precondition: its rounds assert atomicity, but a
// batch that had already committed when the pod was deleted was never torn by
// anything, so a run where that happened every round proves nothing.
func TestBulkLoadKillRequiresAKillInFlight(t *testing.T) {
	b, err := os.ReadFile("bulk_load_kill.go")
	if err != nil {
		t.Fatalf("read bulk_load_kill.go: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "landed == 0") {
		t.Error("bulk_load_kill no longer requires one round's write to have been in flight when the pod was deleted")
	}
	// landed is only set once the load is known not to have finished; without
	// that check the guard counts kills that hit an already-committed batch.
	if !strings.Contains(body, "if !finished {") {
		t.Error("bulk_load_kill no longer separates a mid-flight kill from a load that had already finished")
	}
	// A compute that OOMs or panics on its own only reaches the client as a
	// dropped connection, which reads like a network problem. Every step is
	// checked for it, not just the rounds: the step that first caught one was
	// verify_dataset, reading the table back with no fault injected.
	if !strings.Contains(body, "endpointSelfRestart(t, kube, ten)") {
		t.Error("bulk_load_kill no longer checks whether the compute container died on its own after each step")
	}
	// A backing out of disk rejects every write the compute attempts and wedges
	// it, which looks like a product failure and is not one. Three runs were
	// triaged as an amp bug before the log said "insufficient disk space".
	if !strings.Contains(body, "core.AsEnvFailure") || !strings.Contains(body, "backingDiskFullSignature") {
		t.Error("bulk_load_kill no longer classifies a backing that ran out of disk as an environment failure")
	}
	// "could not find a server to connect to" is BOTH the cluster moving the
	// backing pod and amp caching a tserver hostname that no longer resolves
	// (amp#811). Only the pod's identity separates them, so the classification
	// must consult it — calling every instance environmental would mask that bug.
	if !strings.Contains(body, "backingReplaced(t, backingWas, logf)") {
		t.Error("bulk_load_kill no longer distinguishes a replaced backing pod from amp losing track of one that never moved")
	}
	// One tserver's uid answers "same pod" for the two it never looked at.
	if !strings.Contains(body, "func backingPodUIDs(") {
		t.Error("bulk_load_kill identifies fewer than all of the backing pods, so a replaced one can pass as unchanged")
	}
	// The thin-client lanes resolve the tservers themselves, so the DNS failures count too.
	for _, sig := range []string{"Host not found (authoritative)", "Resolve failed"} {
		if !strings.Contains(body, sig) {
			t.Errorf("bulk_load_kill no longer recognises %q as the compute failing to reach the backing", sig)
		}
	}
	// A client with no reply yet is not the same as an open transaction: a commit
	// whose acknowledgement is still on the wire looks identical from the client,
	// and killing after a commit tests durability, not atomicity. The claim needs
	// the server's view, sampled at the instant of the kill.
	if !strings.Contains(body, "sampleOpenTxn(t.Context, ten, obs, s)") {
		t.Error("bulk_load_kill no longer asks the server whether a transaction was open when it deleted the pod")
	}
	if !strings.Contains(body, "observed == 0") {
		t.Error("bulk_load_kill no longer requires any round to have observed an open transaction at the kill")
	}
}

// Teardown deletes the tenants' endpoint pods, so a burst that acked nothing has to take their logs and the writers' error text with it (1275e33d was triaged blind without them).
func TestWalTailNemesisKeepsTheEvidenceOfARefusedWrite(t *testing.T) {
	b, err := os.ReadFile("wal_tail_nemesis.go")
	if err != nil {
		t.Fatalf("read wal_tail_nemesis.go: %v", err)
	}
	body := string(b)
	if !strings.Contains(body, "attachEndpointDiagnostics(t, kube, ten,") {
		t.Error("wal_tail_nemesis no longer attaches the tenants' endpoint logs when a burst acks nothing: teardown deletes those pods before the run collects anything")
	}
	if !strings.Contains(body, "journals[i].LastErr()") {
		t.Error("wal_tail_nemesis no longer reports why the writes failed: every failure is recorded as an unknown outcome, so a hung COMMIT and a refused connection are the same journal")
	}
	// Round 1 has written nothing yet; from round 2 the previous round recovered and verified.
	if !strings.Contains(body, "no COMMIT completes (cf. meko#727): %s") {
		t.Error("wal_tail_nemesis reports a post-recovery burst that acked nothing as a missing precondition rather than as a product failure")
	}
}
