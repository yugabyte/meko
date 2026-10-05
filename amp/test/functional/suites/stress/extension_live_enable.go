// A customer enabled PostGIS on a branch that already served traffic, which
// redeploys every endpoint on it. On their EKS nodes the extension's
// ImageVolume came up as an empty directory, with no error from Kubernetes or
// amp, and CREATE EXTENSION answered `extension "postgis" is not available`.
// GKE mounts images correctly, so the last step stands in for the empty mount
// with an image that carries no extension tree: the endpoint sees the same
// empty /extensions/<name>, and amp has to say so instead of reporting success.
package stress

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "stress/extension_live_enable",
		Suite:      "stress",
		Category:   core.Stress,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"stress"},
		OwningTeam: core.TeamControlPlane,
		Description: "Register PostGIS and enable it on a branch whose writer already serves data. " +
			"Every redeployed endpoint must mount a non-empty image, list postgis, keep every acknowledged row, and run ST_Area, a GiST nearest-neighbour scan and ST_Transform, " +
			"also after restart cycles that read and write on the cold compute. " +
			"Then enable a registered image that carries no extension tree, the endpoint's view of an ImageVolume the node mounted empty: " +
			"amp must refuse it, fail the enable naming the extension, or keep the endpoint from starting with a reason that names it, never serve with nothing behind the mount.",
		Params:  []string{"stress.extension_live_enable"},
		Timeout: 90 * time.Minute,
		Run:     runExtensionLiveEnable,
	})
}

type extensionLiveEnableParams struct {
	Cycles       int
	Rows         int
	RowsPerCycle int
	Image        string
	EmptyImage   string
	ReadyTimeout time.Duration
}

func extensionLiveEnableParamsFromConfig(c *core.Config) extensionLiveEnableParams {
	return extensionLiveEnableParams{
		Cycles:       c.GetInt("stress.extension_live_enable.cycles", 3),
		Rows:         c.GetInt("stress.extension_live_enable.rows", 5000),
		RowsPerCycle: c.GetInt("stress.extension_live_enable.rows_per_cycle", 500),
		Image:        c.GetString("stress.extension_live_enable.postgis_image", "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
		EmptyImage:   c.GetString("stress.extension_live_enable.empty_image", "registry.k8s.io/pause:3.10"),
		ReadyTimeout: c.GetDuration("stress.extension_live_enable.ready_timeout", 10*time.Minute),
	}
}

type extensionLiveEnableRun struct {
	*liveBranchRun
	p     extensionLiveEnableParams
	admin string
	gis   string
	empty string
}

func runExtensionLiveEnable(t *core.T) error {
	p := extensionLiveEnableParamsFromConfig(t.Options)
	lb, ok, err := newLiveBranchRun(t, p.ReadyTimeout)
	if !ok {
		t.Skip("requires a standing amp control plane with a kube handle for the endpoint pods (sharedk8s)")
		return nil
	}
	if err != nil {
		return err
	}
	r := &extensionLiveEnableRun{liveBranchRun: lb, p: p, gis: "qa-gis-" + t.ResourceSuffix(), empty: "qa-empty-" + t.ResourceSuffix()}
	defer r.cleanup()
	return r.steps(
		liveStep{"register_postgis", r.register},
		liveStep{"live_branch", func(s *core.StepHandle) error { return r.createLiveBranch(s, "live-gis", r.p.Rows) }},
		liveStep{"enable_postgis_on_live_branch", r.enablePostGIS},
		liveStep{"restart_cycles", func(s *core.StepHandle) error {
			return r.restartCycles(s, r.p.Cycles, r.p.RowsPerCycle, r.verifyPostGIS)
		}},
		liveStep{"empty_mount_is_not_enabled", r.emptyMount},
	)
}

func (r *extensionLiveEnableRun) register(s *core.StepHandle) error {
	admin, err := r.cl.CreateUser(r.t.Context, controller.CreateUserRequest{IsAdmin: true})
	if err != nil {
		return fmt.Errorf("create the admin user: %w", err)
	}
	r.admin = admin.ID
	if _, err := r.cl.RegisterExtension(r.t.Context, r.gis, r.admin, postGISRegistration(r.p.Image)); err != nil {
		return fmt.Errorf("register %s from %s: %w", r.gis, r.p.Image, err)
	}
	r.onCleanup(func(ctx context.Context) {
		for _, name := range []string{r.gis, r.empty} {
			if err := r.cl.DeregisterExtension(ctx, name, r.admin); err != nil {
				r.logf("extension_live_enable: deregister %s: %v", name, err)
			}
		}
	})
	s.Set("extension", r.gis)
	s.Set("image", r.p.Image)
	return nil
}

func postGISRegistration(image string) controller.RegisterExtensionRequest {
	return controller.RegisterExtensionRequest{
		Image:       image,
		Version:     "3.6.2",
		Description: "ampqa live-enable PostGIS",
		Layout: &controller.ExtensionLayout{
			SharePath:   "share",
			LibPath:     "lib",
			LibraryPath: []string{"system"},
			EnvPaths:    map[string]string{"PROJ_DATA": "share/proj", "GDAL_DATA": "share/gdal"},
		},
		ABIExtraExempt: []string{"postgis-3", "postgis_raster-3", "postgis_topology-3", "postgis_sfcgal-3", "address_standardizer-3"},
	}
}

func (r *extensionLiveEnableRun) enablePostGIS(s *core.StepHandle) error {
	ctx := r.t.Context
	r.closeConns()
	start := time.Now()
	if _, err := r.cl.AddBranchExtensionsAndWait(ctx, r.poolID, r.cluster.ID, r.cluster.DefaultBranch.BranchID,
		controller.AddBranchExtensionsRequest{Extensions: []string{r.gis}}); err != nil {
		return fmt.Errorf("enable %s on the live branch: %w", r.gis, err)
	}
	r.t.Metric("enable_seconds", time.Since(start).Seconds(), "s")
	for _, le := range r.eps {
		if err := ampstack.WaitEndpointWorkloadReady(ctx, r.kube, le.ep.ID, r.readyTimeout); err != nil {
			return fmt.Errorf("the %s endpoint %s never came back after enabling %s: %w", le.role, le.ep.ID, r.gis, err)
		}
		files, err := r.controlFiles(le, r.gis)
		if err != nil {
			return err
		}
		if !strings.Contains(files, "postgis.control") {
			return fmt.Errorf("the %s endpoint has no postgis.control under /extensions/%s/share/extension (found %q): the image is not mounted or mounted empty", le.role, r.gis, files)
		}
	}
	if err := r.connectAll(); err != nil {
		return err
	}
	for _, le := range r.eps {
		n, err := le.conn.QueryInt(ctx, "SELECT count(*) FROM pg_available_extensions WHERE name = 'postgis'")
		if err != nil {
			return fmt.Errorf("read pg_available_extensions on the %s endpoint: %w", le.role, err)
		}
		if n != 1 {
			return fmt.Errorf("the %s endpoint lists no postgis after %s was enabled on its branch: the image mounted, but share_path is not on extension_control_path", le.role, r.gis)
		}
	}
	for _, stmt := range []string{
		"CREATE EXTENSION postgis",
		"ALTER TABLE qa_places ADD COLUMN geom geometry(Point, 4326)",
		"UPDATE qa_places SET geom = ST_SetSRID(ST_MakePoint(lon, lat), 4326)",
		"CREATE INDEX qa_places_geom ON qa_places USING gist (geom)",
	} {
		if _, err := r.writer().conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s on the writer: %w", stmt, err)
		}
	}
	r.geomReady = true
	for _, le := range r.eps {
		if err := r.verifyRows(le); err != nil {
			return err
		}
		if err := r.verifyPostGIS(le); err != nil {
			return err
		}
	}
	s.Set("rows", r.rows)
	return nil
}

func (r *extensionLiveEnableRun) emptyMount(s *core.StepHandle) error {
	ctx := r.t.Context
	if _, err := r.cl.RegisterExtension(ctx, r.empty, r.admin, controller.RegisterExtensionRequest{
		Image:       r.p.EmptyImage,
		Version:     "1.0",
		Description: "ampqa image with no extension tree",
		Layout:      &controller.ExtensionLayout{SharePath: "share", LibPath: "lib"},
	}); err != nil {
		s.Set("outcome", "registration refused")
		r.logf("extension_live_enable: registering %s from %s was refused: %v", r.empty, r.p.EmptyImage, err)
		return nil
	}
	r.closeConns()
	_, err := r.cl.AddBranchExtensionsAndWait(ctx, r.poolID, r.cluster.ID, r.cluster.DefaultBranch.BranchID,
		controller.AddBranchExtensionsRequest{Extensions: []string{r.empty}})
	if err != nil {
		if !strings.Contains(err.Error(), r.empty) {
			return fmt.Errorf("enabling %s, whose mount carries no extension tree, failed without naming it: %w", r.empty, err)
		}
		s.Set("outcome", "enable failed naming the extension")
		return nil
	}
	var evidence []string
	for _, le := range r.eps {
		if err := ampstack.WaitEndpointWorkloadReady(ctx, r.kube, le.ep.ID, r.readyTimeout); err != nil {
			why, named := r.refusalNames(le, r.empty)
			if !named {
				return fmt.Errorf("the %s endpoint did not come back after enabling %s, and nothing it reported names the extension, so this is not the refusal meko#1705 asks for: %w (pod status and logs: %s)", le.role, r.empty, err, why)
			}
			s.Set("outcome", "endpoint refused to start naming the extension")
			return nil
		}
		files, err := r.controlFiles(le, r.empty)
		if err != nil {
			return err
		}
		evidence = append(evidence, fmt.Sprintf("%s endpoint: share/extension holds %q", le.role, files))
	}
	return fmt.Errorf("amp reported %s enabled and every endpoint ready while /extensions/%s holds no extension (%s): an empty extension mount, what EKS gave the customer for PostGIS, is accepted silently and surfaces only as `extension is not available` at CREATE EXTENSION",
		r.empty, r.empty, strings.Join(evidence, "; "))
}

func (r *extensionLiveEnableRun) refusalNames(le *liveEndpoint, ext string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pod, err := ampstack.EndpointPodName(ctx, r.kube, le.ep.ID)
	if err != nil {
		return err.Error(), false
	}
	desc, _ := r.kube.Describe(ctx, "pod", pod)
	logs, _ := r.kube.LogsAll(ctx, "pod/"+pod, 200)
	tail := logs
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	return tail, strings.Contains(desc+logs, ext)
}

func (r *extensionLiveEnableRun) controlFiles(le *liveEndpoint, ext string) (string, error) {
	pod, err := ampstack.EndpointPodName(r.t.Context, r.kube, le.ep.ID)
	if err != nil {
		return "", fmt.Errorf("find the %s endpoint pod: %w", le.role, err)
	}
	res, err := r.kube.ForPod(pod, "endpoint").Run(r.t.Context, "ls -A /extensions/"+ext+"/share/extension 2>&1 | grep '\\.control$' || true")
	if err != nil {
		return "", fmt.Errorf("list /extensions/%s in the %s endpoint pod %s: %w", ext, le.role, pod, err)
	}
	return strings.Join(strings.Fields(res.Stdout), ","), nil
}

func (r *extensionLiveEnableRun) verifyPostGIS(le *liveEndpoint) error {
	ctx := r.t.Context
	area, err := le.conn.QueryString(ctx, "SELECT ST_Area(ST_GeomFromText('POLYGON((0 0,0 2,3 2,3 0,0 0))'))::text")
	if err != nil {
		return fmt.Errorf("ST_Area on the %s endpoint: %w", le.role, err)
	}
	if area != "6" {
		return fmt.Errorf("ST_Area of a 3x2 rectangle on the %s endpoint is %s, want 6", le.role, area)
	}
	nearest, err := le.conn.QueryInt(ctx, "SELECT id FROM qa_places WHERE geom IS NOT NULL ORDER BY geom <-> ST_SetSRID(ST_MakePoint(-179.0, -84.0), 4326), id LIMIT 1")
	if err != nil {
		return fmt.Errorf("GiST nearest-neighbour scan on the %s endpoint: %w", le.role, err)
	}
	if nearest != 1 {
		return fmt.Errorf("the point nearest (-179,-84) on the %s endpoint is id %d, want 1", le.role, nearest)
	}
	x, err := le.conn.QueryInt(ctx, "SELECT round(ST_X(ST_Transform(ST_SetSRID(ST_MakePoint(1.0, 0.0), 4326), 3857)))::bigint")
	if err != nil {
		return fmt.Errorf("ST_Transform to 3857 on the %s endpoint (fails when PROJ_DATA does not reach proj.db): %w", le.role, err)
	}
	if x != 111319 {
		return fmt.Errorf("ST_Transform of lon 1 to EPSG:3857 on the %s endpoint gave x=%d, want 111319", le.role, x)
	}
	return nil
}
