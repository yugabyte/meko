package core_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func TestEnvFailWrapsAndStaysTransparent(t *testing.T) {
	cause := errors.New("helm install exited 1")
	err := core.EnvFail("provision yb in k8s: %w", cause)
	if !core.IsEnvFailure(err) {
		t.Error("EnvFail result is not an env failure")
	}
	if !errors.Is(err, cause) {
		t.Error("EnvFail lost the cause")
	}
	if want := "provision yb in k8s: helm install exited 1"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	// Still an env failure through another wrap — the call chain re-wraps freely.
	if !core.IsEnvFailure(fmt.Errorf("phase provision_backing: %w", err)) {
		t.Error("env marking must survive re-wrapping")
	}
}

func TestAsEnvFailureKeepsTextAndIsIdempotent(t *testing.T) {
	plain := errors.New("tservers not Ready")
	marked := core.AsEnvFailure(plain)
	if !core.IsEnvFailure(marked) || marked.Error() != plain.Error() {
		t.Errorf("AsEnvFailure = %q (env=%v), want same text marked", marked, core.IsEnvFailure(marked))
	}
	if core.AsEnvFailure(marked) != marked {
		t.Error("AsEnvFailure must pass an already-marked error through unchanged")
	}
	if core.AsEnvFailure(nil) != nil {
		t.Error("AsEnvFailure(nil) must be nil")
	}
	if core.IsEnvFailure(plain) {
		t.Error("a plain error must not read as an env failure")
	}
}

// A self-provisioning test that marks its setup error reports env_failed.
func TestBodyEnvFailureIsEnvFailed(t *testing.T) {
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error {
		return core.EnvFail("provision yb in k8s: %w", errors.New("helm install exited 1"))
	}}, &fake.Provider{}, core.DetectedVersions{})
	if r.Status != core.StatusEnvFailed {
		t.Errorf("status = %s, want env_failed", r.Status)
	}
	if r.ErrorSummary != "provision yb in k8s: helm install exited 1" {
		t.Errorf("error summary = %q", r.ErrorSummary)
	}
}

// The other direction: an unmarked body error is still a product signal.
func TestBodyPlainErrorStaysFailed(t *testing.T) {
	r := runOne(t, core.Test{Name: "t", Run: func(*core.T) error {
		return errors.New("consistency verify failed on 3/10 tables")
	}}, &fake.Provider{}, core.DetectedVersions{})
	if r.Status != core.StatusFailed {
		t.Errorf("status = %s, want failed", r.Status)
	}
}

func TestSchedulingReasonMatchesCapacityFailures(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"0/10 nodes are available: 4 Insufficient cpu, 6 node(s) didn't match PersistentVolume's node affinity.",
			"pod unschedulable (insufficient cpu)"},
		{"pod yugaware-0 is Unschedulable", "pod unschedulable"},
		{"port-forward svc/yb-ep-1 not ready: exit status 1 (kubectl: Unable to connect to the server: dial tcp 10.150.8.6:443: i/o timeout)",
			"kube api server unreachable"},
		{"port-forward svc/yb-ep-1 not ready: exit status 1 (kubectl: The connection to the server 10.150.8.6:443 was refused - did you specify the right host or port?)",
			"kube api server unreachable"},
		{"0/10 nodes are available: 6 node(s) didn't match PersistentVolume's node affinity",
			"pod unschedulable (persistentvolume node affinity)"},
		{"0/12 nodes are available: 12 Insufficient memory.", "pod unschedulable (insufficient memory)"},
		{"helm install exited 1: Error: INSTALLATION FAILED: context deadline exceeded", "helm install timed out waiting for readiness"},
		{"resource StatefulSet/ampqa-yb-abc/yb-master not ready Ready: 0/3 ... context deadline exceeded",
			"helm install timed out waiting for readiness"},
		{"error: timed out waiting for the condition on statefulsets/yb-tserver", "readiness wait timed out"},
		{"yugabyted cluster never became ready: deadline", "readiness wait timed out"},
		{"tservers not Ready before workload: exit 1", "readiness wait timed out"},
		{"copy into mt_0: unexpected EOF [env: connection dropped mid-statement but compute pod yb-ep-1 survived it (uid unchanged, 0 container restarts, same postmaster since 2026-09-15 11:00:43+00) and postgres accepts again: harness port-forward drop, not a product failure]",
			"attested harness port-forward drop"},
		{`provision_yba: create universe "ampqa-70c748e9": task 5643 (Created Universe) ended Failure: ConfigureUniverse: Failed to execute task {...}, hit error: Could not upload the ybc contents.`,
			"yba universe create task failed"},
	}
	for _, c := range cases {
		got, ok := core.SchedulingReason(c.msg)
		if !ok || got != c.want {
			t.Errorf("SchedulingReason(%q) = (%q, %v), want (%q, true)", c.msg, got, ok, c.want)
		}
	}
}

// The load-bearing half: product, credential and image errors must not retry.
func TestSchedulingReasonRejectsNonCapacityFailures(t *testing.T) {
	for _, msg := range []string{
		"",
		"consistency verify failed on 3/10 table(s) after combined load + chaos",
		"perf regression vs baseline: tps -31%",
		"[XX000] Timed out waiting for a leader",
		"create universe: status 401: unauthorized",
		"pull access denied for quay.io/yugabyte/yugabyte-itest",
		"Back-off pulling image: ImagePullBackOff",
		"apps.ybk8s.chart_version is empty and versions.yb is unset",
		"x509: certificate signed by unknown authority",
		// Unschedulable BECAUSE the image never resolves — still no retry.
		"pod unschedulable; last event: ErrImagePull for amp-controller:custom-nope",
		`controller: yba task 5643 (Edit Universe) ended Failure: ConfigureUniverse`,
		// An unattested conn drop is a product signal; only the attestation retries.
		"copy into mt_0: unexpected EOF",
		// The phrase quoted by a workload, not emitted by our own kubectl.
		"consistency verify failed: log line: Unable to connect to the server",
	} {
		if reason, ok := core.SchedulingReason(msg); ok {
			t.Errorf("SchedulingReason(%q) matched %q, want no match", msg, reason)
		}
	}
}

func TestKubeAPIUnreachableIsTypedAndSole(t *testing.T) {
	kube := &k8s.APIServerUnreachableError{
		Err: errors.New("port-forward svc/yb-ep-1 not ready: exit status 1 (kubectl: Unable to connect to the server: dial tcp 10.150.8.6:443: i/o timeout)"),
	}
	product := errors.New("amcheck: heap tuple is not in the index")

	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"typed", kube, true},
		{"wrapped", fmt.Errorf("connect the fork's endpoint: %w", kube), true},
		{"joined with itself", errors.Join(kube, kube), true},
		{"nil", nil, false},
		{"text only", errors.New("log line: Unable to connect to the server: i/o timeout"), false},
		{"joined with a finding", errors.Join(kube, product), false},
		{"a wrapped join carrying a finding", fmt.Errorf("check target: %w", errors.Join(kube, product)), false},
	} {
		if got := core.KubeAPIUnreachable(c.err); got != c.want {
			t.Errorf("%s: KubeAPIUnreachable = %v, want %v", c.name, got, c.want)
		}
	}
}
