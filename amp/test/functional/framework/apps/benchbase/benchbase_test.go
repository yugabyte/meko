package benchbase_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// scriptExec is a scripted fake RemoteExecutor: records every command and
// answers by substring match (in registration order).
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	fallback exec.Result
}

type rule struct {
	match string
	res   exec.Result
	fn    func() exec.Result
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match: match, res: res})
	return s
}

func (s *scriptExec) onFunc(match string, fn func() exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match: match, fn: fn})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			if r.fn != nil {
				return r.fn(), nil
			}
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

func spec() benchbase.RunSpec {
	return benchbase.RunSpec{
		RunDir:       "/opt/benchbase-yugabyte",
		Benchmark:    benchbase.TPCC,
		Conn:         pg.ConnInfo{Host: "10.0.0.9", Port: 5433, User: "yugabyte", Password: "pw", Database: "amp"},
		Warehouses:   2,
		Terminals:    4,
		Duration:     30 * time.Second,
		PollInterval: time.Millisecond,
	}
}

func TestBuildRunCommandTPCC(t *testing.T) {
	got := benchbase.BuildRunCommand(spec())
	for _, want := range []string{
		"cd '/opt/benchbase-yugabyte' &&",
		"java -Xmx8g -jar benchbase.jar -b 'tpcc'",
		"-c 'config/yugabyte/sample_tpcc_config.xml'",
		"--create=true --load=true --execute=true",
		"-im 1000",
		"> 'benchbase.out' 2>&1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command missing %q:\n%s", want, got)
		}
	}
}

func TestBuildSedsPatchesConnectionAndLoad(t *testing.T) {
	seds := strings.Join(benchbase.BuildSeds(spec()), "\n")
	for _, want := range []string{
		"s|localhost:5433|10.0.0.9:5433|g", // target address
		"s|yugabyte?sslmode|amp?sslmode|g", // db name swap
		"<username>yugabyte</username>",    // user
		"<password>pw</password>",          // password
		"<scalefactor>2</scalefactor>",     // TPC-C warehouses → scalefactor
		"<terminals>4</terminals>",         // terminals
		"<time>30</time>",                  // duration seconds
		"<rate>unlimited</rate>",           // default rate
	} {
		if !strings.Contains(seds, want) {
			t.Errorf("seds missing %q:\n%s", want, seds)
		}
	}
}

func TestBuildSedsEscapesMetacharactersInCredentials(t *testing.T) {
	// A password with sed metacharacters (delimiter, ampersand, backslash) must
	// be escaped in the replacement, or the generated expression is corrupt.
	s := spec()
	s.Conn.Password = `a|b&c\d`
	seds := strings.Join(benchbase.BuildSeds(s), "\n")
	if !strings.Contains(seds, `<password>a\|b\&c\\d</password>`) {
		t.Errorf("password metacharacters not escaped in replacement:\n%s", seds)
	}
	// The raw, unescaped form must NOT appear (it would break the s|…| grammar).
	if strings.Contains(seds, `<password>a|b&c\d</password>`) {
		t.Errorf("raw metacharacters leaked into the sed expression:\n%s", seds)
	}
}

func TestBuildSedsLoadBalanceToggle(t *testing.T) {
	// nil → the smart-driver URL is left as the fork ships it (no sed).
	if seds := strings.Join(benchbase.BuildSeds(spec()), "\n"); strings.Contains(seds, "load-balance") {
		t.Errorf("nil LoadBalance should leave the config untouched:\n%s", seds)
	}
	// false → force load-balance off (the in-cluster/Service fix).
	s := spec()
	s.LoadBalance = benchbase.Ptr(false)
	if seds := strings.Join(benchbase.BuildSeds(s), "\n"); !strings.Contains(seds, "s|load-balance=true|load-balance=false|g") {
		t.Errorf("LoadBalance=false should sed load-balance off:\n%s", seds)
	}
}

func TestBuildSedsMultiHostURL(t *testing.T) {
	// Every named tserver lands in the JDBC URL as a pgjdbc multi-host authority,
	// and the driver's own cluster-aware balancing (which discovers addresses via
	// yb_servers()) is replaced by pgjdbc's per-connection host shuffle.
	s := spec()
	s.Addrs = []string{"yb-tserver-0.yb-tservers.ns.svc.cluster.local:5433", "yb-tserver-1.yb-tservers.ns.svc.cluster.local:5433"}
	s.LoadBalance = benchbase.Ptr(false)
	seds := strings.Join(benchbase.BuildSeds(s), "\n")
	for _, want := range []string{
		"s|localhost:5433|yb-tserver-0.yb-tservers.ns.svc.cluster.local:5433,yb-tserver-1.yb-tservers.ns.svc.cluster.local:5433|g",
		`s|load-balance=true|load-balance=false\&amp;loadBalanceHosts=true|g`,
	} {
		if !strings.Contains(seds, want) {
			t.Errorf("seds missing %q:\n%s", want, seds)
		}
	}
	// Conn's single address must not also be written into the URL.
	if strings.Contains(seds, "s|localhost:5433|10.0.0.9:5433|g") {
		t.Errorf("Addrs should replace Conn's single address in the URL:\n%s", seds)
	}
}

func TestBuildSedsSingleAddrKeepsSingleHostURLShape(t *testing.T) {
	// A one-element Addrs must produce EXACTLY the URL a plain Conn target does —
	// no comma list, no loadBalanceHosts (stored baselines assume that shape).
	s := spec()
	s.Addrs = []string{"10.0.0.9:5433"}
	s.LoadBalance = benchbase.Ptr(false)
	seds := strings.Join(benchbase.BuildSeds(s), "\n")
	if !strings.Contains(seds, "s|localhost:5433|10.0.0.9:5433|g") {
		t.Errorf("single Addrs entry should render the plain address:\n%s", seds)
	}
	if strings.Contains(seds, "loadBalanceHosts") {
		t.Errorf("a single host needs no loadBalanceHosts:\n%s", seds)
	}
}

func TestBuildSedsMultiHostKeepsSmartBalancingWhenAsked(t *testing.T) {
	// LoadBalance=true is an explicit ask for the smart driver's own cluster-aware
	// balancing; loadBalanceHosts is pgjdbc's alternative to it, never an addition.
	s := spec()
	s.Addrs = []string{"h1:5433", "h2:5433"}
	s.LoadBalance = benchbase.Ptr(true)
	seds := strings.Join(benchbase.BuildSeds(s), "\n")
	if !strings.Contains(seds, "s|load-balance=true|load-balance=true|g") || strings.Contains(seds, "loadBalanceHosts") {
		t.Errorf("LoadBalance=true should leave smart balancing on and add nothing:\n%s", seds)
	}
}

func TestScalefactorPrefersExplicitOverWarehouses(t *testing.T) {
	s := spec()
	s.Scalefactor = 10 // explicit wins over Warehouses=2
	seds := strings.Join(benchbase.BuildSeds(s), "\n")
	if !strings.Contains(seds, "<scalefactor>10</scalefactor>") {
		t.Errorf("explicit scalefactor should win over warehouses:\n%s", seds)
	}
}

func TestPhaseFlags(t *testing.T) {
	s := spec()
	s.Load = benchbase.Ptr(false)
	s.Execute = benchbase.Ptr(false)
	got := benchbase.BuildRunCommand(s)
	if !strings.Contains(got, "--create=true --load=false --execute=false") {
		t.Errorf("phase flags not honored:\n%s", got)
	}
}

const summaryJSON = `{
  "Throughput (requests/second)": 123.45,
  "Goodput (requests/second)": 120.0,
  "Latency Distribution": {
    "Median Latency (microseconds)": 8100.0,
    "95th Percentile Latency (microseconds)": 25000.0,
    "99th Percentile Latency (microseconds)": 41000.0
  }
}`

func TestRunParsesSummary(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0})                           // jar present
	ex.on("nohup", exec.Result{ExitCode: 0, Stdout: "4242\n"})           // detached start -> pid
	ex.on("benchbase.out.exit", exec.Result{ExitCode: 0, Stdout: "0\n"}) // finished ok
	ex.on("summary.json", exec.Result{ExitCode: 0, Stdout: summaryJSON}) // results summary
	res, err := benchbase.Run(context.Background(), ex, spec())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ThroughputReqPerSec != 123.45 {
		t.Errorf("throughput = %v, want 123.45", res.ThroughputReqPerSec)
	}
	if res.GoodputReqPerSec != 120.0 {
		t.Errorf("goodput = %v, want 120.0", res.GoodputReqPerSec)
	}
	if res.MedianLatencyUS != 8100.0 {
		t.Errorf("median latency = %v, want 8100", res.MedianLatencyUS)
	}
	if res.P99LatencyUS != 41000.0 {
		t.Errorf("p99 latency = %v, want 41000", res.P99LatencyUS)
	}
	if res.P95LatencyUS != 25000.0 {
		t.Errorf("p95 latency = %v, want 25000", res.P95LatencyUS)
	}
	if !strings.Contains(res.Raw, "Throughput") {
		t.Error("Raw must carry the summary")
	}
}

func TestRunMissingJarNamesArtifactRoute(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 1}) // jar absent
	_, err := benchbase.Run(context.Background(), ex, spec())
	if err == nil {
		t.Fatal("want an error when benchbase.jar is missing")
	}
	if !strings.Contains(err.Error(), "artifact") {
		t.Errorf("error should name the artifact route, got: %v", err)
	}
}

func TestRunExitNonZeroKeepsRawOutput(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0})
	ex.on("nohup", exec.Result{ExitCode: 0, Stdout: "4242\n"})
	ex.on("benchbase.out.exit", exec.Result{ExitCode: 0, Stdout: "1\n"})
	ex.on("benchbase.out", exec.Result{ExitCode: 0, Stdout: "starting...\nconnection refused\n"})
	res, err := benchbase.Run(context.Background(), ex, spec())
	if err == nil {
		t.Fatal("want an error on non-zero exit")
	}
	if !strings.Contains(res.Raw, "connection refused") {
		t.Errorf("Result.Raw should carry the failure output, got: %q", res.Raw)
	}
}

func TestRunUnparseableSummaryLeavesZeroFields(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0})
	ex.on("nohup", exec.Result{ExitCode: 0, Stdout: "4242\n"})
	ex.on("benchbase.out.exit", exec.Result{ExitCode: 0, Stdout: "0\n"})
	ex.on("summary.json", exec.Result{ExitCode: 1}) // no summary file
	ex.on("benchbase.out", exec.Result{ExitCode: 0, Stdout: "no numbers here\n"})
	res, err := benchbase.Run(context.Background(), ex, spec())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ThroughputReqPerSec != 0 || res.P95LatencyUS != 0 {
		t.Errorf("expected zero metrics on unparseable output, got %+v", res)
	}
	if res.Raw == "" {
		t.Error("Raw must still be populated")
	}
}

func TestConfigApplyToOverlaysKnobs(t *testing.T) {
	cfg := benchbase.Config{Benchmark: benchbase.SmallBank, Warehouses: 5, Terminals: 6, Duration: 90 * time.Second, Rate: "1000"}
	s := cfg.ApplyTo(benchbase.RunSpec{RunDir: "/keep", Conn: pg.ConnInfo{Host: "keep"}})
	if s.RunDir != "/keep" || s.Conn.Host != "keep" {
		t.Errorf("ApplyTo must not clobber caller fields, got %+v", s)
	}
	if s.Benchmark != benchbase.SmallBank || s.Warehouses != 5 || s.Terminals != 6 || s.Rate != "1000" {
		t.Errorf("ApplyTo did not overlay config knobs, got %+v", s)
	}
}

// The run redirects into this path, so a caller can read the log WHILE the
// benchmark is going. Run only surfaces the output once it returns, which for a
// benchmark that outlives the scenario is never.
func TestResolvedLogPathIncludesTheRunDir(t *testing.T) {
	spec := benchbase.RunSpec{RunDir: "/opt/ampqa/benchbase-yugabyte"}
	if got, want := spec.ResolvedLogPath(), "/opt/ampqa/benchbase-yugabyte/benchbase.out"; got != want {
		t.Errorf("ResolvedLogPath = %q, want %q", got, want)
	}
	if got := (benchbase.RunSpec{}).ResolvedLogPath(); got != "benchbase.out" {
		t.Errorf("ResolvedLogPath with no run dir = %q", got)
	}
	// It must be the file the command actually writes to.
	cmd := benchbase.BuildRunCommand(spec)
	if !strings.Contains(cmd, "> 'benchbase.out'") {
		t.Errorf("run command does not redirect into the log this path names: %s", cmd)
	}
}

// The "&" must bind to the nohup command only: with "rm && nohup … &" the
// backgrounded job is the whole list, $! is a wrapping subshell, and rm races
// the first poll of a stale exit file.
func TestBuildStartCommandDetachesOnlyTheNohup(t *testing.T) {
	got := benchbase.BuildStartCommand(spec())
	if !strings.Contains(got, "'; nohup sh -c ") {
		t.Errorf("rm must complete synchronously before the detached start:\n%s", got)
	}
	if !strings.HasSuffix(got, "& echo $!") {
		t.Errorf("the start must background nohup and return its pid:\n%s", got)
	}
	if !strings.Contains(got, "rm -f '/opt/benchbase-yugabyte/benchbase.out.exit'") {
		t.Errorf("a stale exit file must be removed first:\n%s", got)
	}
}

func TestRunSurvivesRunningPollsBeforeExit(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0})
	ex.on("nohup", exec.Result{ExitCode: 0, Stdout: "4242\n"})
	var polls atomic.Int32
	ex.onFunc("benchbase.out.exit", func() exec.Result {
		if polls.Add(1) < 3 {
			return exec.Result{ExitCode: 0, Stdout: "RUNNING\n"}
		}
		return exec.Result{ExitCode: 0, Stdout: "0\n"}
	})
	ex.on("summary.json", exec.Result{ExitCode: 0, Stdout: summaryJSON})
	if _, err := benchbase.Run(context.Background(), ex, spec()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if polls.Load() < 3 {
		t.Fatalf("expected at least 3 polls, got %d", polls.Load())
	}
}

// A pid gone with no exit file gets one grace poll, then is reported killed.
func TestRunReportsExternallyKilledProcess(t *testing.T) {
	ex := &scriptExec{}
	ex.on("test -f", exec.Result{ExitCode: 0})
	ex.on("nohup", exec.Result{ExitCode: 0, Stdout: "4242\n"})
	ex.on("benchbase.out.exit", exec.Result{ExitCode: 0, Stdout: "GONE\n"})
	_, err := benchbase.Run(context.Background(), ex, spec())
	if err == nil || !strings.Contains(err.Error(), "killed externally") {
		t.Fatalf("want the externally-killed verdict, got: %v", err)
	}
}
