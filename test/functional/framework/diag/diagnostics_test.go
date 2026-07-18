package diag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// fakeKube is a scripted Kube double: it returns canned output per pod and can be
// told to error on a single item, so tests exercise the best-effort skip.
type fakeKube struct {
	pods       []string
	states     []k8s.PodState
	logsErrPod string // pod whose Logs call errors
	eventsErr  bool
}

func (f *fakeKube) Namespace() string { return "ampqa-fnd-test" }

func (f *fakeKube) ListPods(context.Context) ([]string, error) { return f.pods, nil }

func (f *fakeKube) PodStates(context.Context) ([]k8s.PodState, error) { return f.states, nil }

func (f *fakeKube) Logs(_ context.Context, target, _ string, _ int) (string, error) {
	if target == f.logsErrPod {
		return "", fmt.Errorf("logs unavailable for %s", target)
	}
	return "current-logs:" + target, nil
}

func (f *fakeKube) LogsPrevious(_ context.Context, target, _ string, _ int) (string, error) {
	// Only the controller has a prior (crashed) instance; others never restarted.
	if strings.Contains(target, "controller") {
		return "previous-logs:" + target, nil
	}
	return "", fmt.Errorf("previous terminated container %q not found", target)
}

func (f *fakeKube) Describe(_ context.Context, _, name string) (string, error) {
	return "describe:" + name, nil
}

func (f *fakeKube) Events(context.Context) (string, error) {
	if f.eventsErr {
		return "", fmt.Errorf("events API down")
	}
	return "0s Warning BackOff pod/amp-controller-0", nil
}

// recordSink captures every Add for assertion.
type recordSink struct {
	added map[string][]byte
	errOn string // name that errors on Add
}

func (s *recordSink) Add(name, _ string, content []byte) error {
	if name == s.errOn {
		return fmt.Errorf("sink full")
	}
	if s.added == nil {
		s.added = map[string][]byte{}
	}
	s.added[name] = content
	return nil
}

func newFakeKube() *fakeKube {
	return &fakeKube{
		pods: []string{"amp-controller-0", "yb-ep-abc", "yugabyted-0"},
		states: []k8s.PodState{
			{Name: "amp-controller-0", Phase: "Running", Ready: true},
			{Name: "yb-ep-abc", Phase: "Pending", Ready: false},
			{Name: "yugabyted-0", Phase: "Running", Ready: true},
		},
	}
}

// TestCollectK8sDiagnosticsGathersEverything: the collector Adds the event
// stream, current+previous logs for the controller, current logs for the
// endpoint and YB pods, and a describe of ONLY the not-ready pod.
func TestCollectK8sDiagnosticsGathersEverything(t *testing.T) {
	kube := newFakeKube()
	sink := &recordSink{}
	CollectK8sDiagnostics(context.Background(), kube, "ampqa-fnd-test", sink, nil)

	want := []string{
		"ampqa-fnd-test-events.txt",
		"ampqa-fnd-test-amp-controller-0.log",
		"ampqa-fnd-test-amp-controller-0.previous.log",
		"ampqa-fnd-test-yb-ep-abc.log",
		"ampqa-fnd-test-yugabyted-0.log",
		"ampqa-fnd-test-yb-ep-abc.describe.txt", // the only not-ready pod
	}
	for _, name := range want {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("missing collected artifact %q; got %v", name, keys(sink.added))
		}
	}
	// Ready pods are NOT described.
	for _, name := range []string{"ampqa-fnd-test-amp-controller-0.describe.txt", "ampqa-fnd-test-yugabyted-0.describe.txt"} {
		if _, ok := sink.added[name]; ok {
			t.Errorf("described a Running/Ready pod: %q", name)
		}
	}
	// Non-restarted pods produce no previous.log.
	if _, ok := sink.added["ampqa-fnd-test-yb-ep-abc.previous.log"]; ok {
		t.Errorf("collected a previous.log for a pod that never restarted")
	}
}

// TestCollectK8sDiagnosticsSingleErrorDoesNotAbort: one pod's failed Logs (and a
// failed events fetch, and a sink Add error) must not stop the rest of the
// collection — every other item is still gathered.
func TestCollectK8sDiagnosticsSingleErrorDoesNotAbort(t *testing.T) {
	kube := newFakeKube()
	kube.logsErrPod = "amp-controller-0"
	kube.eventsErr = true
	sink := &recordSink{errOn: "ampqa-fnd-test-yugabyted-0.log"}

	CollectK8sDiagnostics(context.Background(), kube, "ampqa-fnd-test", sink, nil)

	// Events failed and the controller's current logs failed, yet the endpoint
	// logs and the describe of the not-ready pod were still collected.
	for _, name := range []string{"ampqa-fnd-test-yb-ep-abc.log", "ampqa-fnd-test-yb-ep-abc.describe.txt"} {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("collection aborted early: missing %q; got %v", name, keys(sink.added))
		}
	}
	if _, ok := sink.added["ampqa-fnd-test-events.txt"]; ok {
		t.Errorf("events should have errored and been skipped")
	}
	if _, ok := sink.added["ampqa-fnd-test-amp-controller-0.log"]; ok {
		t.Errorf("controller current logs should have errored and been skipped")
	}
}

// TestShouldCollectGate: the on_failure default collects only on failure; always
// always collects; never never does; an unknown mode falls back to on_failure.
func TestShouldCollectGate(t *testing.T) {
	cases := []struct {
		mode   string
		failed bool
		want   bool
	}{
		{ModeOnFailure, true, true},
		{ModeOnFailure, false, false},
		{ModeAlways, false, true},
		{ModeAlways, true, true},
		{ModeNever, true, false},
		{ModeNever, false, false},
		{"", true, true}, // default → on_failure
		{"garbage", false, false},
	}
	for _, c := range cases {
		if got := ShouldCollect(c.mode, c.failed); got != c.want {
			t.Errorf("ShouldCollect(%q, %v) = %v, want %v", c.mode, c.failed, got, c.want)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
