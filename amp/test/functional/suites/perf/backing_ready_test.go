package perf

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The readiness probe must run INSIDE the cluster. Dialling the tserver's pod IP
// from this process only works where the coordinator has a route into the pod
// CIDR; where it does not, every attempt times out against a healthy backing —
// which cost four tests their whole readiness budget on 0.260810.0.
func TestYSQLProbeRunsInsideAPodAndReResolvesIt(t *testing.T) {
	orig := ysqlProbeInterval
	ysqlProbeInterval = time.Millisecond
	t.Cleanup(func() { ysqlProbeInterval = orig })

	var cmds []string
	var pods []string
	answer := 2 // fail twice, then answer

	kube := k8s.New(k8s.Config{Context: "ctx", Namespace: "ampqa-yb-1"}).
		WithRunner(func(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
			joined := strings.Join(argv, " ")
			cmds = append(cmds, joined)
			switch {
			case strings.Contains(joined, "get pod"):
				pods = append(pods, "lookup")
				return exec.Result{Stdout: "yb-tserver-0"}, nil
			case strings.Contains(joined, "ysqlsh"):
				if answer > 0 {
					answer--
					return exec.Result{Stdout: "", Stderr: "could not connect", ExitCode: 2}, nil
				}
				return exec.Result{Stdout: "1\n"}, nil
			}
			return exec.Result{}, nil
		})

	err := waitYSQLInCluster(context.Background(), kube,
		ybk8s.Info{YSQLPort: 5433, User: "yugabyte"}, 30*time.Second)
	if err != nil {
		t.Fatalf("waitYSQLInCluster: %v", err)
	}

	var execs int
	for _, c := range cmds {
		if strings.Contains(c, "ysqlsh") {
			execs++
			if !strings.Contains(c, "exec") {
				t.Errorf("the probe ran outside the cluster: %s", c)
			}
			if !strings.Contains(c, "yb-tserver-0") {
				t.Errorf("the probe did not target a tserver pod: %s", c)
			}
		}
		if strings.Contains(c, "5433") && strings.Contains(c, "10.") {
			t.Errorf("the probe dialled a pod IP, which is unroutable from here: %s", c)
		}
		if strings.Contains(c, "bin/ysqlsh") && !strings.Contains(c, "command -v ysqlsh") {
			// A relative path depends on the container's working directory and exited
			// 127 for a full 5-minute budget against a healthy backing.
			t.Errorf("the probe assumes where ysqlsh lives instead of resolving it: %s", c)
		}
	}
	if execs != 3 {
		t.Errorf("probed %d times, want 3 (two failures then success)", execs)
	}
	// Re-resolved every attempt, so a replaced tserver does not pin the probe to a
	// pod that no longer exists.
	if len(pods) != 3 {
		t.Errorf("resolved the pod %d times for %d probes, want one lookup per attempt", len(pods), execs)
	}
}
