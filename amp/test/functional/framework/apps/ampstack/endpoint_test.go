package ampstack

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// waitRec is a scripted kubectl Runner for the readiness waits: it records every
// invocation and fails the ones matching notFound with a kubectl-shaped error.
type waitRec struct {
	calls    []string
	notFound string
}

func (r *waitRec) run(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
	joined := strings.Join(argv, " ")
	r.calls = append(r.calls, joined)
	if r.notFound != "" && strings.Contains(joined, r.notFound) {
		return exec.Result{ExitCode: 1}, fmt.Errorf(`error: deployments.apps "yb-ep-ep-1" not found`)
	}
	return exec.Result{}, nil
}

// TestWaitEndpointWorkloadReadyDeployment: with a Deployment present the wait is
// the single historical `kubectl wait deployment/... --for=condition=Available`
// — no probe, no ReplicaSet call.
func TestWaitEndpointWorkloadReadyDeployment(t *testing.T) {
	rec := &waitRec{}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	if err := WaitEndpointWorkloadReady(context.Background(), kube, "ep-1", time.Minute); err != nil {
		t.Fatalf("WaitEndpointWorkloadReady: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("want exactly one kubectl call for a Deployment-backed endpoint, got %v", rec.calls)
	}
	if !strings.Contains(rec.calls[0], "wait deployment/yb-ep-ep-1 --for=condition=Available") {
		t.Errorf("want the historical Deployment wait, got %q", rec.calls[0])
	}
}

// TestWaitEndpointWorkloadReadyReplicaSetFallback: when no Deployment exists the
// wait falls back to the bare ReplicaSet, whose readiness gate is readyReplicas
// (a ReplicaSet has no Available condition).
func TestWaitEndpointWorkloadReadyReplicaSetFallback(t *testing.T) {
	rec := &waitRec{notFound: "deployment/"}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	if err := WaitEndpointWorkloadReady(context.Background(), kube, "ep-1", time.Minute); err != nil {
		t.Fatalf("WaitEndpointWorkloadReady: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("want a Deployment attempt then a ReplicaSet wait, got %v", rec.calls)
	}
	if !strings.Contains(rec.calls[1], "wait replicaset/yb-ep-ep-1 --for=jsonpath={.status.readyReplicas}=1") {
		t.Errorf("want the ReplicaSet readyReplicas wait, got %q", rec.calls[1])
	}
}

// TestWaitEndpointWorkloadReadyRealFailureNotSwallowed: a Deployment that exists
// but never becomes Available must surface as-is — the fallback is only for a
// missing Deployment, never a way to turn a never-ready compute into a pass.
func TestWaitEndpointWorkloadReadyRealFailureNotSwallowed(t *testing.T) {
	rec := &waitRec{}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(
		func(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
			rec.calls = append(rec.calls, strings.Join(argv, " "))
			return exec.Result{ExitCode: 1}, fmt.Errorf("timed out waiting for the condition")
		})

	err := WaitEndpointWorkloadReady(context.Background(), kube, "ep-1", time.Minute)
	if err == nil {
		t.Fatal("a Deployment that never became Available must fail the wait")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("a timeout must not trigger the ReplicaSet fallback, got %v", rec.calls)
	}
}

type podRec struct {
	calls   []string
	running string
	first   string
}

func (r *podRec) run(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
	joined := strings.Join(argv, " ")
	r.calls = append(r.calls, joined)
	if strings.Contains(joined, `status.phase=="Running"`) {
		return exec.Result{Stdout: r.running}, nil
	}
	return exec.Result{Stdout: r.first}, nil
}

func TestEndpointPodNamePrefersRunning(t *testing.T) {
	rec := &podRec{running: "yb-ep-ep-1-live", first: "yb-ep-ep-1-dying"}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	pod, err := EndpointPodName(context.Background(), kube, "ep-1")
	if err != nil {
		t.Fatalf("EndpointPodName: %v", err)
	}
	if pod != "yb-ep-ep-1-live" {
		t.Errorf("want the Running pod, got %q", pod)
	}
	if len(rec.calls) != 1 {
		t.Errorf("want one kubectl call when a Running pod exists, got %v", rec.calls)
	}
}

func TestEndpointPodNameFallsBackWhileRescheduling(t *testing.T) {
	rec := &podRec{first: "yb-ep-ep-1-pending"}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	pod, err := EndpointPodName(context.Background(), kube, "ep-1")
	if err != nil {
		t.Fatalf("EndpointPodName: %v", err)
	}
	if pod != "yb-ep-ep-1-pending" {
		t.Errorf("want the first listed pod, got %q", pod)
	}

	none := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner((&podRec{}).run)
	if _, err := EndpointPodName(context.Background(), none, "ep-1"); err == nil {
		t.Error("want an error when the selector matches no pod")
	}
}

type authRec struct {
	calls    []string
	psqlExit []int
}

func (r *authRec) run(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
	joined := strings.Join(argv, " ")
	r.calls = append(r.calls, joined)
	if strings.Contains(joined, "get pod") {
		return exec.Result{Stdout: "yb-ep-ep-1-pod0"}, nil
	}
	code := 0
	if len(r.psqlExit) > 0 {
		code = r.psqlExit[0]
		r.psqlExit = r.psqlExit[1:]
	}
	if code != 0 {
		return exec.Result{ExitCode: code, Stderr: `FATAL:  password authentication failed for user "cloud_admin"`}, nil
	}
	return exec.Result{Stdout: "1\n"}, nil
}

func fastAuthPoll(t *testing.T) {
	t.Helper()
	prev := endpointAuthPoll
	endpointAuthPoll = time.Millisecond
	t.Cleanup(func() { endpointAuthPoll = prev })
}

func testAuth() EndpointAuth {
	return EndpointAuth{Host: "yb-ep-ep-1.amp.svc.cluster.local", Port: 55433, User: "cloud_admin", Password: "cloud_admin", Database: "postgres"}
}

func TestWaitEndpointAuthProbesRemotelyNotLoopback(t *testing.T) {
	rec := &authRec{}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	if err := WaitEndpointAuth(context.Background(), kube, "ep-1", testAuth(), time.Minute, nil); err != nil {
		t.Fatalf("WaitEndpointAuth: %v", err)
	}
	probe := rec.calls[len(rec.calls)-1]
	if strings.Contains(probe, "127.0.0.1") || !strings.Contains(probe, "-h yb-ep-ep-1.amp.svc.cluster.local") {
		t.Errorf("probe must dial the Service name, got %q", probe)
	}
	if !strings.Contains(probe, "PGPASSWORD='cloud_admin'") {
		t.Errorf("probe must present the password, got %q", probe)
	}
	if !strings.Contains(probe, "-c "+endpointContainer) {
		t.Errorf("probe must run in the %q container, got %q", endpointContainer, probe)
	}
}

func TestWaitEndpointAuthRetriesUntilAuthenticated(t *testing.T) {
	fastAuthPoll(t)
	rec := &authRec{psqlExit: []int{2}}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	if err := WaitEndpointAuth(context.Background(), kube, "ep-1", testAuth(), time.Minute, nil); err != nil {
		t.Fatalf("WaitEndpointAuth after one refusal: %v", err)
	}
	if got := len(rec.calls); got != 4 {
		t.Fatalf("want a refused attempt then a successful one (4 calls), got %d: %v", got, rec.calls)
	}
}

func TestWaitEndpointAuthTimeoutCarriesTheRefusal(t *testing.T) {
	rec := &authRec{psqlExit: []int{2}}
	kube := k8s.New(k8s.Config{Namespace: "amp"}).WithRunner(rec.run)

	err := WaitEndpointAuth(context.Background(), kube, "ep-1", testAuth(), time.Nanosecond, nil)
	if err == nil {
		t.Fatal("want a failure when the compute never authenticates")
	}
	if !strings.Contains(err.Error(), "ep-1") || !strings.Contains(err.Error(), "password authentication failed") {
		t.Errorf("error must name the endpoint and what it refused, got %v", err)
	}
}
