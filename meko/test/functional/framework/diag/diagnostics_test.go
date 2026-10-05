package diag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// fakeKube is a scripted Kube double: it returns canned output per pod and can be
// told to error on a single item, so tests exercise the best-effort skip.
type fakeKube struct {
	ns           string
	pods         []string
	states       []k8s.PodState
	logs         map[string]string // per-pod LogsAll override (default: single prefixed "main" stream)
	logsErrPod   string            // pod whose LogsAll call errors
	eventsErr    bool
	nodesErr     bool
	nodeUsageErr bool
	wideErr      bool
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
	// Only the api-server has a prior (crashed) instance; others never restarted.
	if strings.Contains(target, "api-server") {
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

func (f *fakeKube) Nodes(_ context.Context) (string, error) {
	if f.nodesErr {
		return "", fmt.Errorf("nodes unavailable")
	}
	return "node conditions", nil
}

func (f *fakeKube) NodeUsage(_ context.Context) (string, error) {
	if f.nodeUsageErr {
		return "", fmt.Errorf("metrics-server not installed")
	}
	return "node usage", nil
}

func (f *fakeKube) Describe(_ context.Context, _, name string) (string, error) {
	return "describe:" + name, nil
}

func (f *fakeKube) Events(context.Context) (string, error) {
	if f.eventsErr {
		return "", fmt.Errorf("events API down")
	}
	return "0s Warning BackOff pod/meko-api-server-0", nil
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
		ns:   "mekoqa-fnd-test",
		pods: []string{"meko-api-server-0", "web-abc", "web-healthy", "yugabyted-0"},
		states: []k8s.PodState{
			{Name: "meko-api-server-0", Phase: "Running", Ready: true},
			{Name: "web-abc", Phase: "Pending", Ready: false},
			{Name: "web-healthy", Phase: "Running", Ready: true},
			{Name: "yugabyted-0", Phase: "Running", Ready: true},
		},
	}
}

// TestCollectK8sDiagnosticsGathersEverything: the collector Adds the event
// stream, the pods/deployments wide listings, current+previous per-container
// logs for the api-server, current logs for the web and YB pods, and a
// describe of every not-ready pod,
// all under the "Stack ..." attachment tree.
func TestCollectK8sDiagnosticsGathersEverything(t *testing.T) {
	kube := newFakeKube()
	sink := &recordSink{}
	CollectK8sDiagnostics(context.Background(), kube, "mekoqa-fnd-test", sink, nil)

	want := []string{
		"Stack events.txt",
		"Stack resources/pods.txt",
		"Stack resources/deployments.txt",
		"Stack logs/meko-api-server-0/main.log",
		"Stack logs/meko-api-server-0/main.previous.log",
		"Stack logs/web-abc/main.log",
		"Stack logs/web-healthy/main.log",
		"Universe logs/yugabyted-0/main.log",     // co-located backing routed to Universe tree
		"Universe logs/yugabyted-0/describe.txt", // backing always described
		"Stack describe/web-abc.txt",             // not-ready pod
	}
	for _, name := range want {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("missing collected artifact %q; got %v", name, keys(sink.added))
		}
	}
	// Ready non-backing stack pods are NOT described.
	if _, ok := sink.added["Stack describe/meko-api-server-0.txt"]; ok {
		t.Errorf("described a Running/Ready non-backing stack pod")
	}
	// The co-located backing is NOT filed under the Stack tree.
	if _, ok := sink.added["Stack logs/yugabyted-0/main.log"]; ok {
		t.Errorf("co-located backing should route to Universe logs, not Stack logs")
	}
	// Non-restarted pods produce no previous.log.
	if _, ok := sink.added["Stack logs/web-abc/main.previous.log"]; ok {
		t.Errorf("collected a previous.log for a pod that never restarted")
	}
}

// TestCollectK8sDiagnosticsSingleErrorDoesNotAbort: one pod's failed Logs (and a
// failed events fetch, and a sink Add error) must not stop the rest of the
// collection, every other item is still gathered.
func TestCollectK8sDiagnosticsSingleErrorDoesNotAbort(t *testing.T) {
	kube := newFakeKube()
	kube.logsErrPod = "meko-api-server-0"
	kube.eventsErr = true
	kube.wideErr = true
	sink := &recordSink{errOn: "Universe logs/yugabyted-0/main.log"}

	CollectK8sDiagnostics(context.Background(), kube, "mekoqa-fnd-test", sink, nil)

	// Events failed and the api-server current logs failed, yet the web
	// logs and the describe of the not-ready pod were still collected.
	for _, name := range []string{"Stack logs/web-abc/main.log", "Stack describe/web-abc.txt"} {
		if _, ok := sink.added[name]; !ok {
			t.Errorf("collection aborted early: missing %q; got %v", name, keys(sink.added))
		}
	}
	if _, ok := sink.added["Stack events.txt"]; ok {
		t.Errorf("events should have errored and been skipped")
	}
	if _, ok := sink.added["Stack logs/meko-api-server-0/main.log"]; ok {
		t.Errorf("api-server current logs should have errored and been skipped")
	}
}

// TestCollectUniverseK8sDiagnostics: backing YB pods land under
// "Universe logs/<pod>/<master|tserver>/<container>.log" with per-role describe
// names; non-YB pods (client pod) keep a plain pod folder; every pod is
// described; the namespace events are captured.
func TestCollectUniverseK8sDiagnostics(t *testing.T) {
	kube := &fakeKube{
		ns:   "mekoqa-yb-test",
		pods: []string{"yb-master-0", "yb-tserver-0", "workload-client"},
		states: []k8s.PodState{
			{Name: "yb-master-0", Phase: "Running", Ready: true},
			{Name: "yb-tserver-0", Phase: "Running", Ready: true},
			{Name: "workload-client", Phase: "Running", Ready: true},
		},
	}
	sink := &recordSink{}
	CollectUniverseK8sDiagnostics(context.Background(), kube, "mekoqa-yb-test", sink, nil)

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
	if _, ok := sink.added["Stack resources/pods.txt"]; ok {
		t.Errorf("universe collection produced an Stack resources listing")
	}
}

// TestCollectYBADiagnostics: the yugaware pod's multi-container log blob splits
// into one file per container under "YBA logs/<pod>/", plus describe + events.
func TestCollectYBADiagnostics(t *testing.T) {
	kube := &fakeKube{
		ns:   "mekoqa-yba-test",
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
	CollectYBADiagnostics(context.Background(), kube, "mekoqa-yba-test", sink, nil)

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

// Node state is what separates "the product hung" from "its node was starved":
// a pod can be Running and Ready with zero restarts while its node is out of
// CPU or disk, and from inside the pod the two are identical. Run 6a26ee65 lost
// three tservers to that with nothing collected to prove it either way.
func TestNodeEvidenceIsCollectedBesideTheNamespaceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		collect   func(context.Context, Kube, string, apps.ArtifactSink, func(string, ...any))
		nodes     string
		nodeUsage string
	}{
		{"stack", CollectK8sDiagnostics, "Stack nodes.txt", "Stack node-usage.txt"},
		{"universe", CollectUniverseK8sDiagnostics, "Universe logs/nodes.txt", "Universe logs/node-usage.txt"},
		{"yba", CollectYBADiagnostics, "YBA logs/nodes.txt", "YBA logs/node-usage.txt"},
	} {
		sink := &recordSink{}
		tc.collect(context.Background(), newFakeKube(), "ns", sink, nil)
		if got := string(sink.added[tc.nodes]); got != "node conditions" {
			t.Errorf("%s: %q = %q, want the node describe", tc.name, tc.nodes, got)
		}
		if got := string(sink.added[tc.nodeUsage]); got != "node usage" {
			t.Errorf("%s: %q = %q, want top nodes", tc.name, tc.nodeUsage, got)
		}
	}
}

// metrics-server is not everywhere, and node access can be denied. Neither may
// cost us the logs, the whole collection is best-effort by design.
func TestMissingNodeEvidenceDoesNotLoseTheRest(t *testing.T) {
	sink := &recordSink{}
	kube := newFakeKube()
	kube.nodesErr, kube.nodeUsageErr = true, true
	CollectK8sDiagnostics(context.Background(), kube, "ns", sink, nil)
	if _, ok := sink.added["Stack nodes.txt"]; ok {
		t.Error("a failed node lookup must attach nothing rather than an empty file")
	}
	if _, ok := sink.added["Stack events.txt"]; !ok {
		t.Error("the namespace events were lost because the node lookup failed")
	}
}
