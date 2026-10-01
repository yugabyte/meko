package api_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/task"
)

// A cluster's tags label every endpoint it has, an endpoint's own tags win on a
// key both set, and each resource reports only the tags it was created with.
func TestTagsLabelEveryEndpointOfTheCluster(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "tags-pool")

	withWriter := true
	clusterTags := api.Tags{"team": "payments", "cost-center": "42"}
	cp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "orders", WithWriter: &withWriter, Password: strptr(testPassword), Tags: &clusterTags,
	})
	if err != nil || cp.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateCluster: status = %d, err = %v; body = %s", cp.StatusCode(), err, cp.Body)
	}
	drainTasks(t, store, backend, prov)
	if got := soleTask(t, store); got.Status != task.StatusSucceeded {
		t.Fatalf("create_cluster task = %q, error = %v", got.Status, got.Error)
	}

	list, err := client.ListClustersWithResponse(ctx, pool.Id, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Clusters) != 1 {
		t.Fatalf("ListClusters: err = %v; body = %s", err, list.Body)
	}
	cluster := list.JSON200.Clusters[0]
	if !reflect.DeepEqual(cluster.Tags, clusterTags) {
		t.Errorf("cluster tags = %v, want %v", cluster.Tags, clusterTags)
	}

	// The writer with_writer created has no tags of its own and inherits the
	// cluster's.
	if len(dep.created) != 1 {
		t.Fatalf("deployer creates = %d, want 1 (the writer)", len(dep.created))
	}
	if want := map[string]string(clusterTags); !reflect.DeepEqual(dep.created[0].Labels, want) {
		t.Errorf("writer labels = %v, want the cluster's %v", dep.created[0].Labels, want)
	}
	// Every endpoint also says which endpoint, branch, cluster and pool it is,
	// and its role — on the very first install, not only after a redeploy.
	wantIdentity := map[string]string{
		deploy.LabelEndpointID:    dep.created[0].EndpointID,
		deploy.LabelBranchID:      cluster.DefaultBranch.BranchId.String(),
		deploy.LabelClusterID:     cluster.Id.String(),
		deploy.LabelStoragePoolID: pool.Id.String(),
		deploy.LabelEndpointType:  "read_write",
	}
	if !reflect.DeepEqual(dep.created[0].Identity, wantIdentity) {
		t.Errorf("writer identity labels = %v, want %v", dep.created[0].Identity, wantIdentity)
	}
	writer := mustOneEndpoint(t, client, pool.Id, cluster.Id, cluster.DefaultBranch.BranchId)
	if len(writer.Tags) != 0 {
		t.Errorf("writer tags = %v, want none of its own", writer.Tags)
	}

	ro := api.CreateEndpointRequestRoleReadOnly
	epTags := api.Tags{"team": "analytics", "acme.com/owner": "jdoe"}
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, cluster.Id, cluster.DefaultBranch.BranchId,
		api.CreateEndpointRequest{Role: &ro, Tags: &epTags})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint: status = %d, err = %v; body = %s", created.StatusCode(), err, created.Body)
	}
	drainTasks(t, store, backend, prov)
	if len(dep.created) != 2 {
		t.Fatalf("deployer creates = %d, want 2", len(dep.created))
	}
	want := map[string]string{"team": "analytics", "cost-center": "42", "acme.com/owner": "jdoe"}
	if got := dep.created[1].Labels; !reflect.DeepEqual(got, want) {
		t.Errorf("reader labels = %v, want %v (cluster's under its own)", got, want)
	}

	reader, err := client.GetEndpointWithResponse(ctx, pool.Id, cluster.Id, cluster.DefaultBranch.BranchId, readerID(t, dep))
	if err != nil || reader.JSON200 == nil {
		t.Fatalf("GetEndpoint: err = %v; body = %s", err, reader.Body)
	}
	if !reflect.DeepEqual(reader.JSON200.Tags, epTags) {
		t.Errorf("reader tags = %v, want only its own %v", reader.JSON200.Tags, epTags)
	}
}

// readerID is the second deployed endpoint's id.
func readerID(t *testing.T, dep *fakeDeployer) api.EndpointId {
	t.Helper()
	id, err := uuid.Parse(dep.created[1].EndpointID)
	if err != nil {
		t.Fatalf("deployed endpoint id %q: %v", dep.created[1].EndpointID, err)
	}
	return id
}

// A tag the API server would refuse as a label is refused at the API, before
// anything is planned.
func TestInvalidTagsAreRefused(t *testing.T) {
	client, _, store := newTestServer(t)
	ctx := context.Background()
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "bad-tags-pool")

	for _, tags := range []api.Tags{
		{"app.kubernetes.io/instance": "other"},
		{"bad key": "v"},
		{"team": "has space"},
	} {
		resp, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "c", Tags: &tags})
		if err != nil {
			t.Fatalf("CreateCluster(%v): %v", tags, err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("CreateCluster(%v) = %d, want 400; body = %s", tags, resp.StatusCode(), resp.Body)
		}
	}
	if _, total, _ := store.List(ctx, task.TaskFilter{}, 1000, 0); total != 0 {
		t.Errorf("tasks planned = %d, want none", total)
	}
}
