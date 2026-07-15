package dockerhost_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/dockerhost"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// scriptExec is a scripted fake RemoteExecutor: it records every command and
// returns canned results matched by substring (in registration order).
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	fallback exec.Result
}

type rule struct {
	match string
	res   exec.Result
	err   error
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

func TestRunContainerCommandConstruction(t *testing.T) {
	ex := &scriptExec{}
	ex.fallback = exec.Result{ExitCode: 0, Stdout: "cid123\n"}
	h := dockerhost.New(ex)

	id, err := h.RunContainer(context.Background(), dockerhost.ContainerSpec{
		Name:   "ep-1",
		Image:  "amp/endpoint:tag",
		Detach: true,
		Env:    map[string]string{"COMPUTE_PORT": "55440", "DEV_PROVISION_SCHEMA": "true"},
		Ports:  []dockerhost.PortMap{{Host: 55440, Container: 55433}},
	})
	if err != nil {
		t.Fatalf("run container: %v", err)
	}
	if id != "ep-1" {
		t.Errorf("id = %q, want ep-1", id)
	}
	got := ex.recorded()[0]
	// Deterministic env ordering (sorted keys).
	wantParts := []string{
		"docker run -d",
		"--name 'ep-1'",
		"-p 55440:55433",
		"-e 'COMPUTE_PORT=55440'",
		"-e 'DEV_PROVISION_SCHEMA=true'",
		"'amp/endpoint:tag'",
	}
	for _, p := range wantParts {
		if !strings.Contains(got, p) {
			t.Errorf("command missing %q:\n%s", p, got)
		}
	}
	if idxPort, idxImage := strings.Index(got, "COMPUTE_PORT"), strings.Index(got, "DEV_PROVISION"); idxPort > idxImage {
		t.Errorf("env keys not sorted deterministically: %s", got)
	}
}

func TestRunContainerHostNetworkOmitsPorts(t *testing.T) {
	ex := &scriptExec{fallback: exec.Result{Stdout: "cid"}}
	h := dockerhost.New(ex)
	_, err := h.RunContainer(context.Background(), dockerhost.ContainerSpec{
		Name:        "yb",
		Image:       "yugabytedb/yugabyte:latest",
		HostNetwork: true,
		Ports:       []dockerhost.PortMap{{Host: 5433, Container: 5433}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := ex.recorded()[0]
	if !strings.Contains(got, "--network host") {
		t.Errorf("want --network host: %s", got)
	}
	if strings.Contains(got, "-p ") {
		t.Errorf("host network must not publish ports: %s", got)
	}
}

func TestInspectRunning(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true\n"})
	h := dockerhost.New(ex)
	running, err := h.Inspect(context.Background(), "ep-1")
	if err != nil || !running {
		t.Fatalf("running=%v err=%v, want true nil", running, err)
	}
}

func TestInspectMissingIsNotRunning(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker inspect", exec.Result{ExitCode: 1, Stderr: "Error: No such object"})
	h := dockerhost.New(ex)
	running, err := h.Inspect(context.Background(), "gone")
	if err != nil {
		t.Fatalf("err = %v, want nil for missing container", err)
	}
	if running {
		t.Errorf("missing container must not be running")
	}
}

func TestStopRmIgnoresMissing(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker rm", exec.Result{ExitCode: 1, Stderr: "Error: No such container: gone"})
	h := dockerhost.New(ex)
	if err := h.StopRm(context.Background(), "gone"); err != nil {
		t.Errorf("StopRm on missing must be nil, got %v", err)
	}
}

func TestLogsTail(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker logs", exec.Result{Stdout: "out\n", Stderr: "err\n"})
	h := dockerhost.New(ex)
	logs, err := h.Logs(context.Background(), "ep-1", 100)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs, "out") || !strings.Contains(logs, "err") {
		t.Errorf("logs should merge streams: %q", logs)
	}
	if !strings.Contains(ex.recorded()[0], "--tail 100") {
		t.Errorf("tail flag missing: %s", ex.recorded()[0])
	}
}

func TestWaitHealthyProbeSucceeds(t *testing.T) {
	ex := &scriptExec{}
	ex.on("docker inspect", exec.Result{ExitCode: 0, Stdout: "true"})
	h := dockerhost.New(ex)
	calls := 0
	err := h.WaitHealthy(context.Background(), "ep-1", func(context.Context) error {
		calls++
		if calls < 2 {
			return context.DeadlineExceeded // stand-in for "not ready yet"
		}
		return nil
	})
	if err != nil {
		t.Fatalf("wait healthy: %v", err)
	}
	if calls != 2 {
		t.Errorf("probe called %d times, want 2", calls)
	}
}
