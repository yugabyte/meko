package clientpod

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/dockerhost"
	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
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

// --- Docker shape ---

// scriptExec is a scripted fake RemoteExecutor (records commands, answers by
// substring match), the same convention used across apps/*.
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	fallback execpkg.Result
}

type rule struct {
	match string
	res   execpkg.Result
	err   error
}

func (s *scriptExec) on(match string, res execpkg.Result) *scriptExec {
	s.rules = append(s.rules, rule{match: match, res: res})
	return s
}

func (s *scriptExec) onErr(match string, err error) *scriptExec {
	s.rules = append(s.rules, rule{match: match, err: err})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (execpkg.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.res, r.err
		}
	}
	return s.fallback, nil
}

func (s *scriptExec) Copy(context.Context, string, string) error { return nil }

func (s *scriptExec) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

func TestDockerStartRunsDetachedContainer(t *testing.T) {
	ex := &scriptExec{fallback: execpkg.Result{ExitCode: 0, Stdout: "cid\n"}}
	d := NewDocker(dockerhost.New(ex))
	err := d.Start(context.Background(), Spec{
		Name:    "wl-1",
		Image:   "amp/workloadgen:latest",
		Command: []string{"run", "--seed", "1"},
		Env:     map[string]string{"MODE": "random_ops"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	got := ex.recorded()[0]
	for _, want := range []string{"docker run -d", "--name 'wl-1'", "'amp/workloadgen:latest'", "-e 'MODE=random_ops'", "'run' '--seed' '1'"} {
		if !strings.Contains(got, want) {
			t.Errorf("start command missing %q:\n%s", want, got)
		}
	}
}

func TestDockerStartRequiresName(t *testing.T) {
	d := NewDocker(dockerhost.New(&scriptExec{}))
	if err := d.Start(context.Background(), Spec{Image: "x"}); err == nil {
		t.Error("want an error when Spec.Name is empty")
	}
}

func TestDockerCollectIntoAddsLogs(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker logs", execpkg.Result{Stdout: "line1\nline2\n"})
	d := NewDocker(dockerhost.New(ex))
	d.name = "wl-1"
	sink := &fakeSink{}
	if err := d.CollectInto(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if names := sink.names(); len(names) != 1 || names[0] != "wl-1/clientpod.log:log" {
		t.Errorf("sink.added = %v, want [wl-1/clientpod.log:log]", names)
	}
}

func TestDockerStopCollectsBeforeRemoving(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker logs", execpkg.Result{Stdout: "output\n"})
	d := NewDocker(dockerhost.New(ex))
	d.name = "wl-1"
	sink := &fakeSink{}
	if err := d.Stop(context.Background(), sink); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(sink.names()) != 1 {
		t.Fatalf("want logs collected before stop, sink = %v", sink.names())
	}
	found := false
	for _, c := range ex.recorded() {
		if strings.Contains(c, "docker rm -f") && strings.Contains(c, "wl-1") {
			found = true
		}
	}
	if !found {
		t.Error("want a docker rm -f wl-1 command")
	}
}

func TestDockerStopStillStopsWhenCollectFails(t *testing.T) {
	ex := &scriptExec{}
	ex.onErr("docker logs", fmt.Errorf("boom"))
	d := NewDocker(dockerhost.New(ex))
	d.name = "wl-1"
	sink := &fakeSink{}
	err := d.Stop(context.Background(), sink)
	if err == nil || !strings.Contains(err.Error(), "collect before stop") {
		t.Fatalf("want a collect-before-stop error, got %v", err)
	}
	found := false
	for _, c := range ex.recorded() {
		if strings.Contains(c, "docker rm -f") {
			found = true
		}
	}
	if !found {
		t.Error("stop must still run even when collect failed")
	}
}

func TestFollowLinesSplitsOnNewline(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker logs", execpkg.Result{Stdout: "a\nb\nc\n"})
	d := NewDocker(dockerhost.New(ex))
	d.name = "wl-1"
	var lines []string
	if err := d.Follow(context.Background(), func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if want := []string{"a", "b", "c"}; !equalSlices(lines, want) {
		t.Errorf("lines = %v, want %v", lines, want)
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
