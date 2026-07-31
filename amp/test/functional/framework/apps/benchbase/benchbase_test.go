package benchbase_test

import (
	"context"
	"strings"
	"sync"
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

func spec() benchbase.RunSpec {
	return benchbase.RunSpec{
		RunDir:     "/opt/benchbase-yugabyte",
		Benchmark:  benchbase.TPCC,
		Conn:       pg.ConnInfo{Host: "10.0.0.9", Port: 5433, User: "yugabyte", Password: "pw", Database: "amp"},
		Warehouses: 2,
		Terminals:  4,
		Duration:   30 * time.Second,
	}
}

func TestBuildRunCommandTPCC(t *testing.T) {
	got := benchbase.BuildRunCommand(spec())
	for _, want := range []string{
		"cd '/opt/benchbase-yugabyte' &&",
		"java -Xmx8g -jar benchbase.jar -b 'tpcc'",
		"-c 'config/yugabyte/sample_tpcc_config.xml'",
		"--create=true --load=true --execute=true",
		"-im 5000",
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
	ex.on("test -f", exec.Result{ExitCode: 0})                                    // jar present
	ex.on("java -Xmx", exec.Result{ExitCode: 0, Stdout: "Benchmark complete.\n"}) // run
	ex.on("summary.json", exec.Result{ExitCode: 0, Stdout: summaryJSON})          // results summary
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
	if !strings.Contains(res.Raw, "Benchmark complete") {
		t.Error("Raw must carry the captured stdout")
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
	ex.on("java -Xmx", exec.Result{ExitCode: 1, Stdout: "starting...\n", Stderr: "connection refused\n"})
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
	ex.on("java -Xmx", exec.Result{ExitCode: 0, Stdout: "no numbers here\n"})
	ex.on("summary.json", exec.Result{ExitCode: 1}) // no summary file
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
