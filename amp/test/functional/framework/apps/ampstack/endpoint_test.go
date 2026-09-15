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
