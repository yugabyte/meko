package core_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
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
		{"0/10 nodes are available: 6 node(s) didn't match PersistentVolume's node affinity",
			"pod unschedulable (persistentvolume node affinity)"},
		{"0/12 nodes are available: 12 Insufficient memory.", "pod unschedulable (insufficient memory)"},
		{"helm install exited 1: Error: INSTALLATION FAILED: context deadline exceeded", "helm install timed out waiting for readiness"},
		{"resource StatefulSet/ampqa-yb-abc/yb-master not ready Ready: 0/3 ... context deadline exceeded",
			"helm install timed out waiting for readiness"},
		{"error: timed out waiting for the condition on statefulsets/yb-tserver", "readiness wait timed out"},
		{"yugabyted cluster never became ready: deadline", "readiness wait timed out"},
		{"tservers not Ready before workload: exit 1", "readiness wait timed out"},
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
	} {
		if reason, ok := core.SchedulingReason(msg); ok {
			t.Errorf("SchedulingReason(%q) matched %q, want no match", msg, reason)
		}
	}
}
