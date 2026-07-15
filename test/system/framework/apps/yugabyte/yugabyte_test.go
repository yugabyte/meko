package yugabyte

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

// scriptExec is a scripted fake RemoteExecutor: records every command, answers
// canned results matched by substring (registration order).
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	fallback exec.Result
}

type rule struct {
	match string
	res   exec.Result
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match, res})
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

func (s *scriptExec) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// leaderExec answers list_all_masters with a LEADER so waitMastersHealthy
// passes in one poll.
func leaderExec() *scriptExec {
	s := &scriptExec{}
	s.on("list_all_masters", exec.Result{ExitCode: 0, Stdout: "uuid host:7100 ALIVE LEADER\n"})
	return s
}

func threeNodes() ([]Node, []*scriptExec) {
	execs := []*scriptExec{leaderExec(), leaderExec(), leaderExec()}
	nodes := []Node{
		{Exec: execs[0], Advertise: "10.0.0.1"},
		{Exec: execs[1], Advertise: "10.0.0.2"},
		{Exec: execs[2], Advertise: "10.0.0.3"},
	}
	return nodes, execs
}

func okWaitPG(context.Context, pg.ConnInfo, time.Duration) error { return nil }

func TestStartSequenceMasterThenTServer(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	wantMasterAddrs := "10.0.0.1:7100,10.0.0.2:7100,10.0.0.3:7100"

	for i, ex := range execs {
		cmds := ex.recorded()
		joined := strings.Join(cmds, "\n")
		// Every node prepared its data dir, started a master (first 3 nodes are
		// masters — here all 3), and started a tserver.
		if !strings.Contains(joined, "mkdir -p '"+DefaultDataDir+"'") {
			t.Errorf("node%d never prepared data dir: %v", i, cmds)
		}
		masterIdx := indexOfContains(cmds, "'/opt/yb/bin/yb-master'")
		tserverIdx := indexOfContains(cmds, "'/opt/yb/bin/yb-tserver'")
		if masterIdx < 0 {
			t.Fatalf("node%d never started yb-master: %v", i, cmds)
		}
		if tserverIdx < 0 {
			t.Fatalf("node%d never started yb-tserver: %v", i, cmds)
		}
		if masterIdx > tserverIdx {
			t.Errorf("node%d started tserver before master", i)
		}

		masterCmd := cmds[masterIdx]
		for _, want := range []string{
			"--master_addresses=" + wantMasterAddrs,
			"--rpc_bind_addresses=10.0.0." + itoa(i+1) + ":7100",
			"--server_broadcast_addresses=10.0.0." + itoa(i+1) + ":7100",
			"--fs_data_dirs='" + DefaultDataDir + "'",
			"--webserver_port=7000",
			"--replication_factor=3",
		} {
			if !strings.Contains(masterCmd, want) {
				t.Errorf("node%d master cmd missing %q in %q", i, want, masterCmd)
			}
		}

		tserverCmd := cmds[tserverIdx]
		for _, want := range []string{
			"--tserver_master_addrs=" + wantMasterAddrs,
			"--rpc_bind_addresses=10.0.0." + itoa(i+1) + ":9100",
			"--server_broadcast_addresses=10.0.0." + itoa(i+1) + ":9100",
			"--enable_ysql=true",
			"--pgsql_proxy_bind_address=10.0.0." + itoa(i+1) + ":5433",
			"--webserver_port=9000",
		} {
			if !strings.Contains(tserverCmd, want) {
				t.Errorf("node%d tserver cmd missing %q in %q", i, want, tserverCmd)
			}
		}
	}
}

func TestStartRunsPostInstallBeforeFirstBinaryCommand(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.on("test -f '/opt/yb/"+postInstallMarker+"'", exec.Result{ExitCode: 1}).
			on("test -f '/opt/yb/"+postInstallScript+"'", exec.Result{ExitCode: 0})
	}
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		cmds := ex.recorded()
		runIdx := indexOfContains(cmds, "cd '/opt/yb' && ./bin/post_install.sh")
		touchIdx := indexOfContains(cmds, "touch '/opt/yb/"+postInstallMarker+"'")
		masterIdx := indexOfContains(cmds, "'/opt/yb/bin/yb-master'")
		if runIdx < 0 {
			t.Fatalf("node%d: post_install.sh never run: %v", i, cmds)
		}
		if touchIdx < 0 || touchIdx < runIdx {
			t.Fatalf("node%d: marker not touched after running post_install.sh: %v", i, cmds)
		}
		if masterIdx < 0 || masterIdx < touchIdx {
			t.Fatalf("node%d: yb-master started before post_install.sh completed: %v", i, cmds)
		}
	}
}

func TestStartSkipsPostInstallWhenMarkerPresent(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.on("test -f '/opt/yb/"+postInstallMarker+"'", exec.Result{ExitCode: 0})
	}
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 3}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i, ex := range execs {
		if cmds := ex.recorded(); indexOfContains(cmds, "post_install.sh") >= 0 {
			t.Errorf("node%d: post_install.sh ran despite marker present: %v", i, cmds)
		}
	}
}

// RF=1 across 3 nodes: only the first node runs a master; all three run tservers.
func TestRFOneMasterThreeTServers(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{InstallDir: "/opt/yb", RF: 1}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	wantMasterAddrs := "10.0.0.1:7100"

	if indexOfContains(execs[0].recorded(), "yb-master") < 0 {
		t.Error("node0 should run a master")
	}
	for i := 1; i < 3; i++ {
		if indexOfContains(execs[i].recorded(), "'/opt/yb/bin/yb-master'") >= 0 {
			t.Errorf("node%d should NOT run a master with RF=1", i)
		}
		if indexOfContains(execs[i].recorded(), "yb-tserver") < 0 {
			t.Errorf("node%d should run a tserver", i)
		}
	}
	// tserver on node1 points at the single master.
	tsCmd := execs[1].recorded()[indexOfContains(execs[1].recorded(), "yb-tserver")]
	if !strings.Contains(tsCmd, "--tserver_master_addrs="+wantMasterAddrs) {
		t.Errorf("node1 tserver master addrs = %q, want %s", tsCmd, wantMasterAddrs)
	}
}

func TestStartWithoutInstallDirUsesPathBinaries(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	joined := strings.Join(execs[0].recorded(), "\n")
	if !strings.Contains(joined, "nohup yb-master ") {
		t.Errorf("expected bare yb-master on PATH: %v", execs[0].recorded())
	}
	if !strings.Contains(joined, "nohup yb-tserver ") {
		t.Errorf("expected bare yb-tserver on PATH: %v", execs[0].recorded())
	}
}

func TestConnInfo(t *testing.T) {
	nodes, _ := threeNodes()
	app := New(nodes, Spec{RF: 3}, WithLogf(nil))
	ci := app.ConnInfo()
	if ci.Host != "10.0.0.1" || ci.Port != DefaultYSQLPort || ci.User != ybUser || ci.Database != ybDatabase {
		t.Errorf("ConnInfo = %+v", ci)
	}
}

func TestWaitProbesYSQLAndMasters(t *testing.T) {
	nodes, _ := threeNodes()
	var waited bool
	app := New(nodes, Spec{RF: 3}, WithLogf(nil), WithWaitPG(func(context.Context, pg.ConnInfo, time.Duration) error {
		waited = true
		return nil
	}))
	if err := app.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !waited {
		t.Error("Wait did not probe YSQL")
	}
}

func TestStopReverseOrderAndPkill(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{RF: 3}, WithLogf(nil))
	if err := app.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Each node pkills tserver; masters pkill yb-master too. The pattern must
	// be self-safe (bracketed) so pkill's own argv never matches itself.
	for i, ex := range execs {
		joined := strings.Join(ex.recorded(), "\n")
		if !strings.Contains(joined, "pkill -9 -f '[y]b-tserver'") {
			t.Errorf("node%d did not pkill tserver with a self-safe pattern: %v", i, ex.recorded())
		}
		if !strings.Contains(joined, "pkill -9 -f '[y]b-master'") {
			t.Errorf("node%d did not pkill master with a self-safe pattern: %v", i, ex.recorded())
		}
		if strings.Contains(joined, "pkill -9 -f 'yb-tserver'") || strings.Contains(joined, "pkill -9 -f 'yb-master'") {
			t.Errorf("node%d used a bare (self-matching) pkill pattern: %v", i, ex.recorded())
		}
	}
}

// TestStopIsBestEffortNonFatal covers the exit-137 teardown bug: pkill -9 -f
// matching its own invocation kills the SSH session itself (SIGKILL -> exit
// 137); a plain "no process matched" also exits 1. Neither should ever fail
// Stop — the instances are destroyed by the host right after regardless.
func TestStopIsBestEffortNonFatal(t *testing.T) {
	for _, exitCode := range []int{1, 137} {
		nodes, execs := threeNodes()
		for _, ex := range execs {
			ex.fallback = exec.Result{ExitCode: exitCode, Stderr: "boom"}
		}
		app := New(nodes, Spec{RF: 3}, WithLogf(nil))
		if err := app.Stop(context.Background()); err != nil {
			t.Fatalf("exit %d: Stop should be best-effort and never fail, got: %v", exitCode, err)
		}
	}
}

// TestDestroySurvivesStopFailure covers the reported bug end to end: a failed
// stop (exit 137 from pkill self-match) must not flip Destroy — and thus the
// test — to failed.
func TestDestroySurvivesStopFailure(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.fallback = exec.Result{ExitCode: 137, Stderr: "boom"}
	}
	app := New(nodes, Spec{RF: 3}, WithLogf(nil))
	sink := &recordingSink{}
	if err := app.Destroy(context.Background(), sink); err != nil {
		t.Fatalf("Destroy should survive a best-effort stop failure, got: %v", err)
	}
}

func TestDestroyCollectsBeforeWipe(t *testing.T) {
	nodes, execs := threeNodes()
	// Node 0 master log present so Collect adds it.
	execs[0].on("yb-master.INFO", exec.Result{ExitCode: 0, Stdout: "master log line\n"})
	app := New(nodes, Spec{RF: 3, InstallDir: "/opt/yb"}, WithLogf(nil))
	sink := &recordingSink{}
	if err := app.Destroy(context.Background(), sink); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// Collected the master log, and the wipe ran after the cat.
	if len(sink.names) == 0 {
		t.Error("Destroy collected nothing")
	}
	cmds := execs[0].recorded()
	catIdx := indexOfContains(cmds, "cat '/home/ec2-user/yb-data/yb-data/master/logs/yb-master.INFO'")
	wipeIdx := indexOfContains(cmds, "rm -rf '"+DefaultDataDir+"'")
	if catIdx < 0 || wipeIdx < 0 {
		t.Fatalf("expected both a collect cat and a wipe: %v", cmds)
	}
	if catIdx > wipeIdx {
		t.Error("wipe ran before collect — logs lost")
	}
}

func TestCollectPullsMasterAndTServerLogs(t *testing.T) {
	nodes, execs := threeNodes()
	for _, ex := range execs {
		ex.on("yb-master.INFO", exec.Result{ExitCode: 0, Stdout: "m-info\n"}).
			on("yb-tserver.INFO", exec.Result{ExitCode: 0, Stdout: "t-info\n"})
	}
	app := New(nodes, Spec{RF: 3}, WithLogf(nil))
	sink := &recordingSink{}
	if err := app.Collect(context.Background(), sink); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// 3 masters × yb-master.INFO + 3 tservers × yb-tserver.INFO all present.
	joined := strings.Join(sink.names, ",")
	for _, want := range []string{
		"10.0.0.1/yb-master.INFO", "10.0.0.3/yb-master.INFO",
		"10.0.0.1/yb-tserver.INFO", "10.0.0.3/yb-tserver.INFO",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Collect missing %q in %v", want, sink.names)
		}
	}
}

func TestDeployWhenConfigured(t *testing.T) {
	nodes, _ := threeNodes()
	dep := &recordingDeployer{}
	app := New(nodes, Spec{
		RF:         3,
		InstallDir: "/opt/yb",
		Deploy: &DeploySpec{
			Artifact:  artifact.Spec{Name: "yb-release"},
			RemoteDir: "/home/ec2-user",
		},
	}, WithWaitPG(okWaitPG), WithDeployer(dep), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if dep.calls != 1 {
		t.Errorf("deployer called %d times, want 1", dep.calls)
	}
	if dep.execs != 3 {
		t.Errorf("deployer got %d execs, want 3", dep.execs)
	}
	if dep.specName != "yb-release" {
		t.Errorf("deployer spec = %q, want yb-release", dep.specName)
	}
}

func TestExtraGflagsAppended(t *testing.T) {
	nodes, execs := threeNodes()
	app := New(nodes, Spec{
		RF:           3,
		InstallDir:   "/opt/yb",
		MasterFlags:  map[string]string{"master_flag_a": "1"},
		TServerFlags: map[string]string{"tserver_flag_b": "2"},
	}, WithWaitPG(okWaitPG), WithLogf(nil))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	cmds := execs[0].recorded()
	mCmd := cmds[indexOfContains(cmds, "yb-master")]
	if !strings.Contains(mCmd, "--master_flag_a=1") {
		t.Errorf("master cmd missing extra flag: %q", mCmd)
	}
	tCmd := cmds[indexOfContains(cmds, "yb-tserver")]
	if !strings.Contains(tCmd, "--tserver_flag_b=2") {
		t.Errorf("tserver cmd missing extra flag: %q", tCmd)
	}
}

// --- helpers ---

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

type recordingSink struct {
	mu    sync.Mutex
	names []string
}

func (s *recordingSink) Add(name, _ string, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
	return nil
}

func indexOfContains(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	return string(rune('0' + n))
}
