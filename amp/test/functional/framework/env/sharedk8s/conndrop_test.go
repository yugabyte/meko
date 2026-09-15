package sharedk8s

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"net"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
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

const ownAddr = "127.0.0.1:1"

func attestEnv(rec *podRec, snap *pgPodSnap) *Env {
	e := &Env{
		endpointID: "ep-1",
		kube:       k8s.New(k8s.Config{Context: "x"}).WithRunner(rec.run),
		pgPod:      snap,
		pfs:        []*k8s.PortForward{{}},
	}
	// Port 1 refuses, so pgForwardAccepts fails fast instead of dialling out.
	e.pg = pg.ConnInfo{Host: "127.0.0.1", Port: 1}
	return e
}

func TestAttestDeclinesWithoutSnapshot(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "0"}, nil)
	if _, ok := e.AttestPGConnDrop(context.Background(), ownAddr); ok {
		t.Fatal("attested with no baseline snapshot")
	}
}

func TestAttestDeclinesOnReplacedPod(t *testing.T) {
	e := attestEnv(&podRec{uid: "u2", restarts: "0"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	if _, ok := e.AttestPGConnDrop(context.Background(), ownAddr); ok {
		t.Fatal("attested although the pod uid changed")
	}
}

func TestAttestDeclinesOnContainerRestart(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "1"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	if _, ok := e.AttestPGConnDrop(context.Background(), ownAddr); ok {
		t.Fatal("attested although a container restarted")
	}
}

// An in-place postgres respawn moves pg_postmaster_start_time while pod uid
// and restart count stay put; it must decline.
func TestAttestDeclinesOnPostmasterRestart(t *testing.T) {
	e := attestEnv(&podRec{uid: "u1", restarts: "0"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t1", nil }
	if _, ok := e.AttestPGConnDrop(context.Background(), ownAddr); ok {
		t.Fatal("attested although the postmaster restarted in place")
	}
}

// The tenant endpoint gets a real loopback listener so pgForwardAccepts sees a
// live forward and the attestation does not re-resolve.
func tenantEnv(t *testing.T, rec *podRec, snap *pgPodSnap) (*Env, string) {
	t.Helper()
	e := attestEnv(rec, &pgPodSnap{name: "own", uid: rec.uid, restarts: 0, postmasterStart: "own"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ci := pg.ConnInfo{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	e.extraPG = map[string]*extraEndpoint{
		ci.Addr(): {ep: controller.Endpoint{ID: "ep-2"}, ci: ci, snap: snap},
	}
	e.pgReady = func(context.Context, pg.ConnInfo) error { return nil }
	return e, ci.Addr()
}

func TestAttestTenantEndpointOnItsOwnPod(t *testing.T) {
	e, addr := tenantEnv(t, &podRec{uid: "u9", restarts: "0"}, &pgPodSnap{name: "p2", uid: "u9", restarts: 0, postmasterStart: "t9"})
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t9", nil }
	reason, ok := e.AttestPGConnDrop(context.Background(), addr)
	if !ok {
		t.Fatal("declined a tenant endpoint whose pod and postmaster were unchanged")
	}
	if !strings.Contains(reason, "yb-ep-ep-1-pod0") {
		t.Fatalf("attestation must name the pod it checked; got %q", reason)
	}
	if !strings.Contains(reason, "harness port-forward drop, not a product failure") {
		t.Fatalf("attestation lost the phrase core/envfail.go matches; got %q", reason)
	}
}

func TestAttestDeclinesForAnUnknownAddr(t *testing.T) {
	e, _ := tenantEnv(t, &podRec{uid: "u9", restarts: "0"}, &pgPodSnap{name: "p2", uid: "u9", restarts: 0, postmasterStart: "t9"})
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t9", nil }
	if _, ok := e.AttestPGConnDrop(context.Background(), "127.0.0.9:9999"); ok {
		t.Fatal("attested a connection this env never opened")
	}
}

func TestAttestDeclinesForATenantEndpointWithNoSnapshot(t *testing.T) {
	e, addr := tenantEnv(t, &podRec{uid: "u9", restarts: "0"}, nil)
	if _, ok := e.AttestPGConnDrop(context.Background(), addr); ok {
		t.Fatal("attested a tenant endpoint with no baseline snapshot")
	}
}

func TestAttestDeclinesOnReplacedTenantPod(t *testing.T) {
	e, addr := tenantEnv(t, &podRec{uid: "u10", restarts: "0"}, &pgPodSnap{name: "p2", uid: "u9", restarts: 0, postmasterStart: "t9"})
	if _, ok := e.AttestPGConnDrop(context.Background(), addr); ok {
		t.Fatal("attested although the tenant pod uid changed")
	}
}

func TestAttestDeclinesOnTenantPostmasterRestart(t *testing.T) {
	e, addr := tenantEnv(t, &podRec{uid: "u9", restarts: "0"}, &pgPodSnap{name: "p2", uid: "u9", restarts: 0, postmasterStart: "t9"})
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t10", nil }
	if _, ok := e.AttestPGConnDrop(context.Background(), addr); ok {
		t.Fatal("attested although the tenant postmaster restarted in place")
	}
}

func TestAttestOwnEndpointStillAttests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	e := attestEnv(&podRec{uid: "u1", restarts: "0"}, &pgPodSnap{name: "p", uid: "u1", restarts: 0, postmasterStart: "t0"})
	e.pg = pg.ConnInfo{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	e.pgProbe = func(context.Context, pg.ConnInfo) (string, error) { return "t0", nil }
	e.pgReady = func(context.Context, pg.ConnInfo) error { return nil }
	if _, ok := e.AttestPGConnDrop(context.Background(), e.pg.Addr()); !ok {
		t.Fatal("declined the env's own endpoint although nothing changed")
	}
}
