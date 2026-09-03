package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

// These cover POST /endpoint-upgrades and GET /tasks/{taskId}/subtasks through
// the generated client: what the handler validates (the request, and only the
// request), and what a drained roll leaves behind.

// TestUpgradeEndpointsReturnsTask is the cheap contract check — 202 + a pending
// upgrade_endpoints task with a Location header, without driving the worker.
func TestUpgradeEndpointsReturnsTask(t *testing.T) {
	client, _, _ := newTestServer(t)

	resp, err := client.UpgradeEndpointsWithResponse(context.Background(), api.UpgradeEndpointsRequest{Version: "v2"})
	if err != nil {
		t.Fatalf("UpgradeEndpoints: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "upgrade_endpoints")
}

// TestUpgradeEndpointsRejectsABadRequest: the handler validates what is true of
// the REQUEST. A missing version and an out-of-range max_parallel are 400s; 0 is
// out of range rather than "unbounded", which would fire the whole fleet at once.
func TestUpgradeEndpointsRejectsABadRequest(t *testing.T) {
	client, _, store := newTestServer(t)
	ctx := context.Background()

	zero, tooMany, negative, ok := 0, 51, -1, 4
	for name, body := range map[string]api.UpgradeEndpointsRequest{
		"no version":       {Version: ""},
		"blank version":    {Version: "   "},
		"max_parallel 0":   {Version: "v2", MaxParallel: &zero},
		"max_parallel 51":  {Version: "v2", MaxParallel: &tooMany},
		"max_parallel -1":  {Version: "v2", MaxParallel: &negative},
		"blank + in range": {Version: "", MaxParallel: &ok},
	} {
		resp, err := client.UpgradeEndpointsWithResponse(ctx, body)
		if err != nil {
			t.Fatalf("%s: UpgradeEndpoints: %v", name, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", name, resp.StatusCode(), resp.Body)
		}
	}
	// Nothing was planned: a rejected request must not leave a task behind.
	tasks, total, err := store.List(ctx, task.TaskFilter{}, 100, 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if total != 0 {
		t.Errorf("a rejected upgrade planned %d tasks: %+v", total, tasks)
	}
}

// TestUpgradeEndpointsDeployDisabled: with no provisioner the roll refuses with
// 501 rather than planning a task no worker can run — like create/resize/retire.
func TestUpgradeEndpointsDeployDisabled(t *testing.T) {
	client, _, _ := newTestServerDeploy(t, false)

	resp, err := client.UpgradeEndpointsWithResponse(context.Background(), api.UpgradeEndpointsRequest{Version: "v2"})
	if err != nil {
		t.Fatalf("UpgradeEndpoints: %v", err)
	}
	if resp.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", resp.StatusCode(), resp.Body)
	}
}

// TestUpgradeEndpointsRoll drives a whole-fleet roll through the worker and
// asserts what an operator actually reads back: the endpoint's recorded version,
// the per-endpoint result on the parent, and the child row behind each line.
func TestUpgradeEndpointsRoll(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "roll-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	rw := api.CreateEndpointRequestRoleReadWrite
	ro := api.CreateEndpointRequestRoleReadOnly
	for _, role := range []api.CreateEndpointRequestRole{rw, ro} {
		if _, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID,
			api.CreateEndpointRequest{Role: &role}); err != nil {
			t.Fatalf("CreateEndpoint(%s): %v", role, err)
		}
	}
	drainTasks(t, store, backend, prov)

	rolled, err := client.UpgradeEndpointsWithResponse(ctx, api.UpgradeEndpointsRequest{Version: "ep-v9.9.9"})
	if err != nil {
		t.Fatalf("UpgradeEndpoints: %v", err)
	}
	if rolled.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rolled.StatusCode(), rolled.Body)
	}
	drainTasks(t, store, backend, prov)

	taskResp, _ := client.GetTaskWithResponse(ctx, rolled.JSON202.Id)
	if taskResp.JSON200 == nil {
		t.Fatalf("GetTask: body = %s", taskResp.Body)
	}
	if taskResp.JSON200.Status != api.TaskStatusSucceeded {
		t.Fatalf("roll status = %q (error %v), want succeeded", taskResp.JSON200.Status, taskResp.JSON200.Error)
	}
	res, ok := taskResp.JSON200.Result.(map[string]any)
	if !ok {
		t.Fatalf("roll result = %v, want the per-endpoint report", taskResp.JSON200.Result)
	}
	if res["version"] != "ep-v9.9.9" || res["upgraded"] != float64(2) {
		t.Errorf("roll result = %v, want version ep-v9.9.9 and 2 upgraded", res)
	}
	lines, ok := res["endpoints"].([]any)
	if !ok || len(lines) != 2 {
		t.Fatalf("roll result endpoints = %v, want one line per endpoint", res["endpoints"])
	}
	for _, l := range lines {
		line := l.(map[string]any)
		if line["outcome"] != "upgraded" || line["subtask_id"] == "" {
			t.Errorf("report line = %v, want upgraded with the subtask to go and read", line)
		}
	}

	// Every endpoint now reports the version it was rolled to.
	list, _ := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if list.JSON200 == nil || len(list.JSON200.Endpoints) != 2 {
		t.Fatalf("ListEndpoints: body = %s", list.Body)
	}
	for _, ep := range list.JSON200.Endpoints {
		if ep.Version == nil || *ep.Version != "ep-v9.9.9" {
			t.Errorf("endpoint %s version = %v, want ep-v9.9.9", ep.Id, ep.Version)
		}
	}

	// The children: one per endpoint, terminal, and NOT in GET /tasks — a roll of
	// N endpoints adds one task row, not N.
	subs, err := client.ListTaskSubtasksWithResponse(ctx, rolled.JSON202.Id)
	if err != nil {
		t.Fatalf("ListTaskSubtasks: %v", err)
	}
	if subs.JSON200 == nil || subs.JSON200.Count != 2 || len(subs.JSON200.Subtasks) != 2 {
		t.Fatalf("subtasks = %s, want two", subs.Body)
	}
	for _, s := range subs.JSON200.Subtasks {
		if s.TaskId != rolled.JSON202.Id {
			t.Errorf("subtask %s hangs off task %s, want %s", s.Id, s.TaskId, rolled.JSON202.Id)
		}
		if s.Kind != "upgrade_endpoint" {
			t.Errorf("subtask kind = %q, want the STEP kind upgrade_endpoint", s.Kind)
		}
		if s.Status != api.TaskStatusSucceeded {
			t.Errorf("subtask %s status = %q, want succeeded: a parent must not go terminal with a live child", s.Id, s.Status)
		}
		if s.Args == nil {
			t.Errorf("subtask %s carries no args, so nothing says which endpoint it moved", s.Id)
		}
	}
	all, _ := client.ListTasksWithResponse(ctx, nil)
	if all.JSON200 == nil {
		t.Fatalf("ListTasks: body = %s", all.Body)
	}
	for _, tk := range all.JSON200.Tasks {
		if tk.Kind == "upgrade_endpoint" {
			t.Errorf("a subtask surfaced in GET /tasks as %s; children are not tasks", tk.Id)
		}
	}

	// Re-rolling to the same version converges on a no-op: every endpoint is now
	// recorded at the target, so the whole-fleet filter skips them all.
	again, err := client.UpgradeEndpointsWithResponse(ctx, api.UpgradeEndpointsRequest{Version: "ep-v9.9.9"})
	if err != nil {
		t.Fatalf("UpgradeEndpoints (again): %v", err)
	}
	drainTasks(t, store, backend, prov)
	repeat, _ := client.GetTaskWithResponse(ctx, again.JSON202.Id)
	if repeat.JSON200 == nil || repeat.JSON200.Result == nil {
		t.Fatalf("second roll has no result; body = %s", repeat.Body)
	}
	if r := repeat.JSON200.Result.(map[string]any); r["skipped"] != float64(2) || r["upgraded"] != float64(0) {
		t.Errorf("second roll = %v, want everything skipped", r)
	}
}

// TestListTaskSubtasksUnknownTask: an absent task is a 404, not an empty list —
// otherwise "no such task" and "a task with no children" read the same.
func TestListTaskSubtasksUnknownTask(t *testing.T) {
	client, _, _ := newTestServer(t)

	resp, err := client.ListTaskSubtasksWithResponse(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("ListTaskSubtasks: %v", err)
	}
	if resp.StatusCode() != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", resp.StatusCode(), resp.Body)
	}
}

// TestListTaskSubtasksOnAPlainTask: a task that fanned out to nothing has an
// empty list, which must marshal as [] rather than null.
func TestListTaskSubtasksOnAPlainTask(t *testing.T) {
	client, _, _ := newTestServer(t)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")

	created, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "shop"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	resp, err := client.ListTaskSubtasksWithResponse(ctx, created.JSON202.Id)
	if err != nil {
		t.Fatalf("ListTaskSubtasks: %v", err)
	}
	if resp.JSON200 == nil || resp.JSON200.Count != 0 || resp.JSON200.Subtasks == nil {
		t.Fatalf("subtasks = %s, want an empty array", resp.Body)
	}
}
