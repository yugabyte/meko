package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
)

// A second read_write endpoint is refused inside its task, not by the handler, so
// GET /tasks/{id} is the only place a caller learns why — the YCP agent among
// them, which holds the declaration on error_code=conflict (CLOUDGA-37405). This
// drives request -> task -> worker -> task row -> response, because the seams are
// where the code and the message could each be lost.
func TestRefusedEndpointCreateReportsItsErrorCode(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	u := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, u.Id, "pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})

	create := func(role api.CreateEndpointRequestRole, name string) api.Task {
		t.Helper()
		resp, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID,
			api.CreateEndpointRequest{Role: &role, Name: &name, Password: strptr(testPassword)})
		if err != nil || resp.StatusCode() != http.StatusAccepted || resp.JSON202 == nil {
			t.Fatalf("create %s %q: err=%v status=%d body=%s", role, name, err, resp.StatusCode(), resp.Body)
		}
		drainTasks(t, store, backend, prov)
		got, err := client.GetTaskWithResponse(ctx, resp.JSON202.Id)
		if err != nil || got.JSON200 == nil {
			t.Fatalf("get task: err=%v body=%s", err, got.Body)
		}
		return *got.JSON200
	}

	if first := create(api.CreateEndpointRequestRoleReadWrite, "rw"); first.Status != api.TaskStatusSucceeded || first.ErrorCode != nil {
		t.Fatalf("first writer: status=%s error_code=%v, want succeeded with no code", first.Status, first.ErrorCode)
	}

	// A duplicate name is refused by the handler (409); the writer slot only
	// inside the task.
	got := create(api.CreateEndpointRequestRoleReadWrite, "rw2")
	if got.Status != api.TaskStatusFailed {
		t.Fatalf("second writer: status = %s, want failed", got.Status)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "conflict" {
		t.Errorf("second writer: error_code = %v, want conflict", got.ErrorCode)
	}
	if msg := strptrValue(got.Error); !strings.Contains(msg, "already has a read_write endpoint") {
		t.Errorf("second writer: error = %q, want it to say the branch already has a writer", msg)
	}
}

func strptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
