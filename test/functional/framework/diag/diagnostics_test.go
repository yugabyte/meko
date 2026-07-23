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
	ns         string
	pods       []string
	states     []k8s.PodState
	logs       map[string]string // per-pod LogsAll override (default: single prefixed "main" stream)
	logsErrPod string            // pod whose LogsAll call errors
	eventsErr  bool
	wideErr    bool
}

func (f *fakeKube) Namespace() string { return f.ns }

func (f *fakeKube) ListPods(context.Context) ([]string, error) { return f.pods, nil }

func (f *fakeKube) PodStates(context.Context) ([]k8s.PodState, error) { return f.states, nil }

func (f *fakeKube) LogsAll(_ context.Context, target string, _ int) (string, error) {
	if target == f.logsErrPod {
		return "", fmt.Errorf("logs unavailable for %s", target)
	}
	if out, ok := f.logs[target]; ok {
		return out, nil
	}
	return "[pod/" + target + "/main] current-logs:" + target, nil
}

func (f *fakeKube) LogsAllPrevious(_ context.Context, target string, _ int) (string, error) {
	// Only the controller has a prior (crashed) instance; others never restarted.
	if strings.Contains(target, "controller") {
		return "[pod/" + target + "/main] previous-logs:" + target, nil
	}
	return "", fmt.Errorf("previous terminated container %q not found", target)
}

func (f *fakeKube) Wide(_ context.Context, kind string) (string, error) {
	if f.wideErr {
		return "", fmt.Errorf("wide listing unavailable")
	}
	return "wide:" + kind, nil
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
		ns:   "ampqa-fnd-test",
		pods: []string{"amp-controller-0", "yb-ep-abc", "yb-ep-healthy", "yugabyted-0"},
		states: []k8s.PodState{
			{Name: "amp-controller-0", Phase: "Running", Ready: true},
			{Name: "yb-ep-abc", Phase: "Pending", Ready: false},
			{Name: "yb-ep-healthy", Phase: "Running", Ready: true},
			{Name: "yugabyted-0", Phase: "Running", Ready: true},
		},
	}
}

// TestCollectK8sDiagnosticsGathersEverything: the collector Adds the event
// stream, the pods/deployments wide listings, current+previous per-container
// logs for the controller, current logs for the endpoint and YB pods, and a
// describe of every not-ready pod AND every yb-ep-* pod (even Ready ones) —
// all under the "Amp ..." attachment tree.
func TestCollectK8sDiagnosticsGathersEverything(t *testing.T) {
	kube := newFakeKube()
	sink := &recordSink{}
	CollectK8sDiagnostics(context.Background(), kube, "ampqa-fnd-test", sink, nil)

	want := []string{
		"Amp events.txt",
		"Amp resources/pods.txt",
		"Amp resources/deployments.txt",
		"Amp logs/amp-controller-0/main.log",
		"Amp logs/amp-controller-0/main.previous.log",
		"Amp logs/yb-ep-abc/main.log",
		"Amp logs/yb-ep-healthy/main.log",
		"Universe logs/yugabyted-0/main.log",     // co-located backing routed to Universe tree
		"Universe logs/yugabyted-0/describe.txt", // backing always described
		"Amp describe/yb-ep-abc.txt",             // not-ready pod
		"Amp describe/yb-ep-healthy.txt",         // endpoint pods always described
	}
	for _, name := range want {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("missing collected artifact %q; got %v", name, keys(sink.added))
		}
	}
	// Ready non-endpoint, non-backing amp pods are NOT described.
	if _, ok := sink.added["Amp describe/amp-controller-0.txt"]; ok {
		t.Errorf("described a Running/Ready non-endpoint amp pod")
	}
	// The co-located backing is NOT filed under the Amp tree.
	if _, ok := sink.added["Amp logs/yugabyted-0/main.log"]; ok {
		t.Errorf("co-located backing should route to Universe logs, not Amp logs")
	}
	// Non-restarted pods produce no previous.log.
	if _, ok := sink.added["Amp logs/yb-ep-abc/main.previous.log"]; ok {
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
	kube.wideErr = true
	sink := &recordSink{errOn: "Universe logs/yugabyted-0/main.log"}

	CollectK8sDiagnostics(context.Background(), kube, "ampqa-fnd-test", sink, nil)

	// Events failed and the controller's current logs failed, yet the endpoint
	// logs and the describe of the not-ready pod were still collected.
	for _, name := range []string{"Amp logs/yb-ep-abc/main.log", "Amp describe/yb-ep-abc.txt"} {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("collection aborted early: missing %q; got %v", name, keys(sink.added))
		}
	}
	if _, ok := sink.added["Amp events.txt"]; ok {
		t.Errorf("events should have errored and been skipped")
	}
	if _, ok := sink.added["Amp logs/amp-controller-0/main.log"]; ok {
		t.Errorf("controller current logs should have errored and been skipped")
	}
}

// TestCollectUniverseK8sDiagnostics: backing YB pods land under
// "Universe logs/<pod>/<master|tserver>/<container>.log" with per-role describe
// names; non-YB pods (client pod) keep a plain pod folder; every pod is
// described; the namespace events are captured.
func TestCollectUniverseK8sDiagnostics(t *testing.T) {
	kube := &fakeKube{
		ns:   "ampqa-yb-test",
		pods: []string{"yb-master-0", "yb-tserver-0", "workload-client"},
		states: []k8s.PodState{
			{Name: "yb-master-0", Phase: "Running", Ready: true},
			{Name: "yb-tserver-0", Phase: "Running", Ready: true},
			{Name: "workload-client", Phase: "Running", Ready: true},
		},
	}
	sink := &recordSink{}
	CollectUniverseK8sDiagnostics(context.Background(), kube, "ampqa-yb-test", sink, nil)

	want := []string{
		"Universe logs/events.txt",
		"Universe logs/yb-master-0/master/main.log",
		"Universe logs/yb-tserver-0/tserver/main.log",
		"Universe logs/workload-client/main.log",
		"Universe logs/yb-master-0/master.describe.txt",
		"Universe logs/yb-tserver-0/tserver.describe.txt",
		"Universe logs/workload-client/describe.txt",
	}
	for _, name := range want {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("missing collected artifact %q; got %v", name, keys(sink.added))
		}
	}
	// The universe tree carries no wide listings.
	if _, ok := sink.added["Amp resources/pods.txt"]; ok {
		t.Errorf("universe collection produced an Amp resources listing")
	}
}

// TestCollectYBADiagnostics: the yugaware pod's multi-container log blob splits
// into one file per container under "YBA logs/<pod>/", plus describe + events.
func TestCollectYBADiagnostics(t *testing.T) {
	kube := &fakeKube{
		ns:   "ampqa-yba-test",
		pods: []string{"yugaware-0"},
		states: []k8s.PodState{
			{Name: "yugaware-0", Phase: "Running", Ready: true},
		},
		logs: map[string]string{
			"yugaware-0": "[pod/yugaware-0/yugaware] platform line\n" +
				"[pod/yugaware-0/prometheus] scrape line\n" +
				"[pod/yugaware-0/postgres] pg line",
		},
	}
	sink := &recordSink{}
	CollectYBADiagnostics(context.Background(), kube, "ampqa-yba-test", sink, nil)

	want := []string{
		"YBA logs/events.txt",
		"YBA logs/yugaware-0/yugaware.log",
		"YBA logs/yugaware-0/prometheus.log",
		"YBA logs/yugaware-0/postgres.log",
		"YBA logs/yugaware-0/describe.txt",
	}
	for _, name := range want {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("missing collected artifact %q; got %v", name, keys(sink.added))
		}
	}
	if got := string(sink.added["YBA logs/yugaware-0/prometheus.log"]); got != "scrape line" {
		t.Errorf("prometheus container log = %q, want %q", got, "scrape line")
	}
}

// TestSplitByContainer: prefixed lines split per container, continuation
// (unprefixed) lines stick to the last-seen container, and a fully unprefixed
// blob falls back to the "pod" stream.
func TestSplitByContainer(t *testing.T) {
	got := splitByContainer("[pod/p/a] one\nraw continuation\n[pod/p/b] two")
	if got["a"] != "one\nraw continuation" || got["b"] != "two" {
		t.Errorf("split = %#v", got)
	}
	got = splitByContainer("no prefix at all")
	if got["pod"] != "no prefix at all" || len(got) != 1 {
		t.Errorf("unprefixed split = %#v", got)
	}
	if got = splitByContainer(""); len(got) != 0 {
		t.Errorf("empty blob split = %#v", got)
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
