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
