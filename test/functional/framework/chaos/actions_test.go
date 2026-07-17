package chaos_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/chaos"
	execpkg "github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// fakeExec is an exec.RemoteExecutor double: it records every command and
// returns a canned result. pgrep succeeds by default so process-recovery waits
// return immediately.
type fakeExec struct {
	mu   sync.Mutex
	cmds []string
}

func (f *fakeExec) Run(_ context.Context, cmd string) (execpkg.Result, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	if strings.Contains(cmd, "pgrep") {
		return execpkg.Result{ExitCode: 0, Stdout: "123"}, nil
	}
	return execpkg.Result{ExitCode: 0}, nil
}

func (f *fakeExec) Copy(context.Context, string, string) error { return nil }

func (f *fakeExec) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

// kubeRecorder records kubectl argv and answers pod pgrep exec with output.
type kubeRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *kubeRecorder) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	r.mu.Unlock()
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "pgrep") {
		return execpkg.Result{ExitCode: 0, Stdout: "123"}, nil
	}
	return execpkg.Result{ExitCode: 0}, nil
}

func (r *kubeRecorder) joined() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func podTarget(rec *kubeRecorder, ns string) *chaos.Target {
	kube := k8s.New(k8s.Config{Namespace: ns}).WithRunner(rec.run)
	return chaos.NewPodTarget("yb-tserver-0", kube, "yb-tserver-0", "yb-tserver")
}

func testCfg() chaos.Config {
	c := chaos.DefaultConfig()
	c.WaitTimeout = 2 * time.Second
	return c
}

func build(t *testing.T, name string) chaos.ChaosAction {
	t.Helper()
	a, err := chaos.DefaultRegistry().Build(name, testCfg())
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return a
}

func containsSubstr(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

func TestPodDeleteInjectRecover(t *testing.T) {
	rec := &kubeRecorder{}
	tgt := podTarget(rec, "ampqa-run1")
	a := build(t, chaos.ActionPodDelete)

	if err := a.Inject(context.Background(), tgt); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := a.Recover(context.Background(), tgt); err != nil {
		t.Fatalf("recover: %v", err)
	}
	calls := rec.joined()
	if !containsSubstr(calls, "delete pod yb-tserver-0 --ignore-not-found") {
		t.Errorf("expected pod delete, got %v", calls)
	}
	if !containsSubstr(calls, "wait pod/yb-tserver-0 --for=condition=Ready") {
		t.Errorf("expected wait-for-ready recover, got %v", calls)
	}
}

func TestPodDeleteRejectsHostTarget(t *testing.T) {
	a := build(t, chaos.ActionPodDelete)
	fe := &fakeExec{}
	tgt := chaos.NewHostTarget("vm-1", fe)
	if err := a.Inject(context.Background(), tgt); err == nil {
		t.Fatal("expected error injecting pod_delete on a host target")
	}
}

func TestProcessKillHost(t *testing.T) {
	fe := &fakeExec{}
	tgt := chaos.NewHostTarget("vm-1", fe)
	a := build(t, chaos.ActionProcessKill)

	if err := a.Inject(context.Background(), tgt); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := a.Recover(context.Background(), tgt); err != nil {
		t.Fatalf("recover: %v", err)
	}
	cmds := fe.recorded()
	if !containsSubstr(cmds, "sudo pkill -KILL -f 'yb-tserver'") {
		t.Errorf("expected SIGKILL, got %v", cmds)
	}
	if !containsSubstr(cmds, "sudo systemctl start 'yb-tserver'") {
		t.Errorf("expected systemctl start on recover, got %v", cmds)
	}
	if !containsSubstr(cmds, "pgrep -f 'yb-tserver'") {
		t.Errorf("expected pgrep wait on recover, got %v", cmds)
	}
}

func TestProcessRestartHostVsPod(t *testing.T) {
	// Host: systemctl restart.
	fe := &fakeExec{}
	host := chaos.NewHostTarget("vm-1", fe)
	a := build(t, chaos.ActionProcessRestart)
	if err := a.Inject(context.Background(), host); err != nil {
		t.Fatalf("host inject: %v", err)
	}
	if !containsSubstr(fe.recorded(), "sudo systemctl restart 'yb-tserver'") {
		t.Errorf("expected systemctl restart on host, got %v", fe.recorded())
	}

	// Pod: SIGTERM the process via kubectl exec.
	rec := &kubeRecorder{}
	pod := podTarget(rec, "ampqa-run1")
	if err := a.Inject(context.Background(), pod); err != nil {
		t.Fatalf("pod inject: %v", err)
	}
	if !containsSubstr(rec.joined(), "pkill -TERM -f 'yb-tserver'") {
		t.Errorf("expected SIGTERM in pod, got %v", rec.joined())
	}
}

func TestNetworkPartitionInjectRecover(t *testing.T) {
	fe := &fakeExec{}
	tgt := chaos.NewHostTarget("vm-1", fe)
	a := build(t, chaos.ActionNetworkPartition)

	if err := a.Inject(context.Background(), tgt); err != nil {
		t.Fatalf("inject: %v", err)
	}
	cmds := fe.recorded()
	if !containsSubstr(cmds, "iptables -A INPUT -p tcp --dport 7100 -j DROP -m comment --comment ampqa-chaos") {
		t.Errorf("expected -A rule for 7100, got %v", cmds)
	}
	fe2 := &fakeExec{}
	tgt2 := chaos.NewHostTarget("vm-1", fe2)
	if err := a.Recover(context.Background(), tgt2); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !containsSubstr(fe2.recorded(), "iptables -D INPUT -p tcp --dport 7100 -j DROP -m comment --comment ampqa-chaos") {
		t.Errorf("expected -D rule for 7100 on recover, got %v", fe2.recorded())
	}
}

func TestNetworkSlowdownAndPacketLoss(t *testing.T) {
	fe := &fakeExec{}
	tgt := chaos.NewHostTarget("vm-1", fe)

	slow := build(t, chaos.ActionNetworkSlowdown)
	if err := slow.Inject(context.Background(), tgt); err != nil {
		t.Fatalf("slowdown inject: %v", err)
	}
	if err := slow.Recover(context.Background(), tgt); err != nil {
		t.Fatalf("slowdown recover: %v", err)
	}
	cmds := fe.recorded()
	if !containsSubstr(cmds, "sudo tc qdisc add dev eth0 root netem delay 200ms") {
		t.Errorf("expected netem delay, got %v", cmds)
	}
	if !containsSubstr(cmds, "sudo tc qdisc del dev eth0 root") {
		t.Errorf("expected qdisc del on recover, got %v", cmds)
	}

	fe2 := &fakeExec{}
	tgt2 := chaos.NewHostTarget("vm-1", fe2)
	loss := build(t, chaos.ActionPacketLoss)
	if err := loss.Inject(context.Background(), tgt2); err != nil {
		t.Fatalf("loss inject: %v", err)
	}
	if !containsSubstr(fe2.recorded(), "sudo tc qdisc add dev eth0 root netem loss 30%") {
		t.Errorf("expected netem loss, got %v", fe2.recorded())
	}
}

// fakeCloud is a CloudVM double for node_stop.
type fakeCloud struct {
	mu                   sync.Mutex
	stops, starts, waits int
}

func (c *fakeCloud) Stop(context.Context) error  { c.mu.Lock(); c.stops++; c.mu.Unlock(); return nil }
func (c *fakeCloud) Start(context.Context) error { c.mu.Lock(); c.starts++; c.mu.Unlock(); return nil }
func (c *fakeCloud) WaitRunning(context.Context, time.Duration) error {
	c.mu.Lock()
	c.waits++
	c.mu.Unlock()
	return nil
}

func TestNodeStopWithCloud(t *testing.T) {
	fe := &fakeExec{}
	fc := &fakeCloud{}
	tgt := chaos.NewHostTarget("vm-1", fe).WithCloud(fc)
	a := build(t, chaos.ActionNodeStop)

	if err := a.Inject(context.Background(), tgt); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := a.Recover(context.Background(), tgt); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if fc.stops != 1 || fc.starts != 1 || fc.waits != 1 {
		t.Errorf("cloud calls = stops:%d starts:%d waits:%d, want 1/1/1", fc.stops, fc.starts, fc.waits)
	}
}

func TestNodeStopRequiresCloud(t *testing.T) {
	fe := &fakeExec{}
	tgt := chaos.NewHostTarget("vm-1", fe) // no WithCloud
	a := build(t, chaos.ActionNodeStop)
	if err := a.Inject(context.Background(), tgt); err == nil {
		t.Fatal("expected error injecting node_stop without a CloudVM")
	}
	// Recover on a never-injected target must be a safe no-op.
	if err := a.Recover(context.Background(), tgt); err != nil {
		t.Fatalf("recover no-op: %v", err)
	}
}
