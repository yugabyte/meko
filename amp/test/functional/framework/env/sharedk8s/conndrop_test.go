package sharedk8s

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// podRec answers the pod name/phase selectors and the uid/phase/restarts get.
type podRec struct {
	mu       sync.Mutex
	uid      string
	restarts string
}

func (r *podRec) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	joined := strings.Join(argv, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case strings.Contains(joined, "metadata.uid"):
		return execpkg.Result{Stdout: r.uid + " Running " + r.restarts}, nil
	case strings.Contains(joined, "metadata.name"):
		return execpkg.Result{Stdout: "yb-ep-ep-1-pod0\n"}, nil
	case strings.Contains(joined, "status.phase"):
		return execpkg.Result{Stdout: "Running\n"}, nil
	default:
		return execpkg.Result{}, nil
	}
}

func attestEnv(rec *podRec, snap *pgPodSnap) *Env {
	return &Env{
		endpointID: "ep-1",
		kube:       k8s.New(k8s.Config{Context: "x"}).WithRunner(rec.run),
		pgPod:      snap,
		pfs:        []*k8s.PortForward{{}},
	}
}

func TestAttestDeclinesWithoutSnapshot(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "0"}, nil)
	if _, ok := e.AttestPGConnDrop(context.Background()); ok {
		t.Fatal("attested with no baseline snapshot")
	}
}

func TestAttestDeclinesOnReplacedPod(t *testing.T) {
	e := attestEnv(&podRec{uid: "u2", restarts: "0"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	if _, ok := e.AttestPGConnDrop(context.Background()); ok {
		t.Fatal("attested although the pod uid changed")
	}
}

func TestAttestDeclinesOnContainerRestart(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "1"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	if _, ok := e.AttestPGConnDrop(context.Background()); ok {
		t.Fatal("attested although a container restarted")
	}
}

// An in-place postgres respawn moves pg_postmaster_start_time while pod uid
// and restart count stay put; it must decline.
func TestAttestDeclinesOnPostmasterRestart(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "0"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	e.pg = pg.ConnInfo{Host: "127.0.0.1", Port: 1} // pgForwardAccepts fails fast
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t1", nil }
	if _, ok := e.AttestPGConnDrop(context.Background()); ok {
		t.Fatal("attested although the postmaster restarted in place")
	}
}
