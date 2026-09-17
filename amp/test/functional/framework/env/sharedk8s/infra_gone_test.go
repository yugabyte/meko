package sharedk8s

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func TestSharedInfraGone(t *testing.T) {
	for _, c := range []struct {
		name  string
		phase string
		err   error
		want  bool
	}{
		{"active", "Active", nil, false},
		{"terminating", "Terminating", nil, true},
		{"deleted", "", errors.New(`Error from server (NotFound): namespaces "ampqa-cp-1" not found`), true},
		{"unreachable", "", errors.New("dial tcp: i/o timeout"), false},
	} {
		p := &Provider{}
		// deploy mode with a stack already up; the never-deployed case is below.
		p.deploy = true
		p.app = &ampstack.App{}
		var argv string
		p.kube = k8s.New(k8s.Config{Context: "x", Namespace: "ampqa-cp-1"}).WithRunner(
			func(_ context.Context, _ io.Reader, a []string) (execpkg.Result, error) {
				argv = strings.Join(a, " ")
				return execpkg.Result{Stdout: c.phase}, c.err
			})
		where, gone := p.SharedInfraGone(context.Background())
		if gone != c.want {
			t.Errorf("%s: gone = %v, want %v", c.name, gone, c.want)
		}
		if gone && where == "" {
			t.Errorf("%s: gone with no location", c.name)
		}
		// -n would name the very namespace being asked about.
		if strings.Contains(argv, "-n ampqa-cp-1") {
			t.Errorf("%s: namespace lookup carried -n: %s", c.name, argv)
		}
	}
}

// An all-dedicated run never deploys the shared stack, so its namespace is
// absent by design — answering "gone" there would rewrite every product failure.
func TestSharedInfraGoneIgnoresAStackThatNeverDeployed(t *testing.T) {
	p := &Provider{deploy: true}
	asked := false
	p.kube = k8s.New(k8s.Config{Context: "x", Namespace: "ampqa-cp-1"}).WithRunner(
		func(context.Context, io.Reader, []string) (execpkg.Result, error) {
			asked = true
			return execpkg.Result{}, errors.New(`Error from server (NotFound): namespaces "ampqa-cp-1" not found`)
		})
	if _, gone := p.SharedInfraGone(context.Background()); gone {
		t.Error("a stack that never deployed reported its namespace deleted")
	}
	if asked {
		t.Error("probed the cluster for a namespace that was never created")
	}
}
