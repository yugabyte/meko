package api_test

import (
	"context"
	"net/http"
	"sort"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// An engine is a named bundle over the extension catalog. These drive the whole
// request -> task -> column -> response path, because the parts that could go
// wrong are all in the seams: the handler resolving the bundle, the task
// carrying it, and the cluster row giving it back.
func TestCreateClusterEngine(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "engine-pool")
	withWriter := true

	aidb := api.Aidb
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "brain", Engine: &aidb, WithWriter: &withWriter,
	})
	if err != nil {
		t.Fatalf("CreateCluster(aidb): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted || ok.JSON202 == nil {
		t.Fatalf("aidb status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	clusterID := *ok.JSON202.ClusterId
	drainTasks(t, store, backend, prov)

	// The engine's bundle reached the deployer as real mounts.
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1 (the writer)", len(dep.created))
	}
	var mounted []string
	for _, e := range dep.created[0].Extensions {
		mounted = append(mounted, e.Name)
	}
	sort.Strings(mounted)
	want := []string{"age", "pg_cron", "pgvector"}
	if len(mounted) != len(want) {
		t.Fatalf("writer mounts %v, want %v", mounted, want)
	}
	for i := range want {
		if mounted[i] != want[i] {
			t.Fatalf("writer mounts %v, want %v", mounted, want)
		}
	}

	// The engine round-trips on the cluster, and its bundle shows as the set.
	got, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.JSON200 == nil || got.JSON200.Engine == nil || *got.JSON200.Engine != "aidb" {
		t.Fatalf("GetCluster engine = %v, want aidb", got.JSON200.Engine)
	}
	if got.JSON200.Extensions == nil || len(*got.JSON200.Extensions) != 3 {
		t.Errorf("GetCluster extensions = %v, want the aidb bundle", got.JSON200.Extensions)
	}

	// listClusters reports it too — YCP reads the engine from here.
	list, err := client.ListClustersWithResponse(ctx, pool.Id, nil)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if list.JSON200 == nil || len(list.JSON200.Clusters) != 1 {
		t.Fatalf("ListClusters returned %v", list.JSON200)
	}
	if e := list.JSON200.Clusters[0].Engine; e == nil || *e != "aidb" {
		t.Errorf("ListClusters engine = %v, want aidb", e)
	}
}

// engine and extensions compose rather than one winning: the documented
// behaviour, and the reason the handler unions instead of assigning.
func TestCreateClusterEngineUnionsWithExtensions(t *testing.T) {
	dep := &fakeDeployer{}
	prov := api.Provisioning{Deployer: dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "engine-union-pool")

	oracle := api.Oracle
	extra := []string{"pgvector"}
	withWriter := true
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "both", Engine: &oracle, Extensions: &extra, WithWriter: &withWriter,
	})
	if err != nil {
		t.Fatalf("CreateCluster(oracle+pgvector): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
	drainTasks(t, store, backend, prov)

	var mounted []string
	for _, e := range dep.created[0].Extensions {
		mounted = append(mounted, e.Name)
	}
	sort.Strings(mounted)
	if len(mounted) != 2 || mounted[0] != "hexabridge" || mounted[1] != "pgvector" {
		t.Fatalf("writer mounts %v, want [hexabridge pgvector] — the engine's bundle unioned with the request's", mounted)
	}
}

// An unknown engine is refused before any task is planned, the same as an
// unknown extension — the spec promises a 400 rather than a silent fallback.
func TestCreateClusterUnknownEngine(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "engine-bad-pool")

	bogus := api.CreateClusterRequestEngine("mysql")
	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "nope", Engine: &bogus})
	if err != nil {
		t.Fatalf("CreateCluster(unknown engine): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("unknown engine status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}
}

// With no catalog (the Docker driver), a non-default engine is refused rather
// than silently creating a cluster without the bundle the caller asked for.
// Plain postgres still works, since its bundle is empty.
func TestCreateClusterEngineWithoutCatalog(t *testing.T) {
	prov := api.Provisioning{Deployer: &fakeDeployer{}} // no Extensions catalog
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "engine-nocat-pool")

	aidb := api.Aidb
	bad, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "nope", Engine: &aidb})
	if err != nil {
		t.Fatalf("CreateCluster(aidb, no catalog): %v", err)
	}
	if bad.StatusCode() != http.StatusBadRequest || bad.JSON400 == nil {
		t.Fatalf("aidb-without-catalog status = %d, want 400; body = %s", bad.StatusCode(), bad.Body)
	}

	pg := api.Postgres
	ok, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{Name: "plain", Engine: &pg})
	if err != nil {
		t.Fatalf("CreateCluster(postgres, no catalog): %v", err)
	}
	if ok.StatusCode() != http.StatusAccepted {
		t.Fatalf("postgres-without-catalog status = %d, want 202; body = %s", ok.StatusCode(), ok.Body)
	}
}
