package pgbench_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pgbench"
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

// available scripts "command -v pgbench" as found.
func available() *scriptExec {
	s := &scriptExec{}
	s.on("command -v pgbench", exec.Result{ExitCode: 0, Stdout: "/usr/bin/pgbench\n"})
	return s
}

func TestInitCommandConstruction(t *testing.T) {
	ex := available()
	conn := pg.ConnInfo{Host: "10.0.0.5", Port: 55433, User: "cloud_admin", Password: "cloud_admin", Database: "postgres"}
	if err := pgbench.Init(context.Background(), ex, pgbench.InitOptions{Conn: conn, Scale: 10}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cmds := ex.recorded()
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2 (availability check + init)", len(cmds))
	}
	got := cmds[1]
	for _, want := range []string{
		"PGPASSWORD='cloud_admin'",
		"pgbench -i",
		"-s 10",
		"-h '10.0.0.5'",
		"-p 55433",
		"-U 'cloud_admin'",
		"-d 'postgres'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("init command missing %q:\n%s", want, got)
		}
	}
}

func TestRunCommandConstruction(t *testing.T) {
	ex := available()
	conn := pg.ConnInfo{Host: "endpoint", Port: 5432, User: "cloud_admin", Database: "postgres"}
	_, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{
		Conn: conn, Clients: 10, Jobs: 2, Duration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := ex.recorded()[1]
	for _, want := range []string{"-c 10", "-j 2", "-T 30", "-h 'endpoint'"} {
		if !strings.Contains(got, want) {
			t.Errorf("run command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PGPASSWORD") {
		t.Errorf("no password set — must not emit PGPASSWORD: %s", got)
	}
}

func TestMissingBinaryDetection(t *testing.T) {
	ex := &scriptExec{}
	ex.on("command -v pgbench", exec.Result{ExitCode: 1, Stderr: "not found"})

	if err := pgbench.Init(context.Background(), ex, pgbench.InitOptions{}); err == nil {
		t.Fatal("want an error when pgbench is missing")
	} else if !strings.Contains(err.Error(), "artifact") {
		t.Errorf("error should name the artifact route, got: %v", err)
	}

	if _, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{}); err == nil {
		t.Fatal("want an error when pgbench is missing")
	} else if !strings.Contains(err.Error(), "artifact") {
		t.Errorf("error should name the artifact route, got: %v", err)
	}
}

func TestRunExitNonZeroIncludesRawOutput(t *testing.T) {
	ex := available()
	ex.on("pgbench -c", exec.Result{ExitCode: 2, Stdout: "starting vacuum...end.\n", Stderr: "connection to server failed\n"})
	res, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{Clients: 1})
	if err == nil {
		t.Fatal("want an error on non-zero exit")
	}
	if !strings.Contains(res.Raw, "connection to server failed") {
		t.Errorf("Result.Raw should carry the failure output, got: %q", res.Raw)
	}
}

// --- output parsing, golden samples ---

const pgbenchModernOutput = `pgbench (16.1)
starting vacuum...end.
transaction type: <builtin: TPC-B (sort of)>
scaling factor: 1
query mode: simple
number of clients: 10
number of threads: 2
maximum number of tries: 1
duration: 10 s
number of transactions actually processed: 4521
number of failed transactions: 0 (0.000%)
latency average = 22.121 ms
initial connection time = 15.230 ms
tps = 452.059738 (without initial connection time)
`

const pgbenchLegacyOutput = `starting vacuum...end.
transaction type: TPC-B (sort of)
scaling factor: 1
query mode: simple
number of clients: 10
number of threads: 1
duration: 10 s
number of transactions actually processed: 6023
latency average = 16.598 ms
tps = 602.881233 (including connections establishing)
tps = 610.442198 (excluding connections establishing)
`

func TestParseOutputModernSample(t *testing.T) {
	ex := available()
	ex.on("pgbench -c", exec.Result{ExitCode: 0, Stdout: pgbenchModernOutput})
	res, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{Clients: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TPS != 452.059738 {
		t.Errorf("tps = %v, want 452.059738", res.TPS)
	}
	if res.LatencyAvgMs != 22.121 {
		t.Errorf("latency = %v, want 22.121", res.LatencyAvgMs)
	}
	if !strings.Contains(res.Raw, "pgbench (16.1)") {
		t.Error("Raw must carry the full captured output")
	}
}

func TestParseOutputLegacyTwoTPSLinesPrefersExcluding(t *testing.T) {
	ex := available()
	ex.on("pgbench -c", exec.Result{ExitCode: 0, Stdout: pgbenchLegacyOutput})
	res, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{Clients: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TPS != 610.442198 {
		t.Errorf("tps = %v, want 610.442198 (the excluding figure)", res.TPS)
	}
	if res.LatencyAvgMs != 16.598 {
		t.Errorf("latency = %v, want 16.598", res.LatencyAvgMs)
	}
}

func TestParseOutputMissingFieldsStayZero(t *testing.T) {
	ex := available()
	ex.on("pgbench -c", exec.Result{ExitCode: 0, Stdout: "starting vacuum...end.\nsome unrelated output\n"})
	res, err := pgbench.Run(context.Background(), ex, pgbench.RunOptions{Clients: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TPS != 0 || res.LatencyAvgMs != 0 {
		t.Errorf("expected zero fields on unparseable output, got tps=%v latency=%v", res.TPS, res.LatencyAvgMs)
	}
	if res.Raw == "" {
		t.Error("Raw must still be populated")
	}
}
