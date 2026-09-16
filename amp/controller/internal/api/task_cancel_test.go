package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
)

// These cover POST /tasks/{taskId}/cancel through the generated client: the three
// answers the route documents, and that the 202 is only a recorded request — the
// task is still running when it comes back.

func TestCancelTaskAcceptsAnInFlightTask(t *testing.T) {
	client, _, tasks := newTestServer(t)

	resp, err := client.UpgradeEndpointsWithResponse(context.Background(), api.UpgradeEndpointsRequest{Version: "v2"})
	if err != nil {
		t.Fatalf("UpgradeEndpoints: %v", err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	id := resp.JSON202.Id

	cancel, err := client.CancelTaskWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if cancel.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", cancel.StatusCode(), cancel.Body)
	}
	assertTaskAccepted(t, cancel.JSON202, cancel.HTTPResponse.Header.Get("Location"), "upgrade_endpoints")
	if cancel.JSON202.CancelRequestedAt == nil {
		t.Error("the 202 body carries no cancel_requested_at, so a client cannot tell the request landed")
	}
	// assertTaskAccepted's pending check is load-bearing here: the 202 records a
	// request, it does not move the status. The worker does that, at a step
	// boundary.
	stored, _ := tasks.Get(context.Background(), id)
	if !stored.CancelRequested() {
		t.Error("the task was not stamped in the store")
	}
}

func TestCancelTaskRejectsAnUnknownTask(t *testing.T) {
	client, _, _ := newTestServer(t)

	resp, err := client.CancelTaskWithResponse(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if resp.StatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode(), resp.Body)
	}
	if got := errorCode(t, resp.Body); got != "not_found" {
		t.Errorf("error code = %q, want not_found", got)
	}
}

func TestCancelTaskRejectsATerminalTask(t *testing.T) {
	client, backend, tasks := newTestServer(t)

	resp, err := client.UpgradeEndpointsWithResponse(context.Background(), api.UpgradeEndpointsRequest{Version: "v2"})
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("UpgradeEndpoints: err=%v body=%s", err, resp.Body)
	}
	drainTasks(t, tasks, backend, api.Provisioning{Deployer: &fakeDeployer{}})

	cancel, err := client.CancelTaskWithResponse(context.Background(), resp.JSON202.Id)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if cancel.StatusCode() != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", cancel.StatusCode(), cancel.Body)
	}
	if got := errorCode(t, cancel.Body); got != "conflict" {
		t.Errorf("error code = %q, want conflict", got)
	}
	stored, _ := tasks.Get(context.Background(), resp.JSON202.Id)
	if stored.CancelRequested() {
		t.Error("a refused cancel still stamped the finished task")
	}
}
