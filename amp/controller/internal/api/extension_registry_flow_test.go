package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
	"github.com/yugabyte/amp/controller/internal/task"
)

// The registration API end to end: an admin registers an image nothing in the
// product knows about, a branch opts into it by name, and it reaches the
// deployer as a real mount with its layout intact. PostGIS from CloudNativePG is
// the worked example — it appears here and in no non-test source file, which is
// the property this whole feature exists to establish.

func postgisBody() api.RegisterExtensionRequest {
	share, lib := "share", "lib"
	version := "3.6.2"
	desc := "Spatial types, functions and indexes"
	return api.RegisterExtensionRequest{
		Image:       "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm",
		Version:     &version,
		Description: &desc,
		Layout: &api.ExtensionLayout{
			SharePath:   &share,
			LibPath:     &lib,
			LibraryPath: &[]string{"system"},
			EnvPaths:    &map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"},
		},
		AbiExtraExempt: &[]string{"postgis-3", "postgis_raster-3", "postgis_topology-3"},
	}
}

func newRegistryServer(t *testing.T) (*api.ClientWithResponses, *core.MemBackend, *task.MemStore, api.Provisioning, *fakeDeployer) {
	t.Helper()
	backend := core.NewMemBackend()
	dep := &fakeDeployer{}
	prov := api.Provisioning{
		Deployer:   dep,
		Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), backend),
	}
	client, _, store := newTestServerWith(t, backend, prov)
	return client, backend, store, prov, dep
}

func TestRegisterExtensionRequiresAnAdmin(t *testing.T) {
	client, _, _, _, _ := newRegistryServer(t)
	ctx := context.Background()
	plain := mustCreateUser(t, client, false)

	resp, err := client.RegisterExtensionWithResponse(ctx, "postgis",
		&api.RegisterExtensionParams{UserId: plain.Id}, postgisBody())
	if err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}
	if resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("non-admin register status = %d, want 403; body = %s", resp.StatusCode(), resp.Body)
	}

	// And the same call as an admin succeeds, so 403 is about the admin bit and
	// not about the body.
	admin := mustCreateUser(t, client, true)
	ok, err := client.RegisterExtensionWithResponse(ctx, "postgis",
		&api.RegisterExtensionParams{UserId: admin.Id}, postgisBody())
	if err != nil {
		t.Fatalf("RegisterExtension(admin): %v", err)
	}
	if ok.StatusCode() != http.StatusOK || ok.JSON200 == nil {
		t.Fatalf("admin register status = %d, body = %s", ok.StatusCode(), ok.Body)
	}
	if ok.JSON200.Builtin {
		t.Error("a registered extension reported builtin = true")
	}
	if ok.JSON200.Image != "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm" {
		t.Errorf("image = %q, want the reference as registered", ok.JSON200.Image)
	}
}

func TestRegisteredExtensionMountsOnABranch(t *testing.T) {
	client, backend, store, prov, dep := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)
	pool := mustCreatePool(t, client, admin.Id, "gis-pool")

	if resp, err := client.RegisterExtensionWithResponse(ctx, "postgis",
		&api.RegisterExtensionParams{UserId: admin.Id}, postgisBody()); err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("RegisterExtension: %v (status %d)", err, resp.StatusCode())
	}

	// createCluster's extensions[] validates against the same catalog, so a name
	// that exists only because it was registered has to pass.
	withWriter := true
	created, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "gis", Extensions: &[]string{"postgis"}, WithWriter: &withWriter,
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if created.StatusCode() != http.StatusAccepted || created.JSON202 == nil {
		t.Fatalf("CreateCluster status = %d, body = %s", created.StatusCode(), created.Body)
	}
	drainTasks(t, store, backend, prov)

	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	var got *struct {
		Name, Image, SharePath, LibPath string
		LibraryPath, ABI                []string
		EnvPaths                        map[string]string
	}
	for _, e := range dep.created[0].Extensions {
		if e.Name != "postgis" {
			continue
		}
		got = &struct {
			Name, Image, SharePath, LibPath string
			LibraryPath, ABI                []string
			EnvPaths                        map[string]string
		}{e.Name, e.Image, e.SharePath, e.LibPath, e.LibraryPath, e.ABIExtraExempt, e.EnvPaths}
	}
	if got == nil {
		t.Fatalf("postgis did not reach the deployer; mounts = %+v", dep.created[0].Extensions)
	}
	// Every field the endpoint needs to make the image usable has to survive the
	// whole path: API body -> database -> catalog -> deploy spec.
	if got.Image != "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm" {
		t.Errorf("image = %q", got.Image)
	}
	if got.SharePath != "share" || got.LibPath != "lib" {
		t.Errorf("layout = %q/%q, want share/lib", got.SharePath, got.LibPath)
	}
	if len(got.LibraryPath) != 1 || got.LibraryPath[0] != "system" {
		t.Errorf("library_path = %v", got.LibraryPath)
	}
	if got.EnvPaths["PROJ_DATA"] != "share/proj" {
		t.Errorf("env_paths = %v", got.EnvPaths)
	}
	if len(got.ABI) != 3 {
		t.Errorf("abi_extra_exempt = %v, want 3 modules", got.ABI)
	}
}

func TestRegisterExtensionRejectsUnsafeBody(t *testing.T) {
	client, _, _, _, _ := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)

	cases := map[string]func(*api.RegisterExtensionRequest){
		"untagged image": func(b *api.RegisterExtensionRequest) { b.Image = "ghcr.io/cloudnative-pg/postgis-extension" },
		"latest tag":     func(b *api.RegisterExtensionRequest) { b.Image = "ghcr.io/x/y:latest" },
		"absolute path": func(b *api.RegisterExtensionRequest) {
			abs := "/share"
			b.Layout.SharePath = &abs
		},
		"reserved env": func(b *api.RegisterExtensionRequest) {
			b.Layout.EnvPaths = &map[string]string{"LD_LIBRARY_PATH": "system"}
		},
		"abi entry keeps .so": func(b *api.RegisterExtensionRequest) {
			b.AbiExtraExempt = &[]string{"postgis-3.so"}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			body := postgisBody()
			mut(&body)
			resp, err := client.RegisterExtensionWithResponse(ctx, "postgis",
				&api.RegisterExtensionParams{UserId: admin.Id}, body)
			if err != nil {
				t.Fatalf("RegisterExtension: %v", err)
			}
			if resp.StatusCode() != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400; body = %s", name, resp.StatusCode(), resp.Body)
			}
		})
	}
}

func TestRegisterExtensionRefusesToShadowABuiltin(t *testing.T) {
	client, _, _, _, _ := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)
	resp, err := client.RegisterExtensionWithResponse(ctx, "pgvector",
		&api.RegisterExtensionParams{UserId: admin.Id}, postgisBody())
	if err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict {
		t.Fatalf("shadowing status = %d, want 409; body = %s", resp.StatusCode(), resp.Body)
	}
}

func TestListExtensionsReportsBuiltinsAndRegistered(t *testing.T) {
	client, _, _, _, _ := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)
	if _, err := client.RegisterExtensionWithResponse(ctx, "postgis",
		&api.RegisterExtensionParams{UserId: admin.Id}, postgisBody()); err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}
	list, err := client.ListExtensionsWithResponse(ctx)
	if err != nil {
		t.Fatalf("ListExtensions: %v", err)
	}
	if list.StatusCode() != http.StatusOK || list.JSON200 == nil {
		t.Fatalf("ListExtensions status = %d, body = %s", list.StatusCode(), list.Body)
	}
	seen := map[string]bool{}
	for _, e := range list.JSON200.Extensions {
		seen[e.Name] = e.Builtin
	}
	if builtin, ok := seen["pgvector"]; !ok || !builtin {
		t.Errorf("pgvector missing or not marked builtin: %+v", seen)
	}
	if builtin, ok := seen["postgis"]; !ok || builtin {
		t.Errorf("postgis missing or wrongly marked builtin: %+v", seen)
	}
	if _, ok := seen["postgis"]; ok && list.JSON200.Total != len(list.JSON200.Extensions) {
		t.Errorf("total = %d, count = %d", list.JSON200.Total, len(list.JSON200.Extensions))
	}
}

func TestDeregisterExtensionRefusedWhileABranchUsesIt(t *testing.T) {
	client, backend, store, prov, _ := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)
	pool := mustCreatePool(t, client, admin.Id, "gis-pool")
	params := &api.RegisterExtensionParams{UserId: admin.Id}

	if _, err := client.RegisterExtensionWithResponse(ctx, "postgis", params, postgisBody()); err != nil {
		t.Fatalf("RegisterExtension: %v", err)
	}
	withWriter := true
	if _, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "gis", Extensions: &[]string{"postgis"}, WithWriter: &withWriter,
	}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	drainTasks(t, store, backend, prov)

	del, err := client.DeregisterExtensionWithResponse(ctx, "postgis",
		&api.DeregisterExtensionParams{UserId: admin.Id})
	if err != nil {
		t.Fatalf("DeregisterExtension: %v", err)
	}
	// Deleting it would strip the mount from that branch's endpoints on their
	// next deploy, taking any objects built on it with them.
	if del.StatusCode() != http.StatusConflict {
		t.Fatalf("in-use deregister status = %d, want 409; body = %s", del.StatusCode(), del.Body)
	}

	// A built-in is likewise not deletable.
	builtin, err := client.DeregisterExtensionWithResponse(ctx, "pgvector",
		&api.DeregisterExtensionParams{UserId: admin.Id})
	if err != nil {
		t.Fatalf("DeregisterExtension(pgvector): %v", err)
	}
	if builtin.StatusCode() != http.StatusConflict {
		t.Fatalf("builtin deregister status = %d, want 409", builtin.StatusCode())
	}
}

func TestAddBranchExtensionsRejectsAnUnregisteredName(t *testing.T) {
	// The opt-in gate is the catalog, so a name that was never registered is a
	// 400 rather than a mount that silently never appears.
	client, _, _, _, _ := newRegistryServer(t)
	ctx := context.Background()
	admin := mustCreateUser(t, client, true)
	pool := mustCreatePool(t, client, admin.Id, "gis-pool")
	withWriter := false
	created, err := client.CreateClusterWithResponse(ctx, pool.Id, api.CreateClusterRequest{
		Name: "plain", Extensions: &[]string{"postgis"}, WithWriter: &withWriter,
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if created.StatusCode() != http.StatusBadRequest {
		t.Fatalf("unregistered name status = %d, want 400; body = %s", created.StatusCode(), created.Body)
	}
}
