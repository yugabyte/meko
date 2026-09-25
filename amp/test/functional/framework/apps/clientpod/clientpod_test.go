package clientpod

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// --- fakes shared by both shapes ---

// fakeSink is a scripted apps.ArtifactSink recording every Add call.
type fakeSink struct {
	mu    sync.Mutex
	added []string // "name:kind"
	fail  bool
}

func (s *fakeSink) Add(name, kind string, content []byte) error {
	if s.fail {
		return fmt.Errorf("sink add failed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = append(s.added, name+":"+kind)
	return nil
}

func (s *fakeSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.added))
	copy(out, s.added)
	return out
}

// TestFollowLinesSplitsOnNewline covers the shape-agnostic log splitter every
// Follow implementation funnels through.
func TestFollowLinesSplitsOnNewline(t *testing.T) {
	var lines []string
	if err := followLines("a\nb\nc\n", func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("followLines: %v", err)
	}
	if want := []string{"a", "b", "c"}; !equalSlices(lines, want) {
		t.Errorf("lines = %v, want %v", lines, want)
	}
}

// TestFollowLinesHandlesLongLines: workload progress dumps exceed bufio.Scanner's
// default 64KiB line cap, which followLines raises deliberately — a regression
// here would silently truncate a workload's output mid-run.
func TestFollowLinesHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	var lines []string
	if err := followLines(long+"\n", func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("followLines: %v", err)
	}
	if len(lines) != 1 || len(lines[0]) != len(long) {
		t.Errorf("got %d lines (first %d bytes), want 1 line of %d bytes", len(lines), len(lines[0]), len(long))
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- K8s shape ---

// scriptRunner is a scripted fake k8s.Runner: records argv (and stdin), and
// answers by substring match over the joined argv (the same convention as
// exec/k8s's own test suite).
type scriptRunner struct {
	mu       sync.Mutex
	calls    [][]string
	stdins   []string
	rules    []rrule
	fallback execpkg.Result
}

type rrule struct {
	match string
	res   execpkg.Result
}

func (s *scriptRunner) on(match string, res execpkg.Result) *scriptRunner {
	s.rules = append(s.rules, rrule{match, res})
	return s
}

func (s *scriptRunner) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, argv)
	var stdinText string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		stdinText = string(b)
	}
	s.stdins = append(s.stdins, stdinText)
	joined := strings.Join(argv, " ")
	for _, r := range s.rules {
		if strings.Contains(joined, r.match) {
			return r.res, nil
		}
	}
	return s.fallback, nil
}

func (s *scriptRunner) argvs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func testKubectl(sr *scriptRunner) *k8s.Kubectl {
	return k8s.New(k8s.Config{Context: "itest-dev", Namespace: "amp-x"}).WithRunner(sr.run)
}

func TestK8sStartAppliesManifestAndWaits(t *testing.T) {
	sr := &scriptRunner{fallback: execpkg.Result{ExitCode: 0}}
	k := NewK8s(testKubectl(sr))
	err := k.Start(context.Background(), Spec{
		Name:    "wl-1",
		Image:   "amp/workloadgen:latest",
		Command: []string{"run"},
		Env:     map[string]string{"MODE": "load"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	argvs := sr.argvs()
	if len(argvs) != 2 {
		t.Fatalf("got %d kubectl calls, want 2 (apply, wait)", len(argvs))
	}
	if !strings.Contains(argvs[0], "apply -f -") {
		t.Errorf("first call = %q, want apply -f -", argvs[0])
	}
	if !strings.Contains(argvs[1], "wait pod/wl-1") || !strings.Contains(argvs[1], "condition=Ready") {
		t.Errorf("second call = %q, want a wait for pod/wl-1 Ready", argvs[1])
	}
	manifest := sr.stdins[0]
	for _, want := range []string{"kind: Pod", "name: wl-1", `ampqa: "1"`, "image: amp/workloadgen:latest", "value: \"load\""} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %q:\n%s", want, manifest)
		}
	}
}

func TestK8sStartRequiresName(t *testing.T) {
	k := NewK8s(testKubectl(&scriptRunner{}))
	if err := k.Start(context.Background(), Spec{Image: "x"}); err == nil {
		t.Error("want an error when Spec.Name is empty")
	}
}

func TestK8sCollectIntoAddsLogs(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("logs pod/wl-1", execpkg.Result{Stdout: "hello\n"})
	k := NewK8s(testKubectl(sr))
	k.name = "wl-1"
	sink := &fakeSink{}
	if err := k.CollectInto(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if names := sink.names(); len(names) != 1 || names[0] != "wl-1/clientpod.log:log" {
		t.Errorf("sink.added = %v", names)
	}
}

func TestK8sStopCollectsThenDeletes(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("logs pod/wl-1", execpkg.Result{Stdout: "hello\n"})
	k := NewK8s(testKubectl(sr))
	k.name = "wl-1"
	sink := &fakeSink{}
	if err := k.Stop(context.Background(), sink); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(sink.names()) != 1 {
		t.Fatalf("want logs collected, sink = %v", sink.names())
	}
	found := false
	for _, c := range sr.argvs() {
		if strings.Contains(c, "delete pod wl-1") {
			found = true
		}
	}
	if !found {
		t.Error("want a delete pod wl-1 call")
	}
}

func TestPodManifestNoCommandOrEnv(t *testing.T) {
	m := podManifest(Spec{Name: "p", Image: "img"})
	if strings.Contains(m, "command:") {
		t.Errorf("no Command set, manifest should omit command: block:\n%s", m)
	}
	if strings.Contains(m, "env:") {
		t.Errorf("no Env set, manifest should omit env: block:\n%s", m)
	}
	if strings.Contains(m, "imagePullSecrets") {
		t.Errorf("no ImagePullSecrets set, manifest should omit the block:\n%s", m)
	}
}

func TestPodManifestRendersImagePullSecrets(t *testing.T) {
	m := podManifest(Spec{Name: "p", Image: "quay.io/amp/wl:1", ImagePullSecrets: []string{"quay-pull", "extra-pull"}})
	for _, want := range []string{"imagePullSecrets:", "- name: quay-pull", "- name: extra-pull"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

func TestPodManifestGuaranteedAndPinned(t *testing.T) {
	m := podManifest(Spec{Name: "p", Image: "img", CPU: "2", Memory: "4Gi",
		NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "default-big"}})
	y := "  nodeSelector:\n    \"cloud.google.com/gke-nodepool\": \"default-big\"\n"
	if !strings.Contains(m, y) {
		t.Errorf("manifest missing the node selector:\n%s", m)
	}
	res := "      resources:\n        requests:\n          cpu: \"2\"\n          memory: \"4Gi\"\n        limits:\n          cpu: \"2\"\n          memory: \"4Gi\"\n"
	if !strings.Contains(m, res) {
		t.Errorf("limits must equal requests:\n%s", m)
	}
	if strings.Contains(podManifest(Spec{Name: "p", Image: "img"}), "resources:") {
		t.Error("no CPU/Memory set, manifest should omit resources")
	}
}

// TestK8sExecBindsToStartedPod: after Start, Exec runs commands INSIDE the pod
// (kubectl exec pod/<name>), the wiring the combined-stress workloads use.
func TestK8sExecBindsToStartedPod(t *testing.T) {
	sr := &scriptRunner{fallback: execpkg.Result{ExitCode: 0}}
	k := NewK8s(testKubectl(sr))
	if err := k.Start(context.Background(), Spec{Name: "wl-1", Image: "img"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := k.Exec().Run(context.Background(), "java -version"); err != nil {
		t.Fatalf("exec run: %v", err)
	}
	last := sr.argvs()[len(sr.argvs())-1]
	for _, want := range []string{"exec wl-1", "java -version"} {
		if !strings.Contains(last, want) {
			t.Errorf("exec argv missing %q: %q", want, last)
		}
	}
}

type sequenceRunner struct {
	waits []execpkg.Result
	n     int
	calls []string
}

func (s *sequenceRunner) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	joined := strings.Join(argv, " ")
	s.calls = append(s.calls, joined)
	if !strings.Contains(joined, " wait ") {
		return execpkg.Result{}, nil
	}
	if s.n < len(s.waits) {
		res := s.waits[s.n]
		s.n++
		return res, nil
	}
	return execpkg.Result{}, nil
}

const podNotFound = `Error from server (NotFound): pods "ampqa-health-1" not found`

func TestK8sStartRetriesWhileThePodIsNotVisibleYet(t *testing.T) {
	prev := podVisiblePoll
	podVisiblePoll = time.Millisecond
	t.Cleanup(func() { podVisiblePoll = prev })

	sr := &sequenceRunner{waits: []execpkg.Result{{ExitCode: 1, Stderr: podNotFound}}}
	k := NewK8s(k8s.New(k8s.Config{Namespace: "amp-x"}).WithRunner(sr.run))
	if err := k.Start(context.Background(), Spec{Name: "ampqa-health-1", Image: "img"}); err != nil {
		t.Fatalf("Start over a not-yet-visible pod: %v", err)
	}
	if sr.n != 1 {
		t.Errorf("want the NotFound wait retried, got %d scripted wait(s) consumed", sr.n)
	}
}

func TestK8sStartFailsWhenThePodNeverAppears(t *testing.T) {
	prev := podVisiblePoll
	podVisiblePoll = time.Millisecond
	t.Cleanup(func() { podVisiblePoll = prev })

	sr := &sequenceRunner{}
	for i := 0; i < 50; i++ {
		sr.waits = append(sr.waits, execpkg.Result{ExitCode: 1, Stderr: podNotFound})
	}
	k := NewK8s(k8s.New(k8s.Config{Namespace: "amp-x"}).WithRunner(sr.run))
	k.ReadyTimeout = 20 * time.Millisecond
	err := k.Start(context.Background(), Spec{Name: "ampqa-health-1", Image: "img"})
	if err == nil {
		t.Fatal("want a failure when the pod never becomes visible")
	}
	if !strings.Contains(err.Error(), "ampqa-health-1") {
		t.Errorf("error must name the pod, got %v", err)
	}
}
