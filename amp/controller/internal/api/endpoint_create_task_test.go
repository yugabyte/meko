package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/task"
)

// readEndpointCreateTask returns the one endpoint on the branch as List and as Get
// report it, so every assertion holds for both.
func readEndpointCreateTask(t *testing.T, client *api.ClientWithResponses, poolID, clusterID, branchID uuid.UUID) []api.Endpoint {
	t.Helper()
	ctx := context.Background()
	list, err := client.ListEndpointsWithResponse(ctx, poolID, clusterID, branchID, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("list endpoints: err=%v body=%s", err, list.Body)
	}
	ep := list.JSON200.Endpoints[0]
	got, err := client.GetEndpointWithResponse(ctx, poolID, clusterID, branchID, ep.Id)
	if err != nil || got.JSON200 == nil {
		t.Fatalf("get endpoint: err=%v body=%s", err, got.Body)
	}
	return []api.Endpoint{ep, *got.JSON200}
}

// The endpoint row is written before the deploy and kept when it fails, so an
// endpoint with no address is either still coming up or already failed. create_task
// is what tells them apart (CLOUDGA-37682), for a writer a create_cluster made as
// much as for a standalone create.
func TestEndpointCreateTaskReportsAFailedCreate(t *testing.T) {
	const cause = "simulated deploy failure: 0/6 nodes are available: 3 Insufficient ephemeral-storage"

	t.Run("writer of a create_cluster", func(t *testing.T) {
		prov := api.Provisioning{Deployer: &fakeDeployer{createErr: errors.New(cause)}}
		client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
		ctx := context.Background()
		user := mustCreateUser(t, client, false)
		pool := mustCreatePool(t, client, user.Id, "ct-pool")
		withWriter := true
		cr, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
			Name: "ct", WithWriter: &withWriter, Password: strptr(testPassword),
		})
		if err != nil || cr.StatusCode() != http.StatusAccepted {
			t.Fatalf("create cluster: %v (%d)", err, cr.StatusCode())
		}
		drainTasks(t, store, backend, prov)
		clusterID := *cr.JSON202.ClusterId
		cl, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
		if err != nil || cl.JSON200 == nil {
			t.Fatalf("get cluster: %v %s", err, cl.Body)
		}

		for _, ep := range readEndpointCreateTask(t, client, pool.Id, clusterID, cl.JSON200.DefaultBranch.BranchId) {
			if ep.Address != "" {
				t.Errorf("address = %q after a failed deploy, want none", ep.Address)
			}
			ct := ep.CreateTask
			if ct == nil {
				t.Fatal("create_task absent")
			}
			if ct.Id != cr.JSON202.Id || ct.Kind != "create_cluster" || ct.Status != api.TaskStatusFailed {
				t.Errorf("create_task = %s %s %s, want task %s create_cluster failed", ct.Id, ct.Kind, ct.Status, cr.JSON202.Id)
			}
			if ct.Error == nil || !strings.Contains(*ct.Error, cause) {
				t.Errorf("create_task.error = %v, want it to carry %q", ct.Error, cause)
			}
		}
	})

	t.Run("standalone create_endpoint", func(t *testing.T) {
		prov := api.Provisioning{Deployer: &fakeDeployer{createErr: errors.New(cause)}}
		client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
		poolID, clusterID, branchID := seededBranch(t, client, backend)
		created := mustCreateEndpoint(t, client, poolID, clusterID, branchID)
		drainTasks(t, store, backend, prov)

		for _, ep := range readEndpointCreateTask(t, client, poolID, clusterID, branchID) {
			ct := ep.CreateTask
			if ct == nil || ct.Id != created.Id || ct.Kind != "create_endpoint" || ct.Status != api.TaskStatusFailed {
				t.Fatalf("create_task = %+v, want task %s create_endpoint failed", ct, created.Id)
			}
			if ct.Error == nil || !strings.Contains(*ct.Error, cause) {
				t.Errorf("create_task.error = %v, want it to carry %q", ct.Error, cause)
			}
		}
	})
}

func TestEndpointCreateTaskReportsASucceededCreate(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	poolID, clusterID, branchID := seededBranch(t, client, backend)
	created := mustCreateEndpoint(t, client, poolID, clusterID, branchID)
	drainTasks(t, store, backend, prov)

	for _, ep := range readEndpointCreateTask(t, client, poolID, clusterID, branchID) {
		if ep.Address == "" {
			t.Error("no address after a successful deploy")
		}
		ct := ep.CreateTask
		if ct == nil || ct.Id != created.Id || ct.Status != api.TaskStatusSucceeded || ct.Error != nil || ct.ErrorCode != nil {
			t.Errorf("create_task = %+v, want task %s succeeded with no error", ct, created.Id)
		}
	}
}

// failingCreateTasks is a MemStore whose create-task lookup fails, standing in
// for a task log the controller cannot read.
type failingCreateTasks struct{ *task.MemStore }

func (failingCreateTasks) EndpointCreateTasks(context.Context, []uuid.UUID) (map[uuid.UUID]*task.Task, error) {
	return nil, errors.New("task log unreachable")
}

// A lookup that fails costs only the field. Failing the request instead would
// hide every endpoint on the branch — and a field that is absent says only
// "unknown", which a caller must already handle for an endpoint that predates
// the task log.
func TestEndpointCreateTaskAbsentWhenUnreadable(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	backend := core.NewMemBackend()
	mem := task.NewMemStore()
	store := failingCreateTasks{mem}
	client := newTestServerOverStore(t, backend, store, mem, prov)
	poolID, clusterID, branchID := seededBranch(t, client, backend)
	mustCreateEndpoint(t, client, poolID, clusterID, branchID)
	drainTasks(t, mem, backend, prov)

	for _, ep := range readEndpointCreateTask(t, client, poolID, clusterID, branchID) {
		if ep.CreateTask != nil {
			t.Errorf("create_task = %+v, want absent when the task log cannot be read", ep.CreateTask)
		}
		if ep.Address == "" {
			t.Error("the endpoint itself must still be reported in full")
		}
	}
}

func seededBranch(t *testing.T, client *api.ClientWithResponses, backend *core.MemBackend) (poolID, clusterID, branchID uuid.UUID) {
	t.Helper()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "ct-pool")
	clusterID, branchID = uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	return pool.Id, clusterID, branchID
}

func mustCreateEndpoint(t *testing.T, client *api.ClientWithResponses, poolID, clusterID, branchID uuid.UUID) *api.Task {
	t.Helper()
	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(context.Background(), poolID, clusterID, branchID,
		api.CreateEndpointRequest{Role: &rw, Password: strptr(testPassword)})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("create endpoint: %v (%d) %s", err, created.StatusCode(), created.Body)
	}
	return created.JSON202
}

// newTestServerOverStore is newTestServerBackend over a caller's task.Store, for a
// test that has to make one store method fail; mem is the store underneath it,
// which drainTasks runs the worker on.
func newTestServerOverStore(t *testing.T, backend *core.MemBackend, store task.Store, mem *task.MemStore, prov api.Provisioning) *api.ClientWithResponses {
	t.Helper()
	cfg := &config.Config{ListenAddr: ":0"}
	cfg.Deploy.Enabled = prov.Deployer != nil
	srv := api.NewServer(cfg, backend, task.NewService(store, mem.Subtasks()), prov, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := api.NewClientWithResponses(ts.URL + "/api/v1")
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client
}

// Every endpoint on a page is answered with its own task, whichever page it is
// on: the lookup is one query for the page, so a mix-up between rows would only
// show with several of them.
func TestEndpointCreateTaskIsPerEndpointAcrossPages(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	poolID, clusterID, branchID := seededBranch(t, client, backend)

	want := map[uuid.UUID]uuid.UUID{} // endpoint -> the task that created it
	created := []*api.Task{mustCreateEndpoint(t, client, poolID, clusterID, branchID)}
	ro := api.CreateEndpointRequestRoleReadOnly
	for i := 0; i < 3; i++ {
		r, err := client.CreateEndpointWithResponse(ctx, poolID, clusterID, branchID, api.CreateEndpointRequest{Role: &ro})
		if err != nil || r.StatusCode() != http.StatusAccepted {
			t.Fatalf("create read_only endpoint: %v (%d) %s", err, r.StatusCode(), r.Body)
		}
		created = append(created, r.JSON202)
	}
	drainTasks(t, store, backend, prov)
	for _, tk := range created {
		ref := tk.TargetRef
		if ref == nil {
			t.Fatalf("create_endpoint task %s has no target_ref", tk.Id)
		}
		want[uuid.MustParse(*ref)] = tk.Id
	}

	all, err := client.ListEndpointsWithResponse(ctx, poolID, clusterID, branchID, nil)
	if err != nil || all.JSON200 == nil || len(all.JSON200.Endpoints) != len(created) {
		t.Fatalf("list endpoints: %v %s", err, all.Body)
	}
	seen := map[uuid.UUID]bool{}
	for offset := 0; offset < len(created); offset++ {
		limit, off := 1, offset
		page, err := client.ListEndpointsWithResponse(ctx, poolID, clusterID, branchID,
			&api.ListEndpointsParams{Limit: &limit, Offset: &off})
		if err != nil || page.JSON200 == nil || len(page.JSON200.Endpoints) != 1 {
			t.Fatalf("page %d: %v %s", offset, err, page.Body)
		}
		ep := page.JSON200.Endpoints[0]
		seen[ep.Id] = true
		if ep.CreateTask == nil || ep.CreateTask.Id != want[ep.Id] {
			t.Errorf("page %d: endpoint %s create_task = %+v, want task %s", offset, ep.Id, ep.CreateTask, want[ep.Id])
		}
	}
	if len(seen) != len(created) {
		t.Errorf("pages covered %d endpoints, want %d", len(seen), len(created))
	}
}

// seedEndpointRecord writes an endpoint row with no address and no deploy, the
// state create_endpoint leaves between recording the row and deploying it.
func seedEndpointRecord(t *testing.T, backend *core.MemBackend, clusterID, branchID uuid.UUID, role string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := backend.EnsureEndpointRecord(context.Background(), id, clusterID, branchID, role, "", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("seed endpoint record: %v", err)
	}
	return id
}

// seedCreateTask records a task of kind in status whose create_endpoint step names
// endpointID, as the worker would have left it.
func seedCreateTask(store *task.MemStore, kind string, status task.Status, endpointID uuid.UUID, errMsg, code string) uuid.UUID {
	taskID, stepID := uuid.New(), uuid.New()
	tk := &task.Task{ID: taskID, Kind: kind, Status: status, StepIDs: []uuid.UUID{stepID}}
	if errMsg != "" {
		tk.Error = &errMsg
	}
	if code != "" {
		tk.ErrorCode = &code
	}
	store.Seed(tk, []*task.StepRecord{{
		ID: stepID, TaskID: taskID, Kind: task.StepCreateEndpoint, Status: status,
		Args: []byte(`{"endpoint_id":"` + endpointID.String() + `"}`),
	}})
	return taskID
}

// The states a listed endpoint's create can be in, and what the API says of each.
func TestEndpointCreateTaskStates(t *testing.T) {
	for name, tc := range map[string]struct {
		seed     bool // whether any task created it
		kind     string
		status   task.Status
		err      string
		code     string
		wantCode *string
	}{
		"predates the task log":           {},
		"create still running":            {seed: true, kind: "create_endpoint", status: task.StatusRunning},
		"create not yet claimed":          {seed: true, kind: "create_endpoint", status: task.StatusPending},
		"writer of a branch, failed":      {seed: true, kind: "branch", status: task.StatusFailed, err: "deploy endpoint: timed out"},
		"cancelled create carries code":   {seed: true, kind: "create_endpoint", status: task.StatusFailed, err: "cancelled", code: task.ErrorCodeCancelled},
		"conflicting create carries code": {seed: true, kind: "create_endpoint", status: task.StatusFailed, err: "conflict", code: task.ErrorCodeConflict},
	} {
		t.Run(name, func(t *testing.T) {
			prov := api.Provisioning{Deployer: &fakeDeployer{}}
			client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
			poolID, clusterID, branchID := seededBranch(t, client, backend)
			epID := seedEndpointRecord(t, backend, clusterID, branchID, "read_write")
			var taskID uuid.UUID
			if tc.seed {
				taskID = seedCreateTask(store, tc.kind, tc.status, epID, tc.err, tc.code)
			}

			for _, ep := range readEndpointCreateTask(t, client, poolID, clusterID, branchID) {
				if ep.Address != "" {
					t.Errorf("address = %q, want none", ep.Address)
				}
				ct := ep.CreateTask
				if !tc.seed {
					if ct != nil {
						t.Errorf("create_task = %+v, want absent", ct)
					}
					continue
				}
				if ct == nil || ct.Id != taskID || ct.Kind != tc.kind || string(ct.Status) != string(tc.status) {
					t.Fatalf("create_task = %+v, want %s %s %s", ct, taskID, tc.kind, tc.status)
				}
				if got := derefStr(ct.Error); got != tc.err {
					t.Errorf("error = %q, want %q", got, tc.err)
				}
				if got := derefStr(ct.ErrorCode); got != tc.code {
					t.Errorf("error_code = %q, want %q", got, tc.code)
				}
			}
		})
	}
}

// The wire shape a client decodes, read as raw JSON: the field names, the
// required ones always present, and error/error_code absent rather than null or
// empty when there is nothing to say.
func TestEndpointCreateTaskWireShape(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	backend := core.NewMemBackend()
	client, store, base := newTestServerRaw(t, backend, prov)
	poolID, clusterID, branchID := seededBranch(t, client, backend)
	ok := seedEndpointRecord(t, backend, clusterID, branchID, "read_only")
	bad := seedEndpointRecord(t, backend, clusterID, branchID, "read_only")
	okTask := seedCreateTask(store, "create_endpoint", task.StatusSucceeded, ok, "", "")
	badTask := seedCreateTask(store, "create_cluster", task.StatusFailed, bad, "deploy endpoint: boom", task.ErrorCodeConflict)

	get := func(id uuid.UUID) map[string]any {
		t.Helper()
		resp, err := http.Get(base + "/storage-pools/" + poolID.String() + "/clusters/" + clusterID.String() +
			"/branches/" + branchID.String() + "/endpoints/" + id.String())
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET endpoint: %v %v", err, resp)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ct, _ := body["create_task"].(map[string]any)
		if ct == nil {
			t.Fatalf("no create_task object in %v", body)
		}
		return ct
	}

	if ct := get(ok); ct["id"] != okTask.String() || ct["kind"] != "create_endpoint" || ct["status"] != "succeeded" {
		t.Errorf("succeeded create_task = %v", ct)
	} else if _, has := ct["error"]; has {
		t.Errorf("succeeded create_task carries an error key: %v", ct)
	} else if _, has := ct["error_code"]; has {
		t.Errorf("succeeded create_task carries an error_code key: %v", ct)
	}
	if ct := get(bad); ct["id"] != badTask.String() || ct["kind"] != "create_cluster" || ct["status"] != "failed" ||
		ct["error"] != "deploy endpoint: boom" || ct["error_code"] != "conflict" {
		t.Errorf("failed create_task = %v", ct)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
