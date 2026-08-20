package ybadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// scriptedExec answers each Run with the next scripted result, recording the commands given.
type scriptedExec struct {
	results []exec.Result
	errs    []error
	cmds    []string
	i       int
}

func (s *scriptedExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.cmds = append(s.cmds, cmd)
	i := s.i
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.i++
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.results[i], err
}

func (s *scriptedExec) Copy(context.Context, string, string) error { return nil }

func ok(stdout string) exec.Result { return exec.Result{Stdout: stdout} }

const tserverListing = `Tablet Server UUID                RPC Host/Port          Heartbeat delay  Status
c9a3b1de4f5061728394a5b6c7d8e9f0  10.4.0.11:9100         0.35s            ALIVE
1f2e3d4c5b6a70819283a4b5c6d7e8f9  10.4.0.12:9100         0.41s            ALIVE
0a1b2c3d4e5f60718293a4b5c6d7e8f0  10.4.0.13:9100         62.10s           DEAD
`

func TestTabletServersParsesRowsAndSkipsHeader(t *testing.T) {
	c := New(&scriptedExec{results: []exec.Result{ok(tserverListing)}}, "m:7100")
	got, err := c.TabletServers(context.Background())
	if err != nil {
		t.Fatalf("TabletServers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d servers, want 3: %+v", len(got), got)
	}
	if got[0].HostPort != "10.4.0.11:9100" || !got[0].Alive() {
		t.Errorf("first server = %+v", got[0])
	}
	if got[2].Alive() {
		t.Errorf("third server should be DEAD: %+v", got[2])
	}
}

func TestAliveTabletServersDropsDead(t *testing.T) {
	c := New(&scriptedExec{results: []exec.Result{ok(tserverListing)}}, "m:7100")
	got, err := c.AliveTabletServers(context.Background())
	if err != nil {
		t.Fatalf("AliveTabletServers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d alive, want 2: %+v", len(got), got)
	}
}

func TestBlacklistSendsTheSubcommandAndNodes(t *testing.T) {
	ex := &scriptedExec{results: []exec.Result{ok("")}}
	c := New(ex, "m1:7100,m2:7100")
	if err := c.Blacklist(context.Background(), BlacklistFull, "add", "10.4.0.11:9100"); err != nil {
		t.Fatalf("Blacklist: %v", err)
	}
	cmd := ex.cmds[0]
	for _, want := range []string{"--master_addresses m1:7100,m2:7100", "change_blacklist ADD", "10.4.0.11:9100"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q is missing %q", cmd, want)
		}
	}
}

func TestBlacklistRejectsAnEmptyNodeList(t *testing.T) {
	c := New(&scriptedExec{results: []exec.Result{ok("")}}, "m:7100")
	if err := c.Blacklist(context.Background(), BlacklistLeader, "REMOVE"); err == nil {
		t.Fatal("an empty host:port list should be rejected, not sent as a bare subcommand")
	}
}

func TestRunSurfacesANonZeroExit(t *testing.T) {
	ex := &scriptedExec{results: []exec.Result{{ExitCode: 1, Stderr: "Not the leader"}}}
	c := New(ex, "m:7100")
	_, err := c.LoadMoveCompletion(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Not the leader") {
		t.Fatalf("error = %v, want it to carry yb-admin's stderr", err)
	}
}

func TestParseLoadMovePercent(t *testing.T) {
	cases := map[string]float64{
		"Percent complete = 0 : 12 tablets remaining":  0,
		"Percent complete = 47.5 : 6 tablets":          47.5,
		"Percent complete = 100 : 0 tablets remaining": 100,
	}
	for out, want := range cases {
		got, err := parseLoadMovePercent(out)
		if err != nil {
			t.Errorf("%q: %v", out, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", out, got, want)
		}
	}
	if _, err := parseLoadMovePercent("Error: cluster config not found"); err == nil {
		t.Error("output with no percentage should error, not read as 0% complete")
	}
}

func TestLoadBalancerIdleAcceptsBothPrintings(t *testing.T) {
	for out, want := range map[string]bool{"Idle = 1\n": true, "Idle = 0\n": false, "Idle = true\n": true, "Idle = false\n": false} {
		c := New(&scriptedExec{results: []exec.Result{ok(out)}}, "m:7100")
		got, err := c.LoadBalancerIdle(context.Background())
		if err != nil {
			t.Errorf("%q: %v", out, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v, want %v", out, got, want)
		}
	}
}

func TestWaitLoadMoveReturnsWhenComplete(t *testing.T) {
	calls := 0
	c := New(&scriptedExec{results: []exec.Result{ok("")}}, "m:7100")
	pct, err := c.WaitLoadMove(context.Background(), func(context.Context) (float64, error) {
		calls++
		if calls < 3 {
			return float64(calls * 20), nil
		}
		return 100, nil
	}, time.Minute, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitLoadMove: %v", err)
	}
	if pct != 100 || calls != 3 {
		t.Errorf("pct = %v after %d samples, want 100 after 3", pct, calls)
	}
}

// A move that never finishes must fail with the percentage it stalled at.
func TestWaitLoadMoveReportsWhereItStalled(t *testing.T) {
	c := New(&scriptedExec{results: []exec.Result{ok("")}}, "m:7100")
	_, err := c.WaitLoadMove(context.Background(), func(context.Context) (float64, error) {
		return 42, nil
	}, 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "42%") {
		t.Fatalf("error = %v, want it to name the stall percentage", err)
	}
}

// A sample error mid-move is retried, not fatal: the masters can be electing.
func TestWaitLoadMoveRetriesASampleError(t *testing.T) {
	calls := 0
	c := New(&scriptedExec{results: []exec.Result{ok("")}}, "m:7100")
	_, err := c.WaitLoadMove(context.Background(), func(context.Context) (float64, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("leader not ready")
		}
		return 100, nil
	}, time.Minute, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitLoadMove: %v", err)
	}
	if calls != 2 {
		t.Errorf("sampled %d times, want the error retried then success", calls)
	}
}

// Asked right after a node rejoins, the balancer reports idle before it picks the work up, so idle must hold for several readings.
func TestWaitLoadBalancerIdleNeedsConsecutiveConfirmations(t *testing.T) {
	ex := &scriptedExec{results: []exec.Result{
		ok("Idle = 1"), ok("Idle = 0"), // a lone idle reading must not count
		ok("Idle = 1"), ok("Idle = 1"), ok("Idle = 1"),
	}}
	c := New(ex, "m:7100")
	if err := c.WaitLoadBalancerIdle(context.Background(), time.Minute, time.Millisecond); err != nil {
		t.Fatalf("WaitLoadBalancerIdle: %v", err)
	}
	if len(ex.cmds) != 5 {
		t.Errorf("polled %d times, want 5 — the streak must restart after a busy reading", len(ex.cmds))
	}
}

func TestWaitLoadBalancerIdleStreakResetsOnAnError(t *testing.T) {
	ex := &scriptedExec{
		results: []exec.Result{ok("Idle = 1"), {ExitCode: 1, Stderr: "not the leader"},
			ok("Idle = 1"), ok("Idle = 1"), ok("Idle = 1")},
	}
	c := New(ex, "m:7100")
	if err := c.WaitLoadBalancerIdle(context.Background(), time.Minute, time.Millisecond); err != nil {
		t.Fatalf("WaitLoadBalancerIdle: %v", err)
	}
	if len(ex.cmds) != 5 {
		t.Errorf("polled %d times, want 5", len(ex.cmds))
	}
}
