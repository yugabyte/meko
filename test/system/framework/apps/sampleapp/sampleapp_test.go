package sampleapp_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/sampleapp"
	"github.com/yugabyte/yb-amp/test/system/framework/artifact"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// scriptExec is a scripted fake RemoteExecutor: records every command and
// answers by substring match (in registration order), same convention as the
// other apps/* fakes in this module.
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

// sink records artifacts collected via apps.ArtifactSink.
type sink struct {
	mu    sync.Mutex
	names []string
	blobs map[string][]byte
}

func newSink() *sink { return &sink{blobs: map[string][]byte{}} }

func (s *sink) Add(name, _ string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
	s.blobs[name] = content
	return nil
}

func TestBuildCommandSqlInserts(t *testing.T) {
	spec := sampleapp.RunSpec{
		JarPath:         "/opt/yb-stress-sample-apps-1.4.90.jar",
		Workload:        sampleapp.SqlInserts,
		Hosts:           []string{"10.0.0.1", "10.0.0.2"},
		NumTables:       4,
		NumValueColumns: 30,
		NumThreadsWrite: 8,
		RunTime:         90 * time.Second,
		UUID:            "abc-123",
		Marker:          "run-1",
	}
	got := sampleapp.BuildCommand(spec)
	for _, want := range []string{
		"nohup java",
		"-Dampqa.sampleapp.marker='run-1'",
		"-jar '/opt/yb-stress-sample-apps-1.4.90.jar'",
		"--workload 'SqlInserts'",
		"--num_tables 4",
		"--num_value_columns 30",
		"--num_threads_write 8",
		"--run_time 90",
		"--uuid 'abc-123'",
		"--nodes '10.0.0.1:5433,10.0.0.2:5433'", // Sql* → YSQL port
		"> '/tmp/ampqa-sample-app-run-1.log' 2>&1 &",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command missing %q:\n%s", want, got)
		}
	}
	// A zero-valued knob (num_threads_read) must be omitted, not sent as 0.
	if strings.Contains(got, "--num_threads_read") {
		t.Errorf("zero num_threads_read should be omitted:\n%s", got)
	}
}

func TestBuildCommandTwoPhaseSqlDataLoad(t *testing.T) {
	base := sampleapp.RunSpec{
		JarPath:  "/opt/ssa.jar",
		Workload: sampleapp.SqlDataLoad,
		Hosts:    []string{"h1"},
		Marker:   "ph",
	}
	// Phase 1: schema only.
	phase1 := base
	phase1.SkipWorkload = true
	if got := sampleapp.BuildCommand(phase1); !strings.Contains(got, "--skip_workload") || strings.Contains(got, "--skip_ddl") {
		t.Errorf("phase 1 should emit --skip_workload only:\n%s", got)
	}
	// Phase 2: data only.
	phase2 := base
	phase2.SkipDDL = true
	if got := sampleapp.BuildCommand(phase2); !strings.Contains(got, "--skip_ddl") || strings.Contains(got, "--skip_workload") {
		t.Errorf("phase 2 should emit --skip_ddl only:\n%s", got)
	}
}

func TestBuildCommandYCQLWorkloadUsesYCQLPort(t *testing.T) {
	spec := sampleapp.RunSpec{
		JarPath:  "/opt/ssa.jar",
		Workload: sampleapp.CassandraKeyValue,
		Hosts:    []string{"h1"},
		Marker:   "cql",
	}
	got := sampleapp.BuildCommand(spec)
	if !strings.Contains(got, "--nodes 'h1:9042'") {
		t.Errorf("Cassandra* workload should target the YCQL port 9042:\n%s", got)
	}
}

func TestBuildCommandExtraPropsSortedAndSSLCert(t *testing.T) {
	spec := sampleapp.RunSpec{
		JarPath:  "/opt/ssa.jar",
		Workload: sampleapp.SqlTransactions,
		Hosts:    []string{"h1"},
		Marker:   "x",
		SSLCert:  "/certs/ca.crt",
		Props:    map[string]string{"zeta": "9", "alpha": "1"},
	}
	got := sampleapp.BuildCommand(spec)
	if i, j := strings.Index(got, "--alpha '1'"), strings.Index(got, "--zeta '9'"); i < 0 || j < 0 || i > j {
		t.Errorf("extra props should be sorted (alpha before zeta):\n%s", got)
	}
	if !strings.Contains(got, "--ssl_cert '/certs/ca.crt'") {
		t.Errorf("ssl_cert missing:\n%s", got)
	}
}

func TestStartLaunchesWorkload(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0}) // jar present
	app := sampleapp.New(ex, sampleapp.RunSpec{
		JarPath:  "/opt/ssa.jar",
		Workload: sampleapp.SqlInserts,
		Hosts:    []string{"h1"},
		Marker:   "m",
	})
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	cmds := ex.recorded()
	launched := false
	for _, c := range cmds {
		if strings.Contains(c, "nohup java") && strings.Contains(c, "--workload 'SqlInserts'") {
			launched = true
		}
	}
	if !launched {
		t.Errorf("Start did not launch the workload; commands: %v", cmds)
	}
}

func TestStartMissingJarNamesArtifactRoute(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 1}) // jar absent
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}})
	err := app.Start(context.Background())
	if err == nil {
		t.Fatal("want an error when jar is missing")
	}
	if !strings.Contains(err.Error(), "artifact") {
		t.Errorf("error should name the artifact route, got: %v", err)
	}
}

// fakeDeployer records that Deploy was called with the sampleapp artifact.
type fakeDeployer struct{ deployed []string }

func (d *fakeDeployer) Deploy(_ context.Context, _ []exec.RemoteExecutor, spec artifact.Spec, _ artifact.DeployOptions) error {
	d.deployed = append(d.deployed, spec.Name)
	return nil
}

func TestStartWithDeployFetchesJar(t *testing.T) {
	ex := &scriptExec{}
	dep := &fakeDeployer{}
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}, Marker: "m"},
		sampleapp.WithDeployer(dep),
		sampleapp.WithDeploy(sampleapp.DeploySpec{Artifact: artifact.Spec{Name: "sampleapp"}, RemoteDir: "/opt", Secret: "tok"}),
	)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(dep.deployed) != 1 || dep.deployed[0] != "sampleapp" {
		t.Errorf("expected the sampleapp artifact to be deployed, got %v", dep.deployed)
	}
	// With a deployer configured, Start must not probe for the jar itself.
	for _, c := range ex.recorded() {
		if strings.HasPrefix(c, "test -f") {
			t.Errorf("Start should not probe the jar when a deployer is configured: %q", c)
		}
	}
}

func TestReportKillCollectsBeforeKill(t *testing.T) {
	ex := &scriptExec{}
	ex.on("cat ", exec.Result{ExitCode: 0, Stdout: "workload log contents\n"})
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}, Marker: "m"})

	dst := newSink()
	if err := app.ReportKill(context.Background(), dst); err != nil {
		t.Fatalf("report kill: %v", err)
	}
	cmds := ex.recorded()
	var catIdx, killIdx = -1, -1
	for i, c := range cmds {
		if strings.HasPrefix(c, "cat ") && catIdx < 0 {
			catIdx = i
		}
		if strings.Contains(c, "pkill") && killIdx < 0 {
			killIdx = i
		}
	}
	if catIdx < 0 || killIdx < 0 {
		t.Fatalf("expected both a collect (cat) and a kill (pkill); commands: %v", cmds)
	}
	if catIdx > killIdx {
		t.Errorf("collect (index %d) must run BEFORE kill (index %d) — report_kill rule; commands: %v", catIdx, killIdx, cmds)
	}
	if got := string(dst.blobs["sample-app-m.log"]); !strings.Contains(got, "workload log contents") {
		t.Errorf("collected log missing content, got %q", got)
	}
}

func TestReportKillPkillPatternIsSelfSafe(t *testing.T) {
	ex := &scriptExec{}
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}, Marker: "unique-marker"})
	_ = app.ReportKill(context.Background(), newSink())
	var kill string
	for _, c := range ex.recorded() {
		if strings.Contains(c, "pkill") {
			kill = c
		}
	}
	if kill == "" {
		t.Fatal("no pkill command recorded")
	}
	// The bracketed first char keeps pkill from matching its own argv.
	if !strings.Contains(kill, "[a]mpqa.sampleapp.marker=unique-marker") {
		t.Errorf("pkill pattern is not self-safe (missing bracketed first char): %q", kill)
	}
}

func TestWaitReadyFailsWhenProcessDies(t *testing.T) {
	ex := &scriptExec{}
	ex.on("pgrep", exec.Result{ExitCode: 1})                  // not running
	ex.on("tail", exec.Result{ExitCode: 0, Stdout: "boom\n"}) // log tail
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}, Marker: "m"},
		sampleapp.WithReadyTimeout(2*time.Second))
	err := app.WaitReady(context.Background())
	if err == nil {
		t.Fatal("want an error when the process is not running")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should fold in the log tail, got: %v", err)
	}
}

func TestWaitReadySucceedsWhenLoadGoing(t *testing.T) {
	ex := &scriptExec{}
	ex.on("pgrep", exec.Result{ExitCode: 0})   // running
	ex.on("test -s", exec.Result{ExitCode: 0}) // log non-empty
	app := sampleapp.New(ex, sampleapp.RunSpec{JarPath: "/opt/ssa.jar", Workload: sampleapp.SqlInserts, Hosts: []string{"h1"}, Marker: "m"},
		sampleapp.WithReadyTimeout(5*time.Second))
	if err := app.WaitReady(context.Background()); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
}

func TestConfigApplyToOverlaysKnobs(t *testing.T) {
	cfg := sampleapp.Config{
		Workload: sampleapp.SqlBankTransfer, NumTables: 3, NumThreadsRead: 2,
		SkipWorkload: true, YSQLPort: 6000,
	}
	spec := cfg.ApplyTo(sampleapp.RunSpec{JarPath: "/keep.jar", Hosts: []string{"h1"}, Marker: "keep"})
	if spec.JarPath != "/keep.jar" || spec.Marker != "keep" {
		t.Errorf("ApplyTo must not clobber caller fields, got %+v", spec)
	}
	if spec.Workload != sampleapp.SqlBankTransfer || spec.NumTables != 3 || !spec.SkipWorkload || spec.YSQLPort != 6000 {
		t.Errorf("ApplyTo did not overlay config knobs, got %+v", spec)
	}
}
