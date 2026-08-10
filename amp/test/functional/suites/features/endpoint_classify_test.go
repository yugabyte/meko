package features

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

type stubTaskLister struct {
	ampCtl
	total int
	err   error
}

func (s stubTaskLister) ListTasks(context.Context, controller.ListTasksOptions) (controller.TaskList, error) {
	return controller.TaskList{Total: s.total}, s.err
}

// A doomed fork FAILS its endpoint task. A cluster with nowhere to put the pod
// leaves the task in flight and the wait times out — identical from the caller's
// side, so only a failed task counts as evidence about the product.
func TestOnlyAFailedTaskIsEvidenceAboutTheFork(t *testing.T) {
	for _, tc := range []struct {
		name string
		lst  stubTaskLister
		want bool
	}{
		{"a create_endpoint task failed", stubTaskLister{total: 1}, true},
		{"nothing failed — the pod was never placed", stubTaskLister{total: 0}, false},
		{"the controller could not be asked", stubTaskLister{err: errors.New("unreachable")}, false},
	} {
		if got := hasFailedEndpointTask(context.Background(), tc.lst, "cluster-1"); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An abandoned wait must not be reported as the product refusing to bring an
// endpoint up — the endpoint may well arrive minutes later.
func TestAbandonedEndpointWaitIsAnEnvFailure(t *testing.T) {
	boom := errors.New("create endpoint on branch b: waiting for task t: context deadline exceeded")

	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if got := classifyEndpointCreate(live, stubTaskLister{total: 0}, "c1", boom); core.IsEnvFailure(got) {
		t.Error("a failure with the context still live was blamed on the environment")
	}

	dead, cancel2 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel2()
	<-dead.Done()

	if got := classifyEndpointCreate(dead, stubTaskLister{total: 0}, "c1", boom); !core.IsEnvFailure(got) {
		t.Error("the wait ran out and no task failed, but it was still reported as a product failure")
	}
	if got := classifyEndpointCreate(dead, stubTaskLister{total: 1}, "c1", boom); core.IsEnvFailure(got) {
		t.Error("a create_endpoint task DID fail; that is a real finding and must not be downgraded to an env failure")
	}
	if got := classifyEndpointCreate(dead, stubTaskLister{total: 0}, "c1", boom); !errors.Is(got, boom) {
		t.Error("the original error was lost in classification")
	}
}
