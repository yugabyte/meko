// Extension availability is a shipping contract, and it broke in the field
// twice ("extension btree_gin is not available"). The expected set is pinned in
// config Go-side, never read back from the database: enumerating whatever
// pg_available_extensions happens to report cannot detect a missing extension.
package features

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// DefaultExtensionsBaked is the set built into the compute image (PG contrib,
// plus pgsentinel from compute/pgxn/vendor), which every endpoint must serve with
// no opt-in.
// test_decoding is deliberately absent: postgres.mk builds it, but it has no
// control file, so the only way to exercise it is to create a logical
// replication slot, which asserts working logical decoding over amp storage.
// That is a separate capability with no product commitment and no other test.
var DefaultExtensionsBaked = []string{
	"pg_prewarm", "pg_buffercache", "pg_visibility", "pageinspect",
	"pg_trgm", "btree_gin", "amcheck", "pg_stat_statements", "tsm_system_rows",
	"postgres_fdw", "pgsentinel",
}

// DefaultExtensionsCatalog is the externalized set, as catalogName:sqlName
// (they differ for pgvector, whose image ships the "vector" extension). Kept in
// step with deploy/extensions/extensions.yaml — but a name belongs here only
// once BOTH controllers this runs against serve it: the build pinned in
// configs/default.yaml (env.ampstack.controller_image), which every run without
// an image override deploys, and the latest release, which the report portal's
// daily-regression schedule overrides it with. require_catalog (on by default)
// turns "the controller does not know it" into a hard failure, which is the
// whole point. documentdb is
// absent by design: compute_ctl creates it in its own database, so the one
// database this test creates everything in cannot hold it.
var DefaultExtensionsCatalog = []string{"pgvector:vector", "pg_cron:pg_cron", "hexabridge:hexabridge", "age:age"}

// preloadCatalogExts are the catalog extensions that must be in
// shared_preload_libraries (the endpoint chart appends them), so they are
// created last.
var preloadCatalogExts = map[string]bool{"pg_cron": true, "hexabridge": true}

// Kubernetes serves ImageVolume as beta from 1.33 (the endpoint chart's volumes
// stanza states the same bound). Below it a kubelet brings the pod up with no
// mount and no API error, which is the one environment condition this test
// tolerates.
const (
	imageVolumeMajor = 1
	imageVolumeMinor = 33
)

func init() {
	core.Register(core.Test{
		Name:        "features/pg_extensions_matrix",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Assert every extension the release commits to shipping is available on an endpoint: the baked contrib set with no opt-in, plus the catalog set on a cluster opted into all of it, each created, version-matched and canary-queried, postgres_fdw over a loopback foreign server so the module is proven to connect and fetch rather than merely to install.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     20 * time.Minute,
		Run:         runPgExtensionsMatrix,
	})
}

// catalogExt is one externalized extension: its catalog name (the opt-in and
// image name) and its SQL name (CREATE EXTENSION / pg_extension.extname).
type catalogExt struct {
	catalog string
	sql     string
	preload bool
}

func runPgExtensionsMatrix(t *core.T) error {
	baked := t.Options.GetStringSlice("functional.extensions.baked", DefaultExtensionsBaked)
	catalog, err := parseCatalogExts(t.Options.GetStringSlice("functional.extensions.catalog", DefaultExtensionsCatalog))
	if err != nil {
		return err
	}
	requireCatalog := t.Options.GetBool("functional.extensions.require_catalog", false)
	record := func(name string, props map[string]any) {
		_ = t.Step(name, func(h *core.StepHandle) error {
			for k, v := range props {
				h.Set(k, v)
			}
			return nil
		})
	}
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"pg_extensions_matrix", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionsMatrixSteps(ctx, cl, a.proj, baked, catalog, requireCatalog,
				"extm_"+a.f.suffix(), a.f.endpointSQL, a.recordEndpointImage,
				a.endpointImageVolumeSupport, record, a.f.logf)
		}}}
	})
}

// recordEndpointImage logs the compute image the endpoint actually runs; the
// whole test only proves something about the image that was deployed.
// Best-effort: kubectl being unavailable is not a product signal.
func (a *ampFeatureRun) recordEndpointImage(ctx context.Context, endpointID string) {
	images, err := a.f.ampKube().GetSelector(ctx, "pod",
		"app.kubernetes.io/instance="+endpointDeployName(endpointID),
		"{.items[*].spec.containers[*].image}")
	if err != nil {
		a.f.logf("pg_extensions_matrix: could not read the endpoint pod's container image (best-effort): %v", err)
		return
	}
	a.f.logf("pg_extensions_matrix: endpoint %s pod images: %s", endpointID, strings.TrimSpace(images))
}

// endpointImageVolumeSupport reads the ImageVolume capability of the node running
// the endpoint pod, plus the image volume references the controller rendered into
// that pod. This is what tells an incapable cluster apart from a product
// regression that drops the opt-in.
func (a *ampFeatureRun) endpointImageVolumeSupport(ctx context.Context, endpointID string) (kubelet, volumes string, err error) {
	kube := a.f.ampKube()
	if kube == nil {
		return "", "", fmt.Errorf("no kubectl is available for the amp namespace")
	}
	selector := "app.kubernetes.io/instance=" + endpointDeployName(endpointID)
	volumes, verr := kube.GetSelector(ctx, "pod", selector, "{.items[*].spec.volumes[*].image.reference}")
	if verr != nil {
		volumes = "unreadable: " + verr.Error()
	}
	node, err := kube.GetSelector(ctx, "pod", selector, "{.items[0].spec.nodeName}")
	if err != nil {
		return "", volumes, fmt.Errorf("read the node of the endpoint pod (%s): %w", selector, err)
	}
	if strings.TrimSpace(node) == "" {
		return "", volumes, fmt.Errorf("no pod matched %s, so its node cannot be resolved", selector)
	}
	kubelet, err = kube.Get(ctx, "node", strings.TrimSpace(node), "{.status.nodeInfo.kubeletVersion}")
	if err != nil {
		return "", volumes, fmt.Errorf("read the kubelet version of node %s: %w", node, err)
	}
	return strings.TrimSpace(kubelet), strings.TrimSpace(volumes), nil
}

// imageVolumeCapableKubelet reports whether a kubelet version can mount an
// ImageVolume, and whether the version string was parseable at all.
func imageVolumeCapableKubelet(version string) (capable, known bool) {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, false
	}
	if major != imageVolumeMajor {
		return major > imageVolumeMajor, true
	}
	return minor >= imageVolumeMinor, true
}

// renderedImageVolumes describes what the controller put in the endpoint pod, so
// a failure says whether the opt-in was dropped before or after rendering.
func renderedImageVolumes(volumes string) string {
	if strings.TrimSpace(volumes) == "" {
		return "the endpoint pod carries no image volume at all, so the opt-in was accepted and then dropped before the pod spec was rendered"
	}
	return "the endpoint pod carries image volumes [" + volumes + "], so they were rendered but their extensions did not reach PostgreSQL"
}

// parseCatalogExts parses the catalogName:sqlName knob entries.
func parseCatalogExts(entries []string) ([]catalogExt, error) {
	out := make([]catalogExt, 0, len(entries))
	for _, e := range entries {
		name, sql, ok := strings.Cut(strings.TrimSpace(e), ":")
		if !ok || name == "" || sql == "" {
			return nil, fmt.Errorf("functional.extensions.catalog entry %q is not catalogName:sqlName", e)
		}
		out = append(out, catalogExt{catalog: name, sql: sql, preload: preloadCatalogExts[name]})
	}
	return out, nil
}

// extensionsMatrixSteps drives the assertions over the ampCtl and
// endpoint-connector seams. It never skips the test: a control plane with no
// extension catalog, or a cluster whose kubelet cannot mount ImageVolumes, drops
// the catalog half only, because the baked contrib set needs no catalog and must
// hold regardless. Every other reason the catalog extensions are missing is a
// product failure.
func extensionsMatrixSteps(ctx context.Context, cl ampCtl, proj ampCluster, baked []string, catalog []catalogExt,
	requireCatalog bool, table string, connect endpointConnector, recordImage func(context.Context, string),
	capability func(context.Context, string) (string, string, error), record func(string, map[string]any),
	logf func(string, ...any)) error {
	optIn := make([]string, 0, len(catalog))
	for _, e := range catalog {
		optIn = append(optIn, e.catalog)
	}
	catalogOK := len(catalog) > 0
	skipReason := ""
	if _, err := cl.AddBranchExtensionsAndWait(ctx, proj.poolID, proj.projID, proj.branchID, controller.AddBranchExtensionsRequest{
		Extensions: optIn,
	}); err != nil {
		if !extensionsUnavailable(err) {
			return fmt.Errorf("opt the branch into the catalog extensions [%s]: %w", strings.Join(optIn, " "), err)
		}
		if requireCatalog {
			return fmt.Errorf("this control plane rejects the catalog extension opt-in [%s] as unsupported, and functional.extensions.require_catalog demands the catalog half: the release commits to serving externalized extensions: %w",
				strings.Join(optIn, " "), err)
		}
		skipReason = "the control plane reports externalized extensions as unsupported: " + err.Error()
		logf("pg_extensions_matrix: this control plane has no extension catalog, so the catalog half [%s] is not asserted; the baked contrib set still is: %v",
			strings.Join(optIn, " "), err)
		catalogOK = false
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint after opting the branch into the catalog extensions: %w", err)
	}
	recordImage(ctx, ep.ID)

	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()

	avail, err := availableExtensions(ctx, c)
	if err != nil {
		return err
	}

	// Zero mounted catalog extensions is softened only where the kubelet provably
	// cannot mount an ImageVolume. On a capable cluster the same symptom is the
	// controller or the chart dropping the opt-in, which is what this test exists
	// to catch. A PARTIAL mount always falls through to the assertions below.
	if catalogOK && countAvailable(avail, catalogSQLNames(catalog)) == 0 {
		kubelet, volumes, cerr := capability(ctx, ep.ID)
		capable, known := imageVolumeCapableKubelet(kubelet)
		switch {
		case cerr != nil:
			return fmt.Errorf("none of the catalog extensions [%s] is available on an endpoint that opted into all of them, and this cluster's ImageVolume capability could not be verified (%v), so the missing extensions cannot be attributed to an old kubelet: %s. Available extensions were: %s",
				strings.Join(optIn, " "), cerr, renderedImageVolumes(volumes), strings.Join(sortedNames(avail), " "))
		case !known:
			return fmt.Errorf("none of the catalog extensions [%s] is available on an endpoint that opted into all of them, and the node's kubelet version %q does not parse, so the missing extensions cannot be attributed to a kubelet older than v%d.%d: %s. Available extensions were: %s",
				strings.Join(optIn, " "), kubelet, imageVolumeMajor, imageVolumeMinor, renderedImageVolumes(volumes), strings.Join(sortedNames(avail), " "))
		case capable:
			return fmt.Errorf("none of the catalog extensions [%s] is available on an endpoint that opted into all of them, on a node whose kubelet %s can mount ImageVolumes (beta since v%d.%d): %s. Available extensions were: %s",
				strings.Join(optIn, " "), kubelet, imageVolumeMajor, imageVolumeMinor, renderedImageVolumes(volumes), strings.Join(sortedNames(avail), " "))
		case requireCatalog:
			return fmt.Errorf("none of the catalog extensions [%s] is available: the node's kubelet %s predates ImageVolume support (beta since v%d.%d), and functional.extensions.require_catalog demands the catalog half be asserted: run this test on a cluster whose kubelet is at least v%d.%d",
				strings.Join(optIn, " "), kubelet, imageVolumeMajor, imageVolumeMinor, imageVolumeMajor, imageVolumeMinor)
		default:
			skipReason = fmt.Sprintf("the node's kubelet %s predates ImageVolume support (beta since v%d.%d)", kubelet, imageVolumeMajor, imageVolumeMinor)
			logf("pg_extensions_matrix: none of the catalog extensions [%s] mounted and %s, so the catalog half is not asserted", strings.Join(optIn, " "), skipReason)
			catalogOK = false
		}
	}
	record("extensions_scope", map[string]any{
		"catalog_asserted":    catalogOK,
		"catalog_extensions":  strings.Join(optIn, " "),
		"baked_extensions":    strings.Join(baked, " "),
		"catalog_skip_reason": skipReason,
	})

	want := expectedExtensions(baked, catalog, catalogOK)
	for _, e := range want {
		if _, ok := avail[e.sql]; !ok {
			return fmt.Errorf("extension %q is not available on this endpoint: pg_available_extensions does not list it, and the release commits to shipping it (%s). Available extensions were: %s",
				e.sql, extClass(e), strings.Join(sortedNames(avail), " "))
		}
	}

	for _, e := range want {
		create := "CREATE EXTENSION IF NOT EXISTS " + quoteExtIdent(e.sql)
		_, err := c.Exec(ctx, create)
		// compute_ctl creates the preloaded ones in the background after start, and
		// IF NOT EXISTS loses a concurrent create with a unique violation.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_, err = c.Exec(ctx, create)
		}
		if err != nil {
			return fmt.Errorf("CREATE EXTENSION %s failed on an endpoint where pg_available_extensions lists it at version %s (%s): %w",
				e.sql, avail[e.sql], extClass(e), err)
		}
		installed, err := c.QueryString(ctx, "SELECT coalesce(max(extversion), '') FROM pg_extension WHERE extname = $1", e.sql)
		if err != nil {
			return fmt.Errorf("read pg_extension.extversion for %s: %w", e.sql, err)
		}
		if installed != avail[e.sql] {
			return fmt.Errorf("extension %s installed version %q, want %q: pg_available_extensions advertises %q as the default version, so the SQL scripts the image ships do not match its control file",
				e.sql, installed, avail[e.sql], avail[e.sql])
		}
	}

	return extensionCanaries(ctx, c, table)
}

// availableExtensions reads pg_available_extensions into a Go map of name to
// default_version. One aggregated row keeps this on the QueryString seam.
func availableExtensions(ctx context.Context, c sqlConn) (map[string]string, error) {
	agg, err := c.QueryString(ctx,
		"SELECT coalesce(string_agg(name || '=' || coalesce(default_version, ''), ' ' ORDER BY name), '') FROM pg_available_extensions")
	if err != nil {
		return nil, fmt.Errorf("read pg_available_extensions: %w", err)
	}
	out := map[string]string{}
	for _, field := range strings.Fields(agg) {
		name, version, _ := strings.Cut(field, "=")
		out[name] = version
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pg_available_extensions is empty on this endpoint: the compute image ships no extension control files at all")
	}
	return out, nil
}

// expectedExtension is one pinned name with the class that must supply it.
type expectedExtension struct {
	sql     string
	catalog string // "" for a baked contrib
	preload bool
}

func expectedExtensions(baked []string, catalog []catalogExt, catalogOK bool) []expectedExtension {
	out := make([]expectedExtension, 0, len(baked)+len(catalog))
	for _, n := range baked {
		out = append(out, expectedExtension{sql: n})
	}
	if catalogOK {
		for _, e := range catalog {
			out = append(out, expectedExtension{sql: e.sql, catalog: e.catalog, preload: e.preload})
		}
	}
	// A preload extension needs shared_preload_libraries and is created last.
	sort.SliceStable(out, func(i, j int) bool { return !out[i].preload && out[j].preload })
	return out
}

func extClass(e expectedExtension) string {
	if e.catalog == "" {
		return "baked into the compute image (postgres.mk contrib or compute/pgxn), no opt-in"
	}
	return "catalog extension " + e.catalog + ", mounted because the cluster opted in"
}

func catalogSQLNames(catalog []catalogExt) []string {
	out := make([]string, 0, len(catalog))
	for _, e := range catalog {
		out = append(out, e.sql)
	}
	return out
}

func countAvailable(avail map[string]string, names []string) int {
	n := 0
	for _, name := range names {
		if _, ok := avail[name]; ok {
			n++
		}
	}
	return n
}

func sortedNames(avail map[string]string) []string {
	out := make([]string, 0, len(avail))
	for name := range avail {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func quoteExtIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// extensionCanaries runs one REAL call per baked extension, because "created and
// version-matched" proves the SQL scripts, not the library or the storage read
// path behind it. Catalog extensions get their depth in their own tests
// (amp_extension_pgvector / _pg_cron / _hexabridge / _age); the matrix stays the
// availability contract.
func extensionCanaries(ctx context.Context, c sqlConn, table string) error {
	if _, err := c.Exec(ctx, "CREATE TABLE "+table+" (id int primary key, status int)"); err != nil {
		return fmt.Errorf("create the canary table: %w", err)
	}
	for id := 1; id <= 20; id++ {
		if _, err := c.Exec(ctx, "INSERT INTO "+table+" (id, status) VALUES ($1, $2)", id, id%4); err != nil {
			return fmt.Errorf("seed the canary table: %w", err)
		}
	}
	if _, err := c.Exec(ctx, "CREATE INDEX "+table+"_gin ON "+table+" USING gin (status)"); err != nil {
		return fmt.Errorf("a GIN index over an int column needs btree_gin's int4 operator class, and the release commits to shipping btree_gin: %w", err)
	}
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+table+" WHERE status = 1")
	if err != nil {
		return fmt.Errorf("query the btree_gin canary index: %w", err)
	}
	if n != 5 {
		return fmt.Errorf("the btree_gin canary query counted %d rows with status = 1, want 5 out of the 20 seeded (ids 1,5,9,13,17)", n)
	}

	sim, err := c.QueryInt(ctx, "SELECT (similarity('abc','abd') > 0 AND similarity('abc','abd') < 1)::int")
	if err != nil {
		return fmt.Errorf("pg_trgm's similarity() is not callable, and the release commits to shipping pg_trgm: %w", err)
	}
	if sim != 1 {
		return fmt.Errorf("similarity('abc','abd') is not strictly between 0 and 1: pg_trgm's trigram similarity is not computing")
	}

	// pg_stat_statements only serves its view when the library is in
	// shared_preload_libraries — installable-but-not-preloaded errors here.
	tracked, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_stat_statements")
	if err != nil {
		return fmt.Errorf("pg_stat_statements is installed but its view is not readable — the library is not in shared_preload_libraries, and every endpoint commits to preloading it: %w", err)
	}
	if tracked == 0 {
		return fmt.Errorf("pg_stat_statements tracked 0 statements on a connection that has been executing SQL — the preloaded library is not recording")
	}

	// A readable view proves the preload and rows prove the sampler is alive. It
	// records only `active` sessions, about once a second but later when its own
	// query is slow, so stay active and poll rather than trust one window.
	var ash int64
	for attempt := 0; attempt < 5 && ash == 0; attempt++ {
		if _, err := c.Exec(ctx, "SELECT pg_sleep(2)"); err != nil {
			return fmt.Errorf("pg_sleep before reading pgsentinel's view: %w", err)
		}
		ash, err = c.QueryInt(ctx, "SELECT count(*) FROM pg_active_session_history")
		if err != nil {
			return fmt.Errorf("pg_active_session_history is not readable, so pgsentinel is not in shared_preload_libraries: %w", err)
		}
	}
	if ash == 0 {
		return fmt.Errorf("pg_active_session_history is empty after 10s of an active session: pgsentinel's sampler is not recording")
	}

	// The observability/maintenance contribs each do one real call over the
	// canary table. pageinspect's get_raw_page and amcheck's bt_index_check are
	// the interesting ones on amp: both read pages through the storage stack.
	if _, err := c.QueryInt(ctx, "SELECT pg_prewarm($1)::bigint", table); err != nil {
		return fmt.Errorf("pg_prewarm over the canary table: %w", err)
	}
	if _, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_buffercache"); err != nil {
		return fmt.Errorf("pg_buffercache view: %w", err)
	}
	if _, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_visibility_map($1)", table); err != nil {
		return fmt.Errorf("pg_visibility_map over the canary table: %w", err)
	}
	if tuples, err := c.QueryInt(ctx, "SELECT count(*) FROM heap_page_items(get_raw_page($1, 0))", table); err != nil {
		return fmt.Errorf("pageinspect heap_page_items(get_raw_page(...)) — a raw page read through amp storage: %w", err)
	} else if tuples == 0 {
		return fmt.Errorf("pageinspect read page 0 of the canary table as empty, but 20 rows were just inserted — the raw-page read path is returning a wrong page")
	}
	if _, err := c.Exec(ctx, "SELECT bt_index_check(($1 || '_pkey')::regclass)", table); err != nil {
		return fmt.Errorf("amcheck bt_index_check over the canary table's primary key: %w", err)
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+table+" TABLESAMPLE SYSTEM_ROWS(5)"); err != nil {
		return fmt.Errorf("tsm_system_rows TABLESAMPLE over the canary table, which documentdb depends on: %w", err)
	} else if n != 5 {
		return fmt.Errorf("TABLESAMPLE SYSTEM_ROWS(5) returned %d rows out of 20, want exactly 5", n)
	}

	return fdwLoopbackCanary(ctx, c, table)
}

// fdwLoopbackCanary reads the canary table back over a foreign server on the
// compute's own postmaster. No password: pg_hba trusts 127.0.0.1 and the role is
// the compute's superuser, which is what exempts it from fdw's password rule.
func fdwLoopbackCanary(ctx context.Context, c sqlConn, table string) error {
	server, foreign := table+"_fdw_srv", table+"_fdw_ft"
	// From the session, not assumed, and interpolated because an OPTION takes a
	// literal only: the compute does not serve Postgres on 5432.
	port, err := c.QueryString(ctx, "SELECT current_setting('port')")
	if err != nil {
		return fmt.Errorf("read the compute's port for the foreign server: %w", err)
	}
	dbname, err := c.QueryString(ctx, "SELECT current_database()")
	if err != nil {
		return fmt.Errorf("read the current database for the foreign server: %w", err)
	}
	user, err := c.QueryString(ctx, "SELECT current_user")
	if err != nil {
		return fmt.Errorf("read the current user for the foreign server: %w", err)
	}
	if _, err := c.Exec(ctx,
		"CREATE SERVER "+server+" FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host '127.0.0.1', port '"+port+"', dbname '"+dbname+"')"); err != nil {
		return fmt.Errorf("create a postgres_fdw foreign server: %w", err)
	}
	defer func() { _, _ = c.Exec(ctx, "DROP SERVER IF EXISTS "+server+" CASCADE") }()
	if _, err := c.Exec(ctx, "CREATE USER MAPPING FOR CURRENT_USER SERVER "+server+" OPTIONS (user '"+user+"')"); err != nil {
		return fmt.Errorf("create the postgres_fdw user mapping: %w", err)
	}
	if _, err := c.Exec(ctx, "CREATE FOREIGN TABLE "+foreign+" (id int, status int) SERVER "+server+" OPTIONS (table_name '"+table+"')"); err != nil {
		return fmt.Errorf("create a foreign table over the canary table: %w", err)
	}
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+foreign)
	if err != nil {
		return fmt.Errorf("postgres_fdw could not read the canary table back over a loopback foreign server. The extension is installed and the server is defined, so this is the module failing to connect or to fetch: %w", err)
	}
	if n != 20 {
		return fmt.Errorf("postgres_fdw read %d rows from the canary table over a loopback foreign server, want the 20 that were inserted", n)
	}
	// A restriction as well as a bare count.
	filtered, err := c.QueryInt(ctx, "SELECT count(*) FROM "+foreign+" WHERE status = 1")
	if err != nil {
		return fmt.Errorf("postgres_fdw could not run a qualified read over the loopback foreign server: %w", err)
	}
	if filtered != 5 {
		return fmt.Errorf("postgres_fdw counted %d foreign rows with status = 1, want 5 of the 20 seeded", filtered)
	}
	return nil
}
