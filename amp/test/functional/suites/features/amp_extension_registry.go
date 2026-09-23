// Operator-registered extension images (meko PR #1206), driven with the PostGIS image of amp/runbooks/postgis.md.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Pinned with amp/test/integration/compute/ext_fixtures/postgis.json: -bookworm matches the endpoint's glibc.
const defaultPostGISImage = "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_registry",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Operator-registered extension images: registration is admin-only (403), strictly validated (400 for :latest, an untagged image, a bad name, an absolute or .. layout path, an env var the endpoint owns) and refuses to shadow a built-in or collide with one on its volume name (409); a registration reads back with builtin=false, a re-PUT replaces it in full, and deregistering a built-in or an unknown name is refused. A cluster created opted into a registered PostGIS image mounts it: CREATE EXTENSION postgis, ST_Area, a GiST nearest-neighbour scan and ST_Transform (proj.db found through env_paths) all work, a fork of that branch inherits the mount and reads the parent's geometries, the extension survives a compute restart, deregistration is refused with 409 naming both branches while they opt in, and succeeds once the cluster is deleted.",
		Params:      []string{"functional", "functional.extensions", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpExtensionRegistry,
	})
}

type extRegistryRun struct {
	a       *ampFeatureRun
	cl      *controller.Client
	admin   string
	tenant  string
	name    string
	image   string
	gis     ampCluster
	fork    string
	deleted bool
}

func runAmpExtensionRegistry(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &extRegistryRun{a: a}
		return []phase{
			{"registry_contract", r.contract},
			{"registered_mount", r.mount},
			{"fork_inherits_registered", r.forkInherits},
			{"survives_compute_restart", r.restart},
			{"deregister_lifecycle", r.deregister},
		}
	})
}

func (r *extRegistryRun) postGISBody() controller.RegisterExtensionRequest {
	return controller.RegisterExtensionRequest{
		Image:       r.image,
		Version:     "3.6.2",
		Description: "ampqa registered PostGIS",
		Layout: &controller.ExtensionLayout{
			SharePath:   "share",
			LibPath:     "lib",
			LibraryPath: []string{"system"},
			EnvPaths:    map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"},
		},
		ABIExtraExempt: []string{"postgis-3", "postgis_raster-3", "postgis_topology-3", "postgis_sfcgal-3", "address_standardizer-3"},
	}
}

func (r *extRegistryRun) contract(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	r.image = r.a.f.t.Options.GetString("functional.extensions.postgis_image", defaultPostGISImage)
	r.name = "qa-gis-" + r.a.f.suffix()

	list, err := cl.ListExtensions(ctx)
	if err != nil {
		if extensionsUnavailable(err) {
			r.a.f.t.Skip("this control plane has no extension catalog: " + err.Error())
			return nil
		}
		return fmt.Errorf("list extensions: %w", err)
	}
	builtins := 0
	for _, e := range list.Extensions {
		if e.Builtin {
			builtins++
		}
	}
	if builtins == 0 {
		return fmt.Errorf("GET /extensions lists %d entries and none is builtin=true: the built-in catalog must always be listed", len(list.Extensions))
	}
	if list.Total != len(list.Extensions) {
		return fmt.Errorf("GET /extensions reports total=%d for %d entries", list.Total, len(list.Extensions))
	}

	admin, err := cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return fmt.Errorf("create the admin user: %w", err)
	}
	tenant, err := cl.CreateUser(ctx, controller.CreateUserRequest{})
	if err != nil {
		return fmt.Errorf("create the non-admin user: %w", err)
	}
	r.admin, r.tenant = admin.ID, tenant.ID
	r.a.f.registerLast("deregister "+r.name, func(ctx context.Context) error {
		if err := cl.DeregisterExtension(ctx, r.name, r.admin); err != nil && apiErrorStatus(err) != http.StatusNotFound {
			return err
		}
		return nil
	})

	if _, err := cl.RegisterExtension(ctx, r.name, r.tenant, r.postGISBody()); apiErrorStatus(err) != http.StatusForbidden {
		return fmt.Errorf("a non-admin registering %s answered %v, want 403: registration picks the image an endpoint pod runs", r.name, err)
	}
	if _, err := cl.GetExtension(ctx, r.name); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("after a refused non-admin PUT, GET /extensions/%s answered %v, want 404", r.name, err)
	}

	bad := []struct {
		what string
		name string
		mut  func(*controller.RegisterExtensionRequest)
	}{
		{"an image pinned to :latest", r.name, func(b *controller.RegisterExtensionRequest) {
			b.Image = "ghcr.io/cloudnative-pg/postgis-extension:latest"
		}},
		{"an untagged image", r.name, func(b *controller.RegisterExtensionRequest) { b.Image = "ghcr.io/cloudnative-pg/postgis-extension" }},
		{"an absolute share_path", r.name, func(b *controller.RegisterExtensionRequest) { b.Layout.SharePath = "/etc" }},
		{"a .. lib_path", r.name, func(b *controller.RegisterExtensionRequest) { b.Layout.LibPath = "../../usr/lib" }},
		{"an env_paths name the endpoint owns", r.name, func(b *controller.RegisterExtensionRequest) {
			b.Layout.EnvPaths = map[string]string{"LD_LIBRARY_PATH": "system"}
		}},
		{"an abi_extra_exempt entry that is a path", r.name, func(b *controller.RegisterExtensionRequest) { b.ABIExtraExempt = []string{"../postgis-3.so"} }},
		{"an upper-case name", "QA-GIS-" + r.a.f.suffix(), func(*controller.RegisterExtensionRequest) {}},
	}
	for _, tc := range bad {
		body := r.postGISBody()
		tc.mut(&body)
		if _, err := cl.RegisterExtension(ctx, tc.name, r.admin, body); apiErrorStatus(err) != http.StatusBadRequest {
			return fmt.Errorf("registering %s answered %v, want 400", tc.what, err)
		}
	}
	for _, shadow := range []string{"pgvector", "pg-cron"} {
		if _, err := cl.RegisterExtension(ctx, shadow, r.admin, r.postGISBody()); apiErrorStatus(err) != http.StatusConflict {
			return fmt.Errorf("registering %q over the built-in catalog answered %v, want 409: shadowing a built-in is refused, including on the sanitized volume name", shadow, err)
		}
	}

	got, err := cl.RegisterExtension(ctx, r.name, r.admin, r.postGISBody())
	if err != nil {
		return fmt.Errorf("an admin registering %s: %w", r.name, err)
	}
	if got.Builtin || got.Image != r.image || got.Name != r.name {
		return fmt.Errorf("registering %s answered %+v, want builtin=false, image %s", r.name, got, r.image)
	}
	twin := strings.ReplaceAll(r.name, "-", "_")
	if _, err := cl.RegisterExtension(ctx, twin, r.admin, r.postGISBody()); apiErrorStatus(err) != http.StatusConflict {
		if err == nil {
			_ = cl.DeregisterExtension(ctx, twin, r.admin)
		}
		return fmt.Errorf("registering %q beside %q answered %v, want 409: both render the same ImageVolume name and a branch opting into both would fail every deploy", twin, r.name, err)
	}

	replaced := r.postGISBody()
	replaced.Version = ""
	replaced.Description = "replaced by ampqa"
	if _, err := cl.RegisterExtension(ctx, r.name, r.admin, replaced); err != nil {
		return fmt.Errorf("re-registering %s: %w", r.name, err)
	}
	read, err := cl.GetExtension(ctx, r.name)
	if err != nil {
		return fmt.Errorf("GET /extensions/%s: %w", r.name, err)
	}
	if read.Description != replaced.Description || read.Version != "" {
		return fmt.Errorf("after a re-PUT with no version, %s reads back version %q description %q: a PUT replaces a registration in full", r.name, read.Version, read.Description)
	}
	if read.Layout == nil || read.Layout.SharePath != "share" || read.Layout.EnvPaths["PROJ_DATA"] != "share/proj" {
		return fmt.Errorf("%s reads back layout %+v, want the registered share/proj layout", r.name, read.Layout)
	}
	if _, err := cl.RegisterExtension(ctx, r.name, r.admin, r.postGISBody()); err != nil {
		return fmt.Errorf("restore %s: %w", r.name, err)
	}
	if list, err = cl.ListExtensions(ctx); err != nil {
		return fmt.Errorf("list extensions after registering: %w", err)
	}
	listed := false
	for _, e := range list.Extensions {
		if e.Name == r.name {
			listed = !e.Builtin
		}
	}
	if !listed {
		return fmt.Errorf("GET /extensions does not list %s as a registered (builtin=false) entry", r.name)
	}

	if err := cl.DeregisterExtension(ctx, r.name, r.tenant); apiErrorStatus(err) != http.StatusForbidden {
		return fmt.Errorf("a non-admin deregistering %s answered %v, want 403", r.name, err)
	}
	if err := cl.DeregisterExtension(ctx, "pgvector", r.admin); apiErrorStatus(err) != http.StatusConflict {
		return fmt.Errorf("deregistering the built-in pgvector answered %v, want 409", err)
	}
	if err := cl.DeregisterExtension(ctx, "qa-never-"+r.a.f.suffix(), r.admin); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("deregistering a name nobody registered answered %v, want 404", err)
	}
	return nil
}

func (r *extRegistryRun) mount(ctx context.Context) error {
	name := "gis-" + r.a.f.suffix()
	if _, err := r.cl.CreateClusterAndWait(ctx, r.a.proj.poolID, controller.CreateClusterRequest{
		Name: name, PgVersion: r.a.f.cfg.PgVersion, Extensions: []string{r.name},
	}); err != nil {
		return fmt.Errorf("create a cluster opted into %s: %w", r.name, err)
	}
	c, err := findClusterCtl(ctx, r.cl, r.a.proj.poolID, name)
	if err != nil {
		return err
	}
	r.gis = ampCluster{poolID: r.a.proj.poolID, projID: c.ID, branchID: c.DefaultBranch.BranchID}
	r.a.f.register("delete the PostGIS cluster", func(ctx context.Context) error {
		if r.deleted {
			return nil
		}
		_, err := r.cl.DeleteClusterAndWait(ctx, r.gis.poolID, r.gis.projID)
		return err
	})
	if !containsString(c.Extensions, r.name) {
		return fmt.Errorf("cluster %s was created with extensions [%s] and reads back %v", name, r.name, c.Extensions)
	}

	ep, err := createEndpoint(ctx, r.cl, r.gis.poolID, r.gis.projID, r.gis.branchID)
	if err != nil {
		return fmt.Errorf("create the writer on the PostGIS cluster: %w", err)
	}
	conn, err := r.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the PostGIS writer: %w", err)
	}
	defer conn.Close()
	ver, err := conn.QueryInt(ctx, "SELECT current_setting('server_version_num')::int / 10000")
	if err != nil {
		return fmt.Errorf("read server_version_num: %w", err)
	}
	if ver != 18 {
		return fmt.Errorf("the endpoint runs PostgreSQL %d but %s is built for 18: set functional.extensions.postgis_image to the matching build", ver, r.image)
	}
	avail, err := conn.QueryInt(ctx, "SELECT count(*) FROM pg_available_extensions WHERE name = 'postgis'")
	if err != nil {
		return fmt.Errorf("probe pg_available_extensions: %w", err)
	}
	if avail == 0 {
		return fmt.Errorf("the endpoint of a branch opted into registered extension %s has no postgis in pg_available_extensions: the ImageVolume was not mounted or share_path was not appended to extension_control_path", r.name)
	}
	if _, err := conn.Exec(ctx, "CREATE EXTENSION postgis"); err != nil {
		return fmt.Errorf("CREATE EXTENSION postgis from registered image %s: %w", r.image, err)
	}
	for _, stmt := range []string{
		"CREATE TABLE qa_places (id int PRIMARY KEY, geom geometry(Point, 4326))",
		"INSERT INTO qa_places SELECT g, ST_SetSRID(ST_MakePoint(g * 0.01, g * 0.02), 4326) FROM generate_series(1, 500) g",
		"CREATE INDEX qa_places_gix ON qa_places USING gist (geom)",
		"ANALYZE qa_places",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return postGISChecks(ctx, conn, "the writer of the opted-in cluster")
}

func postGISChecks(ctx context.Context, c sqlConn, where string) error {
	area, err := c.QueryString(ctx, "SELECT ST_Area(ST_GeomFromText('POLYGON((0 0,0 2,3 2,3 0,0 0))'))::text")
	if err != nil {
		return fmt.Errorf("ST_Area on %s: %w", where, err)
	}
	if area != "6" {
		return fmt.Errorf("ST_Area of a 3x2 rectangle on %s is %s, want 6", where, area)
	}
	nearest, err := c.QueryString(ctx, "SELECT string_agg(id::text, ',') FROM (SELECT id FROM qa_places ORDER BY geom <-> ST_SetSRID(ST_MakePoint(1.0, 2.0), 4326) LIMIT 3) s")
	if err != nil {
		return fmt.Errorf("GiST nearest-neighbour scan on %s: %w", where, err)
	}
	if !strings.HasPrefix(nearest, "100,") {
		return fmt.Errorf("the 3 points nearest (1,2) on %s are %q, want 100 first", where, nearest)
	}
	x, err := c.QueryInt(ctx, "SELECT round(ST_X(ST_Transform(ST_SetSRID(ST_MakePoint(1.0, 0.0), 4326), 3857)))::bigint")
	if err != nil {
		return fmt.Errorf("ST_Transform to 3857 on %s (fails when PROJ_DATA does not reach proj.db): %w", where, err)
	}
	if x != 111319 {
		return fmt.Errorf("ST_Transform of lon 1 to EPSG:3857 on %s gave x=%d, want 111319", where, x)
	}
	return nil
}

func (r *extRegistryRun) forkInherits(ctx context.Context) error {
	name := "gis-fork-" + r.a.f.suffix()
	conn, err := forkChildAt(ctx, r.cl, r.gis, r.gis.branchID, "", name, r.a.f.endpointSQL)
	if err != nil {
		return fmt.Errorf("fork the PostGIS branch: %w", err)
	}
	defer conn.Close()
	b, err := findBranchCtl(ctx, r.cl, r.gis.poolID, r.gis.projID, name)
	if err != nil {
		return err
	}
	r.fork = b.BranchID
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM qa_places WHERE ST_IsValid(geom)")
	if err != nil {
		return fmt.Errorf("read the parent's geometries on the fork (the fork must inherit the registered mount, or every object built on it breaks): %w", err)
	}
	if n != 500 {
		return fmt.Errorf("the fork reads %d valid geometries, want the parent's 500", n)
	}
	return postGISChecks(ctx, conn, "the fork")
}

func (r *extRegistryRun) restart(ctx context.Context) error {
	ep, ok := lastEndpoint(ctx, r.cl, r.gis.poolID, r.gis.projID, r.gis.branchID)
	if !ok {
		return fmt.Errorf("the PostGIS branch has no endpoint to restart")
	}
	c, ok, err := r.a.restartEndpointCompute(ctx, ep)
	if err != nil || !ok {
		return err
	}
	defer c.Close()
	return postGISChecks(ctx, c, "the writer after a compute restart")
}

func (r *extRegistryRun) deregister(ctx context.Context) error {
	err := r.cl.DeregisterExtension(ctx, r.name, r.admin)
	if apiErrorStatus(err) != http.StatusConflict {
		return fmt.Errorf("deregistering %s while two branches opt into it answered %v, want 409", r.name, err)
	}
	for _, br := range []string{r.gis.branchID, r.fork} {
		if br != "" && !strings.Contains(err.Error(), br) {
			return fmt.Errorf("the in-use refusal %q does not name branch %s", err, br)
		}
	}
	if _, err := r.cl.DeleteClusterAndWait(ctx, r.gis.poolID, r.gis.projID); err != nil {
		return fmt.Errorf("delete the PostGIS cluster: %w", err)
	}
	r.deleted = true
	if err := r.cl.DeregisterExtension(ctx, r.name, r.admin); err != nil {
		return fmt.Errorf("deregistering %s after its only cluster was deleted: %w (a dropped branch must not keep a registration alive)", r.name, err)
	}
	if _, err := r.cl.GetExtension(ctx, r.name); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("GET /extensions/%s after deregistering answered %v, want 404", r.name, err)
	}
	gone := "gis-gone-" + r.a.f.suffix()
	if _, _, err := r.cl.CreateCluster(ctx, r.a.proj.poolID, controller.CreateClusterRequest{
		Name: gone, PgVersion: r.a.f.cfg.PgVersion, Extensions: []string{r.name},
	}); apiErrorStatus(err) != http.StatusBadRequest {
		if err == nil {
			r.a.f.register("delete "+gone, func(ctx context.Context) error {
				c, ferr := findClusterCtl(ctx, r.cl, r.a.proj.poolID, gone)
				if ferr != nil {
					return nil
				}
				_, ferr = r.cl.DeleteClusterAndWait(ctx, r.a.proj.poolID, c.ID)
				return ferr
			})
		}
		return fmt.Errorf("creating a cluster opted into the deregistered %s answered %v, want 400 unknown extension", r.name, err)
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
