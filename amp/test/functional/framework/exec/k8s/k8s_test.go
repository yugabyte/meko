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

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
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
	if err := k.DeleteNow(context.Background(), "yb-ep-1-abc"); err != nil {
		t.Fatalf("delete now: %v", err)
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
	assertHas(t, argvs, "delete pod yb-ep-1-abc --ignore-not-found --force --grace-period=0")
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
	// Guarded: a forward that dies is relaunched from the watcher goroutine, so
	// the starter is called again while the test is still reading what it saw.
	var argvMu sync.Mutex
	var gotArgv []string
	k := testKubectl(sr).WithStarter(func(_ context.Context, argv []string) (Process, error) {
		argvMu.Lock()
		gotArgv = argv
		argvMu.Unlock()
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
	argvMu.Lock()
	joinedArgv := strings.Join(gotArgv, " ")
	argvMu.Unlock()
	if joined := joinedArgv; !strings.HasSuffix(joined, "port-forward svc/yb-ep-1 :5432") {
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

// TestRolloutStatus: the argv is the plain `rollout status kind/name` with a
// kubectl-shaped timeout, and a failed rollout errors with kubectl's own output
// attached instead of being swallowed.
func TestRolloutStatus(t *testing.T) {
	sr := &scriptRunner{}
	k := testKubectl(sr)
	if err := k.RolloutStatus(context.Background(), "deployment", "amp-controller", 90*time.Second); err != nil {
		t.Fatalf("RolloutStatus: %v", err)
	}
	assertHas(t, sr.argvs(), "rollout status deployment/amp-controller --timeout=90s")

	failing := &scriptRunner{}
	failing.on("rollout status", execpkg.Result{
		ExitCode: 1,
		Stdout:   "Waiting for deployment \"amp-controller\" rollout to finish: 1 old replicas are pending termination...\n",
		Stderr:   "error: timed out waiting for the condition",
	})
	err := testKubectl(failing).RolloutStatus(context.Background(), "deployment", "amp-controller", time.Second)
	if err == nil {
		t.Fatal("a failed rollout must error")
	}
	if !strings.Contains(err.Error(), "timed out waiting for the condition") {
		t.Errorf("error = %q, want kubectl's stderr in it", err)
	}
	if !strings.Contains(err.Error(), "old replicas are pending termination") {
		t.Errorf("error = %q, want kubectl's progress output in it", err)
	}
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

// A forward that never reports a bound local port must fail where kubectl and
// the target are still known. It used to succeed and hand back port 0, so the
// caller's dial failed naming neither.
func TestPortForwardEphemeralWithoutPortIsAnError(t *testing.T) {
	k := New(Config{Context: "x", Namespace: "ns"}).WithStarter(func(_ context.Context, _ []string) (Process, error) {
		return &fakeProc{out: strings.NewReader("Forwarding from something-unparseable -> 5433\n"), killed: make(chan struct{})}, nil
	})
	if _, err := k.PortForward(context.Background(), "pod/p", 0, 5433); err == nil {
		t.Fatal("a ready line with no parseable local port must be an error")
	}
}

// kubectl says why a forward failed, and that used to be dropped: the run saw a
// bare ECONNREFUSED with no cause. The handle keeps the tail and the error
// carries it.
func TestPortForwardErrorCarriesKubectlOutput(t *testing.T) {
	k := New(Config{Context: "x", Namespace: "ns"}).WithStarter(func(_ context.Context, _ []string) (Process, error) {
		return &fakeProc{out: strings.NewReader("Unable to listen on port 18080: bind: address already in use\n"), killed: make(chan struct{})}, nil
	})
	_, err := k.PortForward(context.Background(), "pod/p", 18080, 5433)
	if err == nil {
		t.Fatal("a forward that never became ready must error")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error = %q, want kubectl's own reason in it", err)
	}
}

// A forward whose kubectl dies must come back on the SAME local port without
// the caller noticing. kubectl port-forward does not reconnect on its own, and
// every "controller unreachable while the controller was fine" failure traces
// to that: one dead local process, every pinned client refused.
func TestPortForwardRestartsItselfOnTheSamePort(t *testing.T) {
	sr := &scriptRunner{}
	first := &fakeProc{out: strings.NewReader("Forwarding from 127.0.0.1:18080 -> 8080\n"), killed: make(chan struct{})}
	// The replacement keeps its stdout open so the handle stays up.
	pr, pw := io.Pipe()
	second := &fakeProc{out: pr, killed: make(chan struct{})}
	var argvs [][]string
	var mu sync.Mutex
	n := 0
	k := testKubectl(sr).WithStarter(func(_ context.Context, argv []string) (Process, error) {
		mu.Lock()
		defer mu.Unlock()
		argvs = append(argvs, argv)
		n++
		if n == 1 {
			return first, nil
		}
		go func() { _, _ = pw.Write([]byte("Forwarding from 127.0.0.1:18080 -> 8080\n")) }()
		return second, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/amp-controller", 18080, 8080)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer pf.Close()

	// first's stdout is already at EOF, so the supervisor sees it exit.
	deadline := time.Now().Add(5 * time.Second)
	for pf.Restarts() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pf.Restarts() == 0 {
		t.Fatal("a dead kubectl must be re-established, not left dead")
	}
	if !pf.Alive() {
		t.Error("the handle must stay alive across a restart")
	}
	if pf.LocalAddr() != "127.0.0.1:18080" {
		t.Errorf("local addr = %q, want the original port so pinned callers keep working", pf.LocalAddr())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(argvs) < 2 {
		t.Fatalf("kubectl was started %d time(s), want a relaunch", len(argvs))
	}
	if got := strings.Join(argvs[1], " "); !strings.Contains(got, "port-forward svc/amp-controller 18080:8080") {
		t.Errorf("restart argv = %q, want the same target and port", got)
	}
	if !strings.Contains(pf.Output(), "re-established") {
		t.Error("the restart must be recorded in the forward's output for triage")
	}
}

// A forward the caller Closed must stay closed: no supervisor resurrection.
func TestPortForwardDoesNotRestartAfterClose(t *testing.T) {
	sr := &scriptRunner{}
	pr, pw := io.Pipe()
	proc := &fakeProc{out: pr, killed: make(chan struct{})}
	starts := 0
	k := testKubectl(sr).WithStarter(func(_ context.Context, argv []string) (Process, error) {
		starts++
		go func() { _, _ = pw.Write([]byte("Forwarding from 127.0.0.1:18080 -> 8080\n")) }()
		return proc, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/amp-controller", 18080, 8080)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	pf.Close()
	pw.Close()
	time.Sleep(200 * time.Millisecond)
	if starts != 1 {
		t.Errorf("kubectl started %d times after Close, want 1", starts)
	}
	if pf.Alive() {
		t.Error("a closed forward must report dead")
	}
}

// The failure that cost the 0.260902.0 stress run: the pod behind a running
// forward goes away, kubectl keeps listening, and every later request is
// accepted and then EOFs. Only a relaunch re-resolves the target.
func TestPortForwardRelaunchesWhenItLosesItsPod(t *testing.T) {
	sr := &scriptRunner{}
	pr1, pw1 := io.Pipe()
	first := &fakeProc{out: pr1, killed: make(chan struct{})}
	pr2, pw2 := io.Pipe()
	second := &fakeProc{out: pr2, killed: make(chan struct{})}
	var mu sync.Mutex
	n := 0
	k := testKubectl(sr).WithStarter(func(_ context.Context, _ []string) (Process, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			go func() { _, _ = pw1.Write([]byte("Forwarding from 127.0.0.1:18081 -> 8080\n")) }()
			return first, nil
		}
		go func() { _, _ = pw2.Write([]byte("Forwarding from 127.0.0.1:18081 -> 8080\n")) }()
		return second, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/yugaware", 18081, 8080)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer pf.Close()

	_, _ = pw1.Write([]byte("E0903 09:44:01.000000   12345 portforward.go:413] lost connection to pod\n"))
	select {
	case <-first.killed:
	case <-time.After(5 * time.Second):
		t.Fatal("a forward that lost its pod must be killed so the supervisor relaunches it")
	}
	pw1.Close()

	deadline := time.Now().Add(5 * time.Second)
	for pf.Restarts() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pf.Restarts() == 0 {
		t.Fatal("the forward was not re-established after losing its pod")
	}
	if !pf.Alive() {
		t.Error("the handle must stay alive across the relaunch")
	}
	if !strings.Contains(pf.Output(), "lost its pod") {
		t.Errorf("output %q does not record why the forward was relaunched", pf.Output())
	}
}

// Per-connection forwarding noise is transient; relaunching on it would drop
// every other connection through the same forward.
func TestPortForwardKeepsGoingOnTransientForwardingErrors(t *testing.T) {
	sr := &scriptRunner{}
	pr, pw := io.Pipe()
	proc := &fakeProc{out: pr, killed: make(chan struct{})}
	starts := 0
	var mu sync.Mutex
	k := testKubectl(sr).WithStarter(func(_ context.Context, _ []string) (Process, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		go func() { _, _ = pw.Write([]byte("Forwarding from 127.0.0.1:18082 -> 8080\n")) }()
		return proc, nil
	})
	pf, err := k.PortForward(context.Background(), "svc/yugaware", 18082, 8080)
	if err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer pf.Close()

	_, _ = pw.Write([]byte("E0903 09:44:01.000000 1 portforward.go:233] an error occurred forwarding 18082 -> 8080: error creating error stream\n"))
	_, _ = pw.Write([]byte("E0903 09:44:02.000000 1 portforward.go:233] an error occurred forwarding 18082 -> 8080: error forwarding port 8080 to pod abc, uid : container not running (abc)\n"))
	time.Sleep(300 * time.Millisecond)
	select {
	case <-proc.killed:
		t.Fatal("a transient forwarding error must not tear the forward down")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Errorf("kubectl started %d times, want 1", starts)
	}
}

func TestForwardLostItsPod(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"E0903 portforward.go:413] lost connection to pod", true},
		{"error upgrading connection: pods \"yugaware-0\" not found", true},
		{"an error occurred forwarding 38757 -> 80: error creating error stream", false},
		// The kubelet's reason rides inside kubectl's per-connection line; one
		// dropped stream must not tear down a forward everything else shares.
		{"an error occurred forwarding 38757 -> 80: error forwarding port 80 to pod abc, uid : container not running (abc)", false},
		{"an error occurred forwarding 38757 -> 80: error forwarding port 80 to pod abc: pod not found", false},
		{"Forwarding from 127.0.0.1:38757 -> 80", false},
		{"Handling connection for 38757", false},
	} {
		if got := forwardLostItsPod(tc.line); got != tc.want {
			t.Errorf("forwardLostItsPod(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
