package k8s

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// scriptRunner records argv and answers by substring match over the joined argv.
type scriptRunner struct {
	mu    sync.Mutex
	calls [][]string
	rules []rrule
}

type rrule struct {
	match string
	res   execpkg.Result
}

func (s *scriptRunner) on(match string, res execpkg.Result) *scriptRunner {
	s.rules = append(s.rules, rrule{match, res})
	return s
}

func (s *scriptRunner) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, argv)
	joined := strings.Join(argv, " ")
	for _, r := range s.rules {
		if strings.Contains(joined, r.match) {
			return r.res, nil
		}
	}
	return execpkg.Result{ExitCode: 0}, nil
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

func testKubectl(sr *scriptRunner) *Kubectl {
	return New(Config{Context: "itest-dev", Namespace: "amp-x"}).WithRunner(sr.run)
}

func TestExecBuildsKubectlExec(t *testing.T) {
	sr := &scriptRunner{}
	k := testKubectl(sr).ForPod("ep-pod", "compute")
	if _, err := k.Run(context.Background(), "psql -c 'SELECT 1'"); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := sr.argvs()[0]
	for _, want := range []string{
		"kubectl --context itest-dev -n amp-x exec ep-pod -c compute -- sh -c psql -c 'SELECT 1'",
	} {
		if got != want {
			t.Errorf("exec argv =\n  %q\nwant\n  %q", got, want)
		}
	}
}

// TestDiagnosticsCommands: the diagnostics helpers build the expected argv
// (--previous logs, describe, sorted events) and PodStates parses the
// tab-separated name/phase/ready output into typed states.
func TestDiagnosticsCommands(t *testing.T) {
	sr := (&scriptRunner{}).
		on("logs amp-controller-0 --previous", execpkg.Result{Stdout: "crashed\n"}).
		on("describe pod amp-controller-0", execpkg.Result{Stdout: "Events:\n  BackOff\n"}).
		on("get events", execpkg.Result{Stdout: "0s Warning BackOff\n"}).
		on("get pods -o jsonpath", execpkg.Result{Stdout: "amp-controller-0\tRunning\tTrue\nyb-ep-abc\tPending\t\n"})
	k := testKubectl(sr)
	ctx := context.Background()

	if out, err := k.LogsPrevious(ctx, "amp-controller-0", "", 100); err != nil || out != "crashed\n" {
		t.Errorf("LogsPrevious = %q, %v", out, err)
	}
	if out, err := k.Describe(ctx, "pod", "amp-controller-0"); err != nil || !strings.Contains(out, "BackOff") {
		t.Errorf("Describe = %q, %v", out, err)
	}
	if out, err := k.Events(ctx); err != nil || !strings.Contains(out, "BackOff") {
		t.Errorf("Events = %q, %v", out, err)
	}
	states, err := k.PodStates(ctx)
	if err != nil {
		t.Fatalf("PodStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("PodStates count = %d, want 2 (%+v)", len(states), states)
	}
	if states[0] != (PodState{Name: "amp-controller-0", Phase: "Running", Ready: true}) {
		t.Errorf("state[0] = %+v, want Running/Ready controller", states[0])
	}
	if states[1] != (PodState{Name: "yb-ep-abc", Phase: "Pending", Ready: false}) {
		t.Errorf("state[1] = %+v, want Pending/not-ready endpoint", states[1])
	}

	assertHas(t, sr.argvs(), "logs amp-controller-0 --previous --tail 100")
	assertHas(t, sr.argvs(), "get events --sort-by=.lastTimestamp")
}

func TestRunWithoutPodErrors(t *testing.T) {
	sr := &scriptRunner{}
	if _, err := testKubectl(sr).Run(context.Background(), "true"); err == nil {
		t.Error("Run without a bound pod must error")
	}
}

func TestApplyDeleteGet(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("get statefulset", execpkg.Result{ExitCode: 0, Stdout: "1\n"})
	k := testKubectl(sr)
	if err := k.Apply(context.Background(), "kind: Service\n"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := k.Delete(context.Background(), "service", "amp-controller"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := k.Get(context.Background(), "statefulset", "yugabyted", "{.status.readyReplicas}")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "1" {
		t.Errorf("get = %q, want 1", got)
	}
	argvs := sr.argvs()
	assertHas(t, argvs, "apply -f -")
	assertHas(t, argvs, "delete service amp-controller --ignore-not-found")
	assertHas(t, argvs, "get statefulset yugabyted -o jsonpath={.status.readyReplicas}")
}

func TestNamespaceOpsDropNamespaceFlag(t *testing.T) {
	sr := &scriptRunner{}
	k := testKubectl(sr)
	if err := k.CreateNamespace(context.Background(), "amp-run-1", map[string]string{"ampqa": "1", "ampqa-created": "t0"}); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	if err := k.DeleteNamespace(context.Background(), "amp-run-1"); err != nil {
		t.Fatalf("delete ns: %v", err)
	}
	argvs := sr.argvs()
	// Neither namespace op may carry "-n amp-x" (they act at cluster scope).
	for _, a := range argvs {
		if strings.Contains(a, "-n amp-x") {
			t.Errorf("namespace op wrongly scoped to a namespace: %q", a)
		}
	}
	assertHas(t, argvs, "apply -f -")
	assertHas(t, argvs, "delete namespace amp-run-1 --ignore-not-found")
}

func TestNamespaceManifestLabelsSorted(t *testing.T) {
	m := namespaceManifest("ns1", map[string]string{"b": "2", "a": "1"})
	ai := strings.Index(m, "a:")
	bi := strings.Index(m, "b:")
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("labels not sorted deterministically:\n%s", m)
	}
	if !strings.Contains(m, "name: ns1") {
		t.Errorf("manifest missing name:\n%s", m)
	}
}

func TestWaitForAndLogs(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("logs", execpkg.Result{ExitCode: 0, Stdout: "hello\n"})
	k := testKubectl(sr)
	if err := k.WaitFor(context.Background(), "statefulset/yugabyted", "jsonpath={.status.readyReplicas}=1", 5*time.Minute); err != nil {
		t.Fatalf("wait: %v", err)
	}
	logs, err := k.Logs(context.Background(), "deployment/amp-controller", "", 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if logs != "hello\n" {
		t.Errorf("logs = %q", logs)
	}
	argvs := sr.argvs()
	assertHas(t, argvs, "wait statefulset/yugabyted --for=jsonpath={.status.readyReplicas}=1 --timeout=300s")
	assertHas(t, argvs, "logs deployment/amp-controller --tail 100")
}

func TestListPods(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("get pods -o name", execpkg.Result{ExitCode: 0, Stdout: "pod/yb-ep-1-abc\npod/yugabyted-0\n"})
	pods, err := testKubectl(sr).ListPods(context.Background())
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods) != 2 || pods[0] != "yb-ep-1-abc" || pods[1] != "yugabyted-0" {
		t.Errorf("pods = %v", pods)
	}
}

func TestNonZeroExitErrors(t *testing.T) {
	sr := &scriptRunner{}
	sr.on("get service", execpkg.Result{ExitCode: 1, Stderr: "NotFound"})
	if _, err := testKubectl(sr).Get(context.Background(), "service", "nope", "{.x}"); err == nil {
		t.Error("non-zero exit must surface as an error")
	}
}

// fakeProc is a scripted long-lived Process for PortForward tests.
type fakeProc struct {
	out     io.Reader
	killed  chan struct{}
	waitErr error
}

func (p *fakeProc) Stdout() io.Reader { return p.out }
func (p *fakeProc) Kill() error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	return nil
}
func (p *fakeProc) Wait() error { return p.waitErr }

func TestPortForwardReadyAndClose(t *testing.T) {
	sr := &scriptRunner{}
	var gotArgv []string
	proc := &fakeProc{out: strings.NewReader("Forwarding from 127.0.0.1:18080 -> 8080\n"), killed: make(chan struct{})}
	k := testKubectl(sr).WithStarter(func(_ context.Context, argv []string) (Process, error) {
		gotArgv = argv
		return proc, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/amp-controller", 18080, 8080)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	if pf.LocalAddr() != "127.0.0.1:18080" {
		t.Errorf("local addr = %q", pf.LocalAddr())
	}
	want := "kubectl --context itest-dev -n amp-x port-forward svc/amp-controller 18080:8080"
	if strings.Join(gotArgv, " ") != want {
		t.Errorf("argv =\n  %q\nwant\n  %q", strings.Join(gotArgv, " "), want)
	}
	if err := pf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-proc.killed:
	default:
		t.Error("Close must kill the port-forward process")
	}
}

func TestPortForwardExitBeforeReady(t *testing.T) {
	sr := &scriptRunner{}
	proc := &fakeProc{out: strings.NewReader(""), killed: make(chan struct{})}
	k := testKubectl(sr).WithStarter(func(_ context.Context, _ []string) (Process, error) { return proc, nil })
	if _, err := k.PortForward(context.Background(), "svc/x", 1, 2); err == nil {
		t.Error("port-forward with no readiness line must error")
	}
}

// TestPortForwardAutoLocalPort: a localPort<=0 requests kubectl's ephemeral
// ":remote" form and the actual bound port is parsed off the readiness line.
func TestPortForwardAutoLocalPort(t *testing.T) {
	sr := &scriptRunner{}
	port := 20000 + rand.Intn(40000) // the port kubectl "chose"
	proc := &fakeProc{out: strings.NewReader(fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 5432\n", port)), killed: make(chan struct{})}
	var gotArgv []string
	k := testKubectl(sr).WithStarter(func(_ context.Context, argv []string) (Process, error) {
		gotArgv = argv
		return proc, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/yb-ep-1", 0, 5432)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	if pf.LocalPort() != port {
		t.Errorf("LocalPort = %d, want parsed %d", pf.LocalPort(), port)
	}
	if pf.LocalAddr() != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Errorf("LocalAddr = %q, want the bound port", pf.LocalAddr())
	}
	if joined := strings.Join(gotArgv, " "); !strings.HasSuffix(joined, "port-forward svc/yb-ep-1 :5432") {
		t.Errorf("auto request must use kubectl ':remote' form, argv = %q", joined)
	}
}

// TestPortForwardSurvivesCtxCancel is the primary-bug regression: the forwarded
// process must be owned by the Handle, not the ctx passed to PortForward. A
// caller cancelling that ctx (as the scheduler cancels its provision ctx the
// instant Provision returns) must NOT kill a live forward — only Close does.
func TestPortForwardSurvivesCtxCancel(t *testing.T) {
	sr := &scriptRunner{}
	proc := newPipeProc("Forwarding from 127.0.0.1:34567 -> 5432\n")
	k := testKubectl(sr).WithStarter(func(_ context.Context, _ []string) (Process, error) { return proc, nil })

	ctx, cancel := context.WithCancel(context.Background())
	pf, err := k.PortForward(ctx, "svc/yb-ep-1", 0, 5432)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	if !pf.Alive() {
		t.Fatal("forward must be alive right after readiness")
	}
	cancel() // the provisioning deadline ends here — the forward must live on
	time.Sleep(20 * time.Millisecond)
	if !pf.Alive() {
		t.Fatal("forward was killed by ctx cancellation (primary bug); it must outlive the provision ctx")
	}
	if err := pf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !waitUntil(func() bool { return !pf.Alive() }, time.Second) {
		t.Error("Close must terminate the forward (Alive stays true)")
	}
}

func TestParseForwardPort(t *testing.T) {
	cases := []struct {
		line string
		port int
		ok   bool
	}{
		{"Forwarding from 127.0.0.1:54123 -> 5432", 54123, true},
		{"Forwarding from [::1]:54123 -> 5432", 54123, true},
		{"Forwarding from 127.0.0.1:8080 -> 8080", 8080, true},
		{"Handling connection for 8080", 0, false},
		{"noise", 0, false},
	}
	for _, c := range cases {
		p, ok := parseForwardPort(c.line)
		if ok != c.ok || p != c.port {
			t.Errorf("parseForwardPort(%q) = %d,%v; want %d,%v", c.line, p, ok, c.port, c.ok)
		}
	}
}

// pipeProc is a long-lived fake Process: it emits one line then keeps stdout
// open (so Alive stays true), unblocking only on Kill. Wait blocks until Kill,
// matching a real port-forward that runs until torn down.
type pipeProc struct {
	out    *io.PipeReader
	pw     *io.PipeWriter
	killed chan struct{}
	once   sync.Once
}

func newPipeProc(line string) *pipeProc {
	pr, pw := io.Pipe()
	p := &pipeProc{out: pr, pw: pw, killed: make(chan struct{})}
	go func() { _, _ = io.WriteString(pw, line) }()
	return p
}

func (p *pipeProc) Stdout() io.Reader { return p.out }
func (p *pipeProc) Kill() error {
	p.once.Do(func() {
		close(p.killed)
		_ = p.pw.Close() // EOF unblocks the scanner so watch can reap
	})
	return nil
}
func (p *pipeProc) Wait() error {
	<-p.killed
	return nil
}

func waitUntil(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func assertHas(t *testing.T, argvs []string, sub string) {
	t.Helper()
	for _, a := range argvs {
		if strings.Contains(a, sub) {
			return
		}
	}
	t.Errorf("no command contained %q\ngot: %v", sub, argvs)
}
