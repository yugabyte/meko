package yugabyted

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/artifact"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// scriptExec is a scripted fake RemoteExecutor: it records every command and
// returns canned results matched by substring (in registration order).
type scriptExec struct {
	name     string
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

// recordingDeployer records the Deploy call and its target count.
type recordingDeployer struct {
	calls    int
	execs    int
	specName string
}

func (d *recordingDeployer) Deploy(_ context.Context, execs []exec.RemoteExecutor, spec artifact.Spec, _ artifact.DeployOptions) error {
	d.calls++
	d.execs = len(execs)
	d.specName = spec.Name
	return nil
}

func threeNodes() ([]Node, []*scriptExec) {
	execs := []*scriptExec{{name: "n0"}, {name: "n1"}, {name: "n2"}}
	nodes := []Node{
		{Exec: execs[0], Advertise: "10.0.0.1"},
		{Exec: execs[1], Advertise: "10.0.0.2"},
		{Exec: execs[2], Advertise: "10.0.0.3"},
	}
	return nodes, execs
}

func TestStartJoinSequence(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Node 0 bootstraps: the post_install marker probe, the python probe,
	// then start with no --join (the default fake exec reports success for
	// both probes, so they are the only extra commands).
	c0 := execs[0].recorded()
	if len(c0) != 3 {
		t.Fatalf("node0 commands = %v", c0)
	}
	startCmd := c0[len(c0)-1]
	for _, want := range []string{
		"'/opt/yb/bin/yugabyted' start",
		"--advertise_address='10.0.0.1'",
		"--base_dir='" + DefaultBaseDir + "'",
		"--background=true",
		"--fault_tolerance=zone",
	} {
		if !strings.Contains(startCmd, want) {
			t.Errorf("node0 start missing %q in %q", want, startCmd)
		}
	}
	if strings.Contains(startCmd, "--join") {
		t.Errorf("node0 must not join: %q", startCmd)
	}

	// Nodes 1 and 2 join node 0.
	for i, ex := range execs[1:] {
		c := ex.recorded()
		if len(c) != 3 || !strings.Contains(c[len(c)-1], "--join='10.0.0.1'") {
			t.Errorf("node%d join command = %v", i+1, c)
		}
	}
}

func TestStartRunsPostInstallBeforeFirstBinaryCommand(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.on("test -f '/opt/yb/"+postInstallMarker+"'", exec.Result{ExitCode: 1}).
			on("test -f '/opt/yb/"+postInstallScript+"'", exec.Result{ExitCode: 0})
	}
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		cmds := ex.recorded()
		runIdx := indexOfContains(cmds, "cd '/opt/yb' && ./bin/post_install.sh")
		touchIdx := indexOfContains(cmds, "touch '/opt/yb/"+postInstallMarker+"'")
		startIdx := indexOfContains(cmds, "'/opt/yb/bin/yugabyted' start")
		if runIdx < 0 {
			t.Fatalf("node%d: post_install.sh never run: %v", i, cmds)
		}
		if touchIdx < 0 || touchIdx < runIdx {
			t.Fatalf("node%d: marker not touched after running post_install.sh: %v", i, cmds)
		}
		if startIdx < 0 || startIdx < touchIdx {
			t.Fatalf("node%d: yugabyted start ran before post_install.sh completed: %v", i, cmds)
		}
	}
}

func TestStartSkipsPostInstallWhenMarkerPresent(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.on("test -f '/opt/yb/"+postInstallMarker+"'", exec.Result{ExitCode: 0})
	}
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		if cmds := ex.recorded(); indexOfContains(cmds, "post_install.sh") >= 0 {
			t.Errorf("node%d: post_install.sh ran despite marker present: %v", i, cmds)
		}
	}
}

func TestStartWithoutInstallDirUsesPathBinary(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// No InstallDir → no post_install probe; the python probe (default fake
	// reports success) runs first, then the start command.
	cmds := execs[0].recorded()
	startIdx := indexOfContains(cmds, "yugabyted start")
	if startIdx < 0 {
		t.Fatalf("yugabyted start never ran: %v", cmds)
	}
	c0 := cmds[startIdx]
	if !strings.HasPrefix(c0, "yugabyted start") {
		t.Errorf("expected bare `yugabyted` on PATH, got %q", c0)
	}
	if strings.Contains(c0, "--fault_tolerance") {
		t.Errorf("RF<=1 must not set fault_tolerance: %q", c0)
	}
}

func TestStartFailurePropagates(t *testing.T) {
	nodes, execs := threeNodes()
	// The python probe must report healthy so the bootstrap failure below
	// (not an unrelated ensure-python failure) is what's under test.
	execs[0].on(pythonProbeCmd, exec.Result{ExitCode: 0})
	execs[0].fallback = exec.Result{ExitCode: 1, Stderr: "bind: address in use"}
	app := New(nodes, Spec{})
	err := app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bootstrap node 10.0.0.1") {
		t.Fatalf("expected bootstrap failure, got %v", err)
	}
}

// TestEnsurePythonRunsBeforeFirstYugabytedCommand asserts the dataclasses
// probe runs on every node, before any `yugabyted` binary invocation, and
// that a healthy probe (the default fake reports exit 0) never triggers the
// install command.
func TestEnsurePythonRunsBeforeFirstYugabytedCommand(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		cmds := ex.recorded()
		probeIdx := indexOfContains(cmds, pythonProbeCmd)
		startIdx := indexOfContains(cmds, "'/opt/yb/bin/yugabyted' start")
		if probeIdx < 0 {
			t.Fatalf("node%d: dataclasses probe never run: %v", i, cmds)
		}
		if startIdx < 0 || startIdx < probeIdx {
			t.Fatalf("node%d: yugabyted start ran before/without the python probe: %v", i, cmds)
		}
		if indexOfContains(cmds, "dnf install") >= 0 {
			t.Errorf("node%d: install command issued despite a healthy probe: %v", i, cmds)
		}
	}
}

// TestEnsurePythonDisabledSkipsProbe asserts WithEnsurePython(false) is a
// full opt-out: no probe, no install, on any node.
func TestEnsurePythonDisabledSkipsProbe(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{}, WithEnsurePython(false))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		if cmds := ex.recorded(); indexOfContains(cmds, "dataclasses") >= 0 {
			t.Errorf("node%d: python probe ran despite WithEnsurePython(false): %v", i, cmds)
		}
	}
}

// stagedProbeExec is a RemoteExecutor whose exact-match dataclasses probe
// fails once then succeeds, simulating a successful python_setup_cmd; every
// other command (including python_setup_cmd itself) succeeds.
type stagedProbeExec struct {
	mu       sync.Mutex
	commands []string
	probes   int
}

func (s *stagedProbeExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	if cmd == pythonProbeCmd {
		s.probes++
		if s.probes == 1 {
			return exec.Result{ExitCode: 1, Stderr: "ModuleNotFoundError: No module named 'dataclasses'"}, nil
		}
	}
	return exec.Result{ExitCode: 0}, nil
}

func (s *stagedProbeExec) Copy(context.Context, string, string) error { return nil }

func (s *stagedProbeExec) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// TestEnsurePythonInstallsAndReverifiesWhenProbeFails asserts the full
// probe → install → re-verify sequence runs, in order, before yugabyted
// starts, when the initial probe fails.
func TestEnsurePythonInstallsAndReverifiesWhenProbeFails(t *testing.T) {
	ex := &stagedProbeExec{}
	nodes := []Node{{Exec: ex, Advertise: "10.0.0.1"}}
	app := New(nodes, Spec{})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	cmds := ex.recorded()
	if len(cmds) < 4 {
		t.Fatalf("expected probe, install, re-verify, start; got %v", cmds)
	}
	if cmds[0] != pythonProbeCmd {
		t.Fatalf("command 0 = %q, want the dataclasses probe", cmds[0])
	}
	if !strings.Contains(cmds[1], "dnf install -y python39") || !strings.Contains(cmds[1], "alternatives") {
		t.Fatalf("command 1 = %q, want the install/alternatives command", cmds[1])
	}
	if cmds[2] != pythonProbeCmd {
		t.Fatalf("command 2 = %q, want the re-verify probe", cmds[2])
	}
	if !strings.HasPrefix(cmds[3], "yugabyted start") {
		t.Fatalf("command 3 = %q, want yugabyted start", cmds[3])
	}
}

// TestEnsurePythonUsesConfiguredSetupCmd asserts WithPythonSetupCmd overrides
// the command issued on a failed probe.
func TestEnsurePythonUsesConfiguredSetupCmd(t *testing.T) {
	ex := &stagedProbeExec{}
	nodes := []Node{{Exec: ex, Advertise: "10.0.0.1"}}
	const custom = "echo custom-setup"
	app := New(nodes, Spec{}, WithPythonSetupCmd(custom))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if cmds := ex.recorded(); len(cmds) < 2 || cmds[1] != custom {
		t.Fatalf("expected configured setup cmd as command 1, got %v", cmds)
	}
}

// TestEnsurePythonUnrecoverableFailureAbortsStart asserts a still-broken
// python after setup produces a diagnosable error and never reaches
// yugabyted start.
func TestEnsurePythonUnrecoverableFailureAbortsStart(t *testing.T) {
	nodes, execs := threeNodes()
	execs[0].fallback = exec.Result{ExitCode: 1, Stderr: "no python39 available"}
	app := New(nodes, Spec{})
	err := app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ensure python") {
		t.Fatalf("expected ensure-python error, got %v", err)
	}
	if idx := indexOfContains(execs[0].recorded(), "yugabyted start"); idx >= 0 {
		t.Errorf("yugabyted start ran despite unrecoverable python failure: %v", execs[0].recorded())
	}
}

func TestStopAndDestroyCommands(t *testing.T) {
	nodes, execs := threeNodes()
	sink := &memSink{}
	app := New(nodes, Spec{InstallDir: "/opt/yb", BaseDir: "/data/yb"})

	if err := app.Destroy(context.Background(), sink); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// Each node must see: collect (cat logs) → stop → wipe.
	for _, ex := range execs {
		cmds := ex.recorded()
		var sawStop, sawWipe bool
		for _, c := range cmds {
			if strings.Contains(c, "yugabyted' stop --base_dir='/data/yb'") {
				sawStop = true
			}
			if strings.Contains(c, "rm -rf '/data/yb'") {
				sawWipe = true
			}
		}
		if !sawStop || !sawWipe {
			t.Errorf("node %s: stop=%v wipe=%v cmds=%v", ex.name, sawStop, sawWipe, cmds)
		}
	}
}

// TestDefaultBaseDirWritableAndConsistentAcrossLifecycle guards the bootstrap
// PermissionError regression: yugabyted runs as an unprivileged ssh user
// (ec2-user), so a Spec with no BaseDir override must never resolve under
// /root, and every command that needs to locate the cluster (start, join,
// status, stop) must agree on the same path.
func TestDefaultBaseDirWritableAndConsistentAcrossLifecycle(t *testing.T) {
	if strings.HasPrefix(DefaultBaseDir, "/root") {
		t.Fatalf("DefaultBaseDir = %q, must not be under /root (yugabyted does not run as root)", DefaultBaseDir)
	}
	nodes, execs := threeNodes()
	app := New(nodes, Spec{})

	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	bootstrapStart := execs[0].recorded()[len(execs[0].recorded())-1]
	if !strings.Contains(bootstrapStart, "--base_dir='"+DefaultBaseDir+"'") {
		t.Errorf("bootstrap start = %q, want --base_dir=%q", bootstrapStart, DefaultBaseDir)
	}
	joinStart := execs[1].recorded()[len(execs[1].recorded())-1]
	if !strings.Contains(joinStart, "--base_dir='"+DefaultBaseDir+"'") {
		t.Errorf("join start = %q, want --base_dir=%q", joinStart, DefaultBaseDir)
	}

	if _, err := app.Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	statusCmd := execs[0].recorded()[len(execs[0].recorded())-1]
	if !strings.Contains(statusCmd, "status --base_dir='"+DefaultBaseDir+"'") {
		t.Errorf("status cmd = %q, want --base_dir=%q", statusCmd, DefaultBaseDir)
	}

	if err := app.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for i, ex := range execs {
		stopCmd := ex.recorded()[len(ex.recorded())-1]
		if !strings.Contains(stopCmd, "stop --base_dir='"+DefaultBaseDir+"'") {
			t.Errorf("node%d stop cmd = %q, want --base_dir=%q", i, stopCmd, DefaultBaseDir)
		}
	}
}

func TestConnInfo(t *testing.T) {
	nodes, _ := threeNodes()
	app := New(nodes, Spec{YSQLPort: 5433})
	ci := app.ConnInfo()
	if ci.Host != "10.0.0.1" || ci.Port != 5433 || ci.User != ybUser || ci.Database != ybDatabase {
		t.Errorf("conninfo = %+v", ci)
	}

	app2 := New(nodes, Spec{YSQLHost: "yb.internal"})
	if h := app2.ConnInfo().Host; h != "yb.internal" {
		t.Errorf("ysql host override = %q, want yb.internal", h)
	}
}

func TestDeployPathInvokedWhenConfigured(t *testing.T) {
	nodes, _ := threeNodes()
	dep := &recordingDeployer{}
	app := New(nodes, Spec{
		InstallDir: "/opt/yb",
		Deploy: &DeploySpec{
			Artifact:  artifact.Spec{Name: "yb-release"},
			RemoteDir: "/opt",
		},
	}, WithDeployer(dep))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if dep.calls != 1 {
		t.Errorf("deploy calls = %d, want 1", dep.calls)
	}
	if dep.execs != 3 {
		t.Errorf("deploy target execs = %d, want 3", dep.execs)
	}
	if dep.specName != "yb-release" {
		t.Errorf("deploy spec = %q, want yb-release", dep.specName)
	}
}

func TestNoDeployWhenUnset(t *testing.T) {
	nodes, _ := threeNodes()
	dep := &recordingDeployer{}
	app := New(nodes, Spec{}, WithDeployer(dep))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if dep.calls != 0 {
		t.Errorf("deploy calls = %d, want 0 (no Spec.Deploy)", dep.calls)
	}
}

func TestWaitProbesYSQLAndStatus(t *testing.T) {
	nodes, execs := threeNodes()
	execs[0].on("status", exec.Result{ExitCode: 0, Stdout: "Status: Running"})
	var probed pg.ConnInfo
	app := New(nodes, Spec{}, WithWaitPG(func(_ context.Context, ci pg.ConnInfo, _ time.Duration) error {
		probed = ci
		return nil
	}))
	if err := app.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if probed.Host != "10.0.0.1" || probed.Port != DefaultYSQLPort {
		t.Errorf("probed = %+v", probed)
	}
}

func TestCollectGathersLogs(t *testing.T) {
	nodes, execs := threeNodes()
	execs[0].on("yugabyted.log", exec.Result{ExitCode: 0, Stdout: "yb line\n"})
	sink := &memSink{}
	app := New(nodes, Spec{})
	if err := app.Collect(context.Background(), sink); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, ok := sink.blobs["10.0.0.1/yugabyted.log"]; !ok {
		t.Errorf("expected yugabyted.log blob, got %v", sink.names())
	}
}

// memSink is an in-memory ArtifactSink.
type memSink struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (m *memSink) Add(name, _ string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[name] = content
	return nil
}

func (m *memSink) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.blobs))
	for k := range m.blobs {
		out = append(out, k)
	}
	return out
}

func indexOfContains(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}
