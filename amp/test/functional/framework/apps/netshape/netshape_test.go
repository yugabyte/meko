package netshape

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

type scripted struct {
	results []execpkg.Result
	cmds    []string
	n       int
}

func (s *scripted) runner() k8s.Runner {
	return func(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
		s.cmds = append(s.cmds, argv[len(argv)-1])
		if s.n >= len(s.results) {
			return execpkg.Result{ExitCode: 1}, nil
		}
		res := s.results[s.n]
		s.n++
		return res, nil
	}
}

func testHelper(s *scripted) *Helper {
	kube := k8s.New(k8s.Config{Namespace: "ampqa-t"}).WithRunner(s.runner())
	return Adopt(kube, "ampqa-netshape-t", "u-1", "node-a", "yb-tserver-0")
}

func TestScriptGroupsDestinationsByDelay(t *testing.T) {
	s := Script("eth0", []Delay{
		{Dst: "10.0.0.2", OneWay: 6 * time.Millisecond},
		{Dst: "10.0.0.3", OneWay: 23500 * time.Microsecond},
		{Dst: "10.0.0.9", OneWay: 6 * time.Millisecond},
		{Dst: "10.0.0.4", OneWay: 0},
	})
	for _, want := range []string{
		"prio bands 3 priomap 2 2 2 2 2 2 2 2 2 2 2 2 2 2 2 2",
		"parent 1:1 handle 10: netem limit 100000 delay 6000us",
		"parent 1:2 handle 20: netem limit 100000 delay 23500us",
		"match ip dst 10.0.0.2/32 flowid 1:1",
		"match ip dst 10.0.0.9/32 flowid 1:1",
		"match ip dst 10.0.0.3/32 flowid 1:2",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "10.0.0.4") {
		t.Errorf("a zero delay must not get a filter:\n%s", s)
	}
}

func TestScriptIsEmptyWithNothingToDelay(t *testing.T) {
	if s := Script("eth0", []Delay{{Dst: "10.0.0.4"}}); s != "" {
		t.Errorf("Script = %q, want empty", s)
	}
}

func TestParsePingRTT(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want time.Duration
	}{
		{"5 packets transmitted, 5 received, 0% packet loss, time 804ms\nrtt min/avg/max/mdev = 12.081/12.413/12.952/0.325 ms\n", 12413 * time.Microsecond},
		{"round-trip min/avg/max = 0.081/0.113/0.152 ms\n", 113 * time.Microsecond},
	} {
		got, err := ParsePingRTT(tc.out)
		if err != nil {
			t.Fatalf("ParsePingRTT(%q): %v", tc.out, err)
		}
		if got != tc.want {
			t.Errorf("ParsePingRTT(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
	if _, err := ParsePingRTT("5 packets transmitted, 0 received, 100% packet loss"); err == nil {
		t.Error("ParsePingRTT on a lossy run = nil, want an error")
	}
}

func TestRunEntersTheResolvedPID(t *testing.T) {
	s := &scripted{results: []execpkg.Result{{Stdout: "4242\n"}, {Stdout: "ok"}}}
	res, err := testHelper(s).Run(context.Background(), "tc qdisc show")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "ok" || !strings.Contains(s.cmds[1], "nsenter -t 4242 -n -- sh -c 'tc qdisc show'") {
		t.Errorf("Run entered %q", s.cmds[1])
	}
}

func TestClearTreatsAVanishedPodAsCleared(t *testing.T) {
	s := &scripted{results: []execpkg.Result{{Stdout: noPodPIDSentinel + "\n"}}}
	if err := testHelper(s).Clear(context.Background(), "eth0"); err != nil {
		t.Fatalf("Clear on a vanished pod = %v, want nil", err)
	}
}

func TestClearFailsWhenTheScanNeverRan(t *testing.T) {
	s := &scripted{results: []execpkg.Result{{ExitCode: 1, Stderr: `pods "ampqa-netshape-t" not found`}}}
	if err := testHelper(s).Clear(context.Background(), "eth0"); err == nil {
		t.Fatal("Clear with a dead helper pod = nil, want an error")
	}
}

// kubectl failing to reach the API server prints nothing on stdout; ping that
// ran and lost every packet does. Only the first is retried.
func TestPingRetriesAnExecThatNeverRan(t *testing.T) {
	pingRetryGap = 0
	s := &scripted{results: []execpkg.Result{
		{Stdout: "1\n"}, {ExitCode: 1, Stderr: "Unable to connect to the server: dial tcp 10.150.8.6:443: i/o timeout"},
		{Stdout: "1\n"}, {Stdout: "rtt min/avg/max/mdev = 0.4/0.5/0.6/0.1 ms\n"},
	}}
	rtt, err := testHelper(s).Ping(context.Background(), "10.0.0.2", 3)
	if err != nil || rtt != 500*time.Microsecond {
		t.Fatalf("Ping = %v, %v; want 500µs after one retry", rtt, err)
	}
	lossy := &scripted{results: []execpkg.Result{
		{Stdout: "1\n"}, {ExitCode: 1, Stdout: "3 packets transmitted, 0 received, 100% packet loss\n"},
	}}
	if _, err := testHelper(lossy).Ping(context.Background(), "10.0.0.2", 3); err == nil || len(lossy.cmds) != 2 {
		t.Fatalf("a lossy ping = %v after %d command(s); want an error with no retry", err, len(lossy.cmds))
	}
}

func TestRunReResolvesAPIDThatExitedBeforeNsenter(t *testing.T) {
	s := &scripted{results: []execpkg.Result{
		{Stdout: "146500\n"}, {ExitCode: 1, Stderr: "nsenter: cannot open /proc/146500/ns/net: No such file or directory"},
		{Stdout: "1207\n"}, {Stdout: "ok"},
	}}
	res, err := testHelper(s).Run(context.Background(), "tc qdisc show")
	if err != nil || res.Stdout != "ok" {
		t.Fatalf("Run = %q, %v; want ok after re-resolving", res.Stdout, err)
	}
	if !strings.Contains(s.cmds[3], "nsenter -t 1207 -n") {
		t.Errorf("second attempt entered %q, want pid 1207", s.cmds[3])
	}
}

func TestFindPodPIDScriptSortsNumerically(t *testing.T) {
	if !strings.Contains(findPodPIDScript("u-1"), "sort -n") {
		t.Error("the scan must take the lowest pid by number, not by glob order")
	}
}
