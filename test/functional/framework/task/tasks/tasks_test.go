package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/env/reap"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/task"
)

// scriptExec is a scripted fake sshTransport: it records every command and
// returns canned results matched by substring, mirroring apps/dockerhost's
// test double. Close is a no-op recorded for assertions.
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	closed   bool
	rules    []rule
	fallback exec.Result
}

type rule struct {
	match string
	res   exec.Result
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match: match, res: res})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.res, nil
		}
	}
	return s.fallback, nil
}

func (s *scriptExec) Copy(context.Context, string, string) error { return nil }

func (s *scriptExec) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestTaskRegistryHasBuiltins(t *testing.T) {
	for _, name := range []string{"reap", "collect-logs", "deploy-artifact", "deploy-amp", "status-amp", "teardown-amp"} {
		tk, ok := task.Default().Get(name)
		if !ok {
			t.Errorf("built-in task %q not registered", name)
			continue
		}
		if tk.Doc == "" {
			t.Errorf("task %q has no Doc", name)
		}
		if tk.Run == nil {
			t.Errorf("task %q has no Run", name)
		}
	}
}

func TestTaskListSortedForCLI(t *testing.T) {
	list := task.Default().List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("List not sorted: %q before %q", list[i-1].Name, list[i].Name)
		}
	}
}

func TestReapRemovesExpiredDockerEntryAndTearsDownContainers(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "old-soak", Shape: "docker", Provider: "docker",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"host": "10.0.0.1", "containers": "ep-1, yb-1"},
	})
	if err != nil {
		t.Fatalf("Add expired: %v", err)
	}
	fresh, err := reg.Add(registry.Entry{
		Name: "fresh-soak", Shape: "docker", Provider: "docker",
		CreatedAt: time.Now(), TTL: time.Hour,
		Data: map[string]string{"host": "10.0.0.2", "containers": "ep-2"},
	})
	if err != nil {
		t.Fatalf("Add fresh: %v", err)
	}

	fake := &scriptExec{fallback: exec.Result{ExitCode: 0}}
	var dialedHost string
	res, err := Reap(context.Background(), reg, ReapOptions{
		Now: func() time.Time { return time.Now() },
		Dial: func(ctx context.Context, host, user, keyPath string) (sshTransport, error) {
			dialedHost = host
			return fake, nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if len(res.Expired) != 1 || res.Expired[0] != expired.ID {
		t.Errorf("Expired = %v, want [%s]", res.Expired, expired.ID)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s]", res.Removed, expired.ID)
	}
	if dialedHost != "10.0.0.1" {
		t.Errorf("dialed host = %q, want 10.0.0.1 (the expired entry's host)", dialedHost)
	}
	if !fake.closed {
		t.Error("teardown ssh transport should be closed")
	}

	var stoppedEp1, stoppedYb1 bool
	for _, c := range fake.commands {
		if strings.Contains(c, "docker rm -f") && strings.Contains(c, "ep-1") {
			stoppedEp1 = true
		}
		if strings.Contains(c, "docker rm -f") && strings.Contains(c, "yb-1") {
			stoppedYb1 = true
		}
	}
	if !stoppedEp1 || !stoppedYb1 {
		t.Errorf("expected StopRm for both containers, got commands: %v", fake.commands)
	}

	// The fresh entry must survive.
	_, ok, err := reg.Get(fresh.ID)
	if err != nil {
		t.Fatalf("Get fresh: %v", err)
	}
	if !ok {
		t.Error("fresh (non-expired) entry should not be reaped")
	}
}

func TestReapDryRunRemovesNothing(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "old", CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	dialed := false
	res, err := Reap(context.Background(), reg, ReapOptions{
		DryRun: true,
		Dial: func(ctx context.Context, host, user, keyPath string) (sshTransport, error) {
			dialed = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.Expired) != 1 || res.Expired[0] != expired.ID {
		t.Errorf("Expired = %v, want [%s]", res.Expired, expired.ID)
	}
	if len(res.Removed) != 0 {
		t.Errorf("dry run must not remove anything, got %v", res.Removed)
	}
	if dialed {
		t.Error("dry run must not dial out for teardown")
	}
	_, ok, err := reg.Get(expired.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("dry run must leave the expired entry in place")
	}
}

func TestReapUnknownProviderRemovedWithoutTeardown(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "manual-vm", Provider: "vm", CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	dialed := false
	res, err := Reap(context.Background(), reg, ReapOptions{
		Dial: func(ctx context.Context, host, user, keyPath string) (sshTransport, error) {
			dialed = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s]", res.Removed, expired.ID)
	}
	if dialed {
		t.Error("unknown provider must not be dialed for teardown")
	}
}

// TestReapDockerTeardownFailureKeepsRecord: a docker entry whose teardown fails
// (host unreachable) may still have live containers, so the record is KEPT — a
// missed reap is never silently dropped from the registry.
func TestReapDockerTeardownFailureKeepsRecord(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "flaky", Provider: "docker", CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"host": "10.0.0.9", "containers": "c1"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := Reap(context.Background(), reg, ReapOptions{
		Dial: func(ctx context.Context, host, user, keyPath string) (sshTransport, error) {
			return nil, errAlwaysFails{}
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.TeardownErrs) != 1 || res.TeardownErrs[expired.ID] == nil {
		t.Errorf("expected a teardown error for %s, got %v", expired.ID, res.TeardownErrs)
	}
	if len(res.Removed) != 0 {
		t.Errorf("failed teardown must KEEP the record, got Removed=%v", res.Removed)
	}
	if _, ok, _ := reg.Get(expired.ID); !ok {
		t.Error("entry with a failed reap must remain in the registry")
	}
}

// TestReapExecutesRecordedPlan: a k8s/cloud entry that carries a reap.Plan has
// its plan EXECUTED (not silently removed), and on success the record is dropped.
func TestReapExecutesRecordedPlan(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "k8s-soak", Provider: "functional", Shape: "yb_k8s",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"reap_plan": `{"namespaces":[{"kind":"ybk8s","namespace":"ampqa-yb-abc"}]}`},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var executed []string
	res, err := Reap(context.Background(), reg, ReapOptions{
		Execute: func(_ context.Context, p *reap.Plan) error {
			for _, n := range p.Namespaces {
				executed = append(executed, n.Namespace)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(executed) != 1 || executed[0] != "ampqa-yb-abc" {
		t.Fatalf("plan executor ran for %v, want [ampqa-yb-abc]", executed)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s] after a successful plan reap", res.Removed, expired.ID)
	}
}

// TestReapFailedPlanKeepsRecord: when the recorded plan's reap fails, the record
// is KEPT with a loud error — the leaked namespace/instances stay discoverable.
func TestReapFailedPlanKeepsRecord(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "k8s-soak", Provider: "functional", Shape: "yb_k8s",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"reap_plan": `{"namespaces":[{"kind":"ybk8s","namespace":"ampqa-yb-abc"}]}`},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := Reap(context.Background(), reg, ReapOptions{
		Execute: func(context.Context, *reap.Plan) error { return errAlwaysFails{} },
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.TeardownErrs[expired.ID] == nil {
		t.Errorf("expected a teardown error for the failed plan reap, got %v", res.TeardownErrs)
	}
	if len(res.Removed) != 0 {
		t.Errorf("a failed plan reap must KEEP the record, got Removed=%v", res.Removed)
	}
	if _, ok, _ := reg.Get(expired.ID); !ok {
		t.Error("entry with a failed plan reap must remain in the registry")
	}
}

// TestReapCloudTagEntry: a bare cloud entry (run_tag + cloud, no plan) is reaped
// via the cloud tag-reap seam, then removed.
func TestReapCloudTagEntry(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "vm-soak", Provider: "vm", Shape: "vm",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"cloud": "aws", "run_tag": "cafef00d"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var reaped []string
	res, err := Reap(context.Background(), reg, ReapOptions{
		CloudReap: func(_ context.Context, cloudName, runTag string) error {
			reaped = append(reaped, cloudName+"/"+runTag)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "aws/cafef00d" {
		t.Fatalf("cloud reaper ran for %v, want [aws/cafef00d]", reaped)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s] after a successful tag reap", res.Removed, expired.ID)
	}
}

type errAlwaysFails struct{}

func (errAlwaysFails) Error() string { return "dial always fails" }

func TestCollectLogsWritesFilesPerContainer(t *testing.T) {
	fake := &scriptExec{}
	fake.on("docker logs", exec.Result{Stdout: "line1\n", Stderr: "line2\n"})
	outDir := t.TempDir()

	err := CollectLogs(context.Background(),
		func(ctx context.Context, host, user, keyPath string) (sshTransport, error) { return fake, nil },
		"host1", "", "", []string{"a", "b"}, outDir)
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		data, err := os.ReadFile(filepath.Join(outDir, name+".log"))
		if err != nil {
			t.Fatalf("read %s.log: %v", name, err)
		}
		if !strings.Contains(string(data), "line1") || !strings.Contains(string(data), "line2") {
			t.Errorf("%s.log missing expected content: %q", name, data)
		}
	}
	if !fake.closed {
		t.Error("ssh transport should be closed after CollectLogs")
	}
}
