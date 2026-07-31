package sysbench_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sysbench"
	"github.com/yugabyte/amp/test/functional/framework/exec"
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

// available scripts "command -v sysbench" as found.
func available() *scriptExec {
	s := &scriptExec{}
	s.on("command -v sysbench", exec.Result{ExitCode: 0, Stdout: "/usr/bin/sysbench\n"})
	return s
}

func TestEnsureInstalledCommand(t *testing.T) {
	ex := &scriptExec{}
	if err := sysbench.EnsureInstalled(context.Background(), ex); err != nil {
		t.Fatalf("ensure installed: %v", err)
	}
	got := ex.recorded()[0]
	for _, want := range []string{"command -v sysbench", "apt-get install", "sysbench"} {
		if !strings.Contains(got, want) {
			t.Errorf("install command missing %q:\n%s", want, got)
		}
	}
}

func TestPrepareCommandConstruction(t *testing.T) {
	ex := available()
	conn := pg.ConnInfo{Host: "10.0.0.5", Port: 5433, User: "yugabyte", Password: "secret", Database: "yugabyte"}
	err := sysbench.Prepare(context.Background(), ex, sysbench.PrepareOptions{
		Conn: conn, Script: "oltp_read_write", Tables: 4, TableSize: 100000,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cmds := ex.recorded()
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2 (availability check + prepare)", len(cmds))
	}
	got := cmds[1]
	for _, want := range []string{
		"PGPASSWORD='secret'",
		"sysbench 'oltp_read_write'",
		"--db-driver=pgsql",
		"--pgsql-host='10.0.0.5'",
		"--pgsql-port=5433",
		"--pgsql-user='yugabyte'",
		"--pgsql-db='yugabyte'",
		"--tables=4",
		"--table-size=100000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prepare command missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "prepare") {
		t.Errorf("prepare must be the trailing sysbench verb:\n%s", got)
	}
	if strings.Contains(got, "--pgsql-password") {
		t.Errorf("password must travel via PGPASSWORD, never argv:\n%s", got)
	}
}

func TestRunCommandConstruction(t *testing.T) {
	ex := available()
	conn := pg.ConnInfo{Host: "endpoint", Port: 5433, User: "yugabyte", Database: "yugabyte"}
	_, err := sysbench.Run(context.Background(), ex, sysbench.RunOptions{
		Conn: conn, Tables: 4, TableSize: 100000, Threads: 8,
		Duration: 60 * time.Second, ReportInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := ex.recorded()[1]
	for _, want := range []string{
		"sysbench oltp_read_write", // empty Script → the unquoted default
		"--threads=8",
		"--time=60",
		"--report-interval=10",
		"--tables=4",
		"--table-size=100000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run command missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "run") {
		t.Errorf("run must be the trailing sysbench verb:\n%s", got)
	}
	if strings.Contains(got, "PGPASSWORD") {
		t.Errorf("no password set — must not emit PGPASSWORD: %s", got)
	}
}

func TestMissingBinaryDetection(t *testing.T) {
	ex := &scriptExec{}
	ex.on("command -v sysbench", exec.Result{ExitCode: 1, Stderr: "not found"})

	if err := sysbench.Prepare(context.Background(), ex, sysbench.PrepareOptions{}); err == nil {
		t.Fatal("want an error when sysbench is missing")
	} else if !strings.Contains(err.Error(), "EnsureInstalled") {
		t.Errorf("error should name the install route, got: %v", err)
	}

	if _, err := sysbench.Run(context.Background(), ex, sysbench.RunOptions{}); err == nil {
		t.Fatal("want an error when sysbench is missing")
	} else if !strings.Contains(err.Error(), "EnsureInstalled") {
		t.Errorf("error should name the install route, got: %v", err)
	}
}

func TestRunExitNonZeroIncludesRawOutput(t *testing.T) {
	ex := available()
	ex.on("--threads=", exec.Result{ExitCode: 1, Stdout: "sysbench 1.0.20\n", Stderr: "FATAL: connection failed\n"})
	res, err := sysbench.Run(context.Background(), ex, sysbench.RunOptions{Threads: 1})
	if err == nil {
		t.Fatal("want an error on non-zero exit")
	}
	if !strings.Contains(res.Raw, "FATAL: connection failed") {
		t.Errorf("Result.Raw should carry the failure output, got: %q", res.Raw)
	}
}

// --- output parsing, golden sample (sysbench 1.0.20) ---

const sysbenchOutput = `sysbench 1.0.20 (using bundled LuaJIT 2.1.0-beta2)

Running the test with following options:
Number of threads: 8
Report interval: 10 seconds
Initializing random number generator from current time

Initializing worker threads...

Threads started!

[ 10s ] thds: 8 tps: 411.58 qps: 8237.42 (r/w/o: 5767.44/1646.13/823.85) lat (ms,95%): 44.98 err/s: 0.00 reconn/s: 0.00
[ 20s ] thds: 8 tps: 398.21 qps: 7963.90 (r/w/o: 5574.73/1592.74/796.43) lat (ms,95%): 46.63 err/s: 0.00 reconn/s: 0.00

SQL statistics:
    queries performed:
        read:                            337638
        write:                           96468
        other:                           48234
        total:                           482340
    transactions:                        24117  (401.93 per sec.)
    queries:                             482340 (8038.63 per sec.)
    ignored errors:                      0      (0.00 per sec.)
    reconnects:                          0      (0.00 per sec.)

General statistics:
    total events:                        24117
    total time:                          60.0021s

Latency (ms):
         min:                                    2.32
         avg:                                   19.90
         max:                                  135.13
         95th percentile:                       38.25
         sum:                               479895.06

Threads fairness:
    events (avg/stddev):           3014.6250/21.31
    execution time (avg/stddev):   59.9869/0.00
`

func TestParseOutputGoldenSample(t *testing.T) {
	ex := available()
	ex.on("--threads=", exec.Result{ExitCode: 0, Stdout: sysbenchOutput})
	res, err := sysbench.Run(context.Background(), ex, sysbench.RunOptions{Threads: 8})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TPS != 401.93 {
		t.Errorf("tps = %v, want 401.93", res.TPS)
	}
	if res.LatencyAvgMs != 19.90 {
		t.Errorf("latency avg = %v, want 19.90", res.LatencyAvgMs)
	}
	if res.LatencyP95Ms != 38.25 {
		t.Errorf("latency p95 = %v, want 38.25", res.LatencyP95Ms)
	}
	if !strings.Contains(res.Raw, "sysbench 1.0.20") {
		t.Error("Raw must carry the full captured output")
	}
}

func TestParseOutputMissingFieldsStayZero(t *testing.T) {
	ex := available()
	ex.on("--threads=", exec.Result{ExitCode: 0, Stdout: "some unrelated output\n"})
	res, err := sysbench.Run(context.Background(), ex, sysbench.RunOptions{Threads: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TPS != 0 || res.LatencyAvgMs != 0 || res.LatencyP95Ms != 0 {
		t.Errorf("expected zero fields on unparseable output, got %+v", res)
	}
	if res.Raw == "" {
		t.Error("Raw must still be populated")
	}
}
