package sharedk8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// A dead forward must cost a re-forward and a retry, not the run. Six tests died
// at once in nightly 0.260810.0 because each held a client pinned to the one
// local port, and kubectl port-forward does not reconnect.
func TestARefusedForwardIsRevivedAndTheRequestRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("body reached the server as %q, want %q — a retry must replay it", body, "payload")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	live := strings.TrimPrefix(srv.URL, "http://")

	var calls atomic.Int32
	tr := &revivingTransport{host: func(force bool) (string, error) {
		// First resolution hands out a port nobody listens on, as a dead forward
		// does; the forced one hands out the live server.
		if calls.Add(1) == 1 {
			if force {
				t.Error("the first attempt should not have forced a re-forward")
			}
			return "127.0.0.1:1", nil
		}
		if !force {
			t.Error("the retry must force a re-forward; kubectl port-forward does not reconnect")
		}
		return live, nil
	}}

	req, err := http.NewRequest(http.MethodPost, "http://pinned.invalid/api/v1/x", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("request failed instead of being revived: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("resolved the host %d times, want exactly 2 (one attempt, one revive)", got)
	}
}

// Twice refused is the environment, and must say so: the bare error reads as if
// the controller rejected the call.
func TestAForwardThatCannotComeBackIsAnEnvFailure(t *testing.T) {
	tr := &revivingTransport{host: func(bool) (string, error) { return "127.0.0.1:1", nil }}
	req, _ := http.NewRequest(http.MethodGet, "http://pinned.invalid/api/v1/tasks/x", nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !core.IsEnvFailure(err) {
		t.Errorf("err = %v; a forward that cannot be re-established is an environment failure, not a product one", err)
	}
}

// A resolution failure is also the environment, not the product.
func TestAnUnresolvableForwardIsAnEnvFailure(t *testing.T) {
	tr := &revivingTransport{host: func(bool) (string, error) { return "", errors.New("no kube context") }}
	req, _ := http.NewRequest(http.MethodGet, "http://pinned.invalid/x", nil)
	if _, err := tr.RoundTrip(req); !core.IsEnvFailure(err) {
		t.Errorf("err = %v, want an env failure", err)
	}
}

// A successful call must not re-resolve or retry: the revival path is for
// failures only.
func TestAHealthyForwardResolvesOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var calls atomic.Int32
	tr := &revivingTransport{host: func(bool) (string, error) {
		calls.Add(1)
		return strings.TrimPrefix(srv.URL, "http://"), nil
	}}
	req, _ := http.NewRequest(http.MethodGet, "http://pinned.invalid/readyz", nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Errorf("resolved %d times on a healthy forward, want 1", got)
	}
}

// trackingStarter hands out live fake forwards and remembers each process, so a
// test can assert they were killed rather than left running on the agent.
type trackingStarter struct {
	mu    sync.Mutex
	procs []*livePFProc
}

func (s *trackingStarter) start(_ context.Context, _ []string) (k8s.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := newLivePFProc(fmt.Sprintf("Forwarding from 127.0.0.1:%d -> 8080\n", 41000+len(s.procs)))
	s.procs = append(s.procs, p)
	return p, nil
}

func (s *trackingStarter) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs)
}

func (s *trackingStarter) alive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.procs {
		select {
		case <-p.killed:
		default:
			n++
		}
	}
	return n
}

// Each environment forwards for itself, and gives the process back when it is
// torn down. A per-run forward means one test's revival closes the port its
// neighbours are mid-request on; a forward that outlives its Env means a kubectl
// process accumulating on the agent for the length of the run.
func TestEachEnvOwnsAndReleasesItsControllerForward(t *testing.T) {
	st := &trackingStarter{}
	rec := &execRec{}
	p := New(Config{AmpStack: ampstack.Config{Namespace: "amp", Context: "x"}})
	p.logf = nil
	p.kube = k8s.New(k8s.Config{Context: "x", Namespace: "amp"}).WithRunner(rec.run).WithStarter(st.start)

	a, b := &Env{prov: p, ownCtrl: p.ownControllerConn()}, &Env{prov: p, ownCtrl: p.ownControllerConn()}
	if a.ownCtrl == nil || b.ownCtrl == nil {
		t.Fatal("kubectl-reached control plane must give each Env its own connection")
	}
	if a.ownCtrl == b.ownCtrl {
		t.Fatal("two Envs share one connection; a revival in either would disturb the other")
	}

	// Lazy: nothing is forwarded until an Env actually asks.
	if st.started() != 0 {
		t.Errorf("%d forwards opened before any Env used the controller, want 0", st.started())
	}

	for _, e := range []*Env{a, b} {
		if _, err := e.AmpController(context.Background()); err != nil {
			t.Fatalf("AmpController: %v", err)
		}
	}
	if got := st.started(); got != 2 {
		t.Errorf("opened %d forwards for 2 Envs, want 2 (one each)", got)
	}
	if got := st.alive(); got != 2 {
		t.Errorf("%d of 2 forwards alive after use, want 2", got)
	}

	// Tearing one down must release only its own process.
	a.ownCtrl.reset()
	if got := st.alive(); got != 1 {
		t.Errorf("%d forwards alive after tearing down one Env, want 1 — the other Env's forward must survive", got)
	}
	b.ownCtrl.reset()
	if got := st.alive(); got != 0 {
		t.Errorf("%d forwards still running after both Envs were torn down: kubectl processes are leaking onto the agent", got)
	}
}

// A direct controller URL has no forward to own, so no per-Env process is spawned.
func TestADirectControllerURLOwnsNoForward(t *testing.T) {
	p := New(Config{ControllerURL: "http://amp.example:8080"})
	if p.ownControllerConn() != nil {
		t.Error("a direct URL needs no port-forward; spawning one per Env would be pure waste")
	}
}
