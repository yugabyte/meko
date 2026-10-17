package features

import (
	"context"
	"io"
	"testing"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func scriptedKube(results ...execpkg.Result) (*k8s.Kubectl, *int) {
	n := 0
	run := func(_ context.Context, _ io.Reader, _ []string) (execpkg.Result, error) {
		res := execpkg.Result{ExitCode: 1}
		if n < len(results) {
			res = results[n]
		}
		n++
		return res, nil
	}
	return k8s.New(k8s.Config{Namespace: "ampqa-cp-t"}).WithRunner(run), &n
}

func TestPodCgroupLimitsRetriesAnEmptyExec(t *testing.T) {
	kube, n := scriptedKube(
		execpkg.Result{ExitCode: 1},
		execpkg.Result{Stdout: "200000 100000\n1073741824\n"},
	)
	cpu, mem, err := podCgroupLimits(context.Background(), kube, "yb-ep-1-abc")
	if err != nil {
		t.Fatalf("podCgroupLimits: %v", err)
	}
	if cpu != 2000 || mem != 1<<30 {
		t.Errorf("limits = %dm %d bytes, want 2000m %d bytes", cpu, mem, 1<<30)
	}
	if *n != 2 {
		t.Errorf("ran the probe %d times, want 2", *n)
	}
}
