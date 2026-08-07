package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/proc"
)

// endpointBasePort is the compute port-allocation base (Rust
// ComputeControlPlane::base_port).
const endpointBasePort uint16 = 55431

// endpointDir is where an endpoint's local process state lives: its pgdata, its
// co-located daemons' data dirs, its compute_ctl pidfile. `endpoint stop
// --destroy` removes it wholesale.
func endpointDir(env *config.LocalEnv, endpointID string) string {
	return filepath.Join(env.EndpointsPath(), endpointID)
}

// endpointConf is how to run this endpoint's local compute: which ports to bind,
// which pg binaries to exec, what to put in the compute spec. The controller has
// no use for any of it, so it rides in the endpoint's `extended_config` — the
// product API's opaque per-endpoint passthrough, stored verbatim and never
// interpreted. Only the Primary path is written.
//
// What is NOT here is anything the control plane already models: the endpoint's
// id and name are the row's, and the branch and pg version come from the branch.
type endpointConf struct {
	EndpointID                   string               `json:"endpoint_id"`
	BranchID                     string               `json:"branch_id"`
	Mode                         string               `json:"mode"`
	PgPort                       uint16               `json:"pg_port"`
	ExternalHTTPPort             uint16               `json:"external_http_port"`
	InternalHTTPPort             uint16               `json:"internal_http_port"`
	PgVersion                    int                  `json:"pg_version"`
	Grpc                         bool                 `json:"grpc"`
	SkipPgCatalogUpdates         bool                 `json:"skip_pg_catalog_updates"`
	ReconfigureConcurrency       int                  `json:"reconfigure_concurrency"`
	DropSubscriptionsBeforeStart bool                 `json:"drop_subscriptions_before_start"`
	Features                     []string             `json:"features"`
	Cluster                      *cfg.Cluster         `json:"cluster"`
	ComputeCtlConfig             cfg.ComputeCtlConfig `json:"compute_ctl_config"`
	PrivilegedRoleName           *string              `json:"privileged_role_name"`
	SharedBuffers                *string              `json:"shared_buffers"`
	LfcSize                      *string              `json:"lfc_size"`
}

// launcherEndpoint is one of the launcher's endpoints: the control plane's row
// plus the local run config it carries in extended_config.
type launcherEndpoint struct {
	// ID is the row's id, the key every product-API call takes.
	ID uuid.UUID
	// Pool and Project locate the endpoint's branch, which is what the nested
	// endpoint routes are addressed by.
	Pool, Project, Branch uuid.UUID
	conf                  endpointConf
	pgSettings            map[string]string
}

// launcherEndpoints is every endpoint the launcher owns. It keeps the full list
// as well as the by-name index, because the two answer different questions and
// only one of them can be keyed by name.
type launcherEndpoints struct {
	// all is every launcher endpoint across every project, in load order. Port
	// allocation reads THIS, not byName: an endpoint left out of the index still
	// holds its ports, and handing them to a new compute would collide.
	all []launcherEndpoint
	// byName resolves the handle the CLI and the harness use. Names are unique
	// only within a branch (endpoints_name_per_branch), so a name used on two
	// branches lands here once with the ambiguity recorded below.
	byName map[string]launcherEndpoint
	// ambiguous holds the names that appear on more than one branch. Resolving
	// one is an error rather than a coin flip: `endpoint start ep_b1` naming two
	// different computes has no right answer, and picking whichever row loaded
	// last would start (or destroy) the wrong one.
	ambiguous map[string]int
}

// lookup resolves a handle, refusing an ambiguous one.
func (l launcherEndpoints) lookup(name string) (launcherEndpoint, error) {
	if n := l.ambiguous[name]; n > 1 {
		return launcherEndpoint{}, fmt.Errorf(
			"endpoint %q exists on %d different branches; the launcher addresses endpoints by name, so give it a unique one", name, n)
	}
	le, ok := l.byName[name]
	if !ok {
		return launcherEndpoint{}, fmt.Errorf("endpoint %s not found", name)
	}
	return le, nil
}

// loadEndpoints returns every endpoint the launcher owns. Endpoints span
// projects (a lineage is a project), so this walks all of them.
//
// An endpoint with no extended_config is not the launcher's: something else
// created it against the same controller. Skipped rather than failing the scan.
func loadEndpoints(ctx context.Context, env *config.LocalEnv) (launcherEndpoints, error) {
	out := launcherEndpoints{byName: map[string]launcherEndpoint{}, ambiguous: map[string]int{}}
	b, err := newBranchAPI(env)
	if err != nil {
		return out, err
	}
	branches, err := b.allBranches(ctx)
	if err != nil {
		return out, err
	}
	for _, br := range branches {
		eps, err := b.api.ListEndpoints(ctx, b.poolID, br.ProjectId, br.BranchId)
		if err != nil {
			return out, err
		}
		for _, ep := range eps {
			if ep.Name == "" || ep.ExtendedConfig == nil {
				continue
			}
			raw, err := json.Marshal(*ep.ExtendedConfig)
			if err != nil {
				return out, fmt.Errorf("re-encoding extended_config for endpoint %s: %w", ep.Name, err)
			}
			var conf endpointConf
			if err := json.Unmarshal(raw, &conf); err != nil {
				return out, fmt.Errorf("deserializing extended_config for endpoint %s: %w", ep.Name, err)
			}
			var settings map[string]string
			if ep.PgSettings != nil {
				settings = *ep.PgSettings
			}
			le := launcherEndpoint{
				ID: ep.Id, Pool: b.poolID, Project: br.ProjectId, Branch: br.BranchId,
				conf: conf, pgSettings: settings,
			}
			out.all = append(out.all, le)
			out.ambiguous[ep.Name]++
			if _, seen := out.byName[ep.Name]; !seen {
				out.byName[ep.Name] = le
			}
		}
	}
	return out, nil
}

// nextCandidatePort is the starting point for compute port allocation: one
// above the highest port any existing endpoint uses (Rust
// `ComputeControlPlane::next_candidate_port`).
func nextCandidatePort(endpoints launcherEndpoints) uint16 {
	var max uint16
	found := false
	// Over `all`, not the by-name index: an endpoint whose name collides with
	// another's is missing from the index but still holds its ports.
	for _, ep := range endpoints.all {
		for _, p := range []uint16{ep.conf.PgPort, ep.conf.ExternalHTTPPort, ep.conf.InternalHTTPPort} {
			if p > max {
				max = p
			}
		}
		found = true
	}
	if !found {
		max = endpointBasePort
	}
	return max + 1
}

// findFreePort returns the first bindable localhost port >= start (Rust
// `ComputeControlPlane::find_free_port`).
func findFreePort(start uint16) (uint16, error) {
	for port := int(start); port <= 65535; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = l.Close()
			return uint16(port), nil
		}
	}
	return 0, fmt.Errorf("no free port found in range [%d, 65535]", start)
}

// EndpointCreateArgs are the flags of `endpoint create`.
type EndpointCreateArgs struct {
	EndpointID       string // optional positional; defaults to "ep-<branch_id>"
	BranchID         string // required: the branch to place the endpoint on
	PgVersion        int
	PgPort           *uint16
	ExternalHTTPPort *uint16
	InternalHTTPPort *uint16
	LSN              string      // Static mode not supported in the minimal port
	ConfigOnly       bool        // accepted for compat; ignored (Rust ignores it too)
	PgSettings       [][2]string // repeatable --pg-setting NAME=VALUE
}

// EndpointCreate provisions the branch's pageserver + safekeeper pair and this
// endpoint's daemons, then records the endpoint in the controller.
// Mirrors the `EndpointCmd::Create` arm of Rust `handle_endpoint`. The pair +
// pair members are started sequentially here (the Rust concurrency is a
// latency optimization, not a correctness requirement).
func EndpointCreate(ctx context.Context, env *config.LocalEnv, args EndpointCreateArgs) error {
	if args.LSN != "" {
		return fmt.Errorf("static (--lsn) endpoints are not supported in this build; only Primary mode")
	}
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}

	// A branch is addressed by id, never by name: names are unique only within a
	// project, and the launcher spans several (a lineage is a project).
	branchID := config.BranchID(args.BranchID)
	if branchID == "" {
		return fmt.Errorf("`endpoint create` requires --branch-id")
	}
	endpointID := args.EndpointID
	if endpointID == "" {
		endpointID = "ep-" + string(branchID)
	}

	// The endpoint hangs off this branch, and the branch says which project and
	// pool to address it in: a lineage IS a project, so there is nothing ambient
	// to fall back on.
	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	branch, err := b.resolveBranch(ctx, branchID)
	if err != nil {
		return err
	}
	if branch == nil {
		return fmt.Errorf("no live branch %s in any of the launcher's projects", branchID)
	}

	// Primary mode only; conflict check unless allow_multiple (not exposed).
	if err := checkConflictingEndpoints(endpoints, branchID); err != nil {
		return err
	}

	// Provision the branch's pair (consumes the pending init). On its own
	// failure it cleans up the daemons it started (provisionBootstrap/Branch);
	// on success they are recorded in env below.
	if err := provisionTimelineForEndpoint(ctx, env, endpointID, branchID); err != nil {
		return err
	}

	// The pair is committed to env now. On any failure past this point the
	// config mutation is discarded and a retry reuses these ports, so stop the
	// co-located daemons (the pair) to avoid orphans. stop* on a
	// not-yet-started daemon is a no-op.
	success := false
	defer func() {
		if success {
			return
		}
		endpointDir := endpointDir(env, endpointID)
		for i := range env.Safekeepers {
			if env.Safekeepers[i].EndpointID == endpointID {
				_ = stopSafekeeper(env, &env.Safekeepers[i], true)
			}
		}
		for i := range env.Pageservers {
			if strings.HasPrefix(env.Pageservers[i].DataDir, endpointDir) {
				_ = stopPageserver(&env.Pageservers[i], true)
			}
		}
	}()

	// Record the endpoint: create its controller row, then persist its local
	// config next to the process it describes.
	if err := newEndpoint(ctx, env, b, branch, endpoints, endpointID, args); err != nil {
		return err
	}
	success = true
	return nil
}

// newEndpoint allocates the endpoint's compute ports, creates its row in the
// control plane, and writes its local config to disk.
//
// The row comes first. It is what claims the branch's single-writer slot, so a
// duplicate primary is refused by the schema before any local state exists; and
// it is what mints the endpoint id the config has to carry.
func newEndpoint(ctx context.Context, env *config.LocalEnv, b *branchAPI, branch *api.Branch, endpoints launcherEndpoints, endpointID string, args EndpointCreateArgs) error {
	branchID := config.BranchID(hex32UUID(branch.BranchId))
	pgPort, err := resolvePort(args.PgPort, func() (uint16, error) {
		return findFreePort(nextCandidatePort(endpoints))
	})
	if err != nil {
		return err
	}
	extPort, err := resolvePort(args.ExternalHTTPPort, func() (uint16, error) {
		return findFreePort(pgPort + 1)
	})
	if err != nil {
		return err
	}
	intPort, err := resolvePort(args.InternalHTTPPort, func() (uint16, error) {
		return findFreePort(extPort + 1)
	})
	if err != nil {
		return err
	}

	pub, err := env.ReadPublicKey()
	if err != nil {
		return err
	}
	pgVersion := args.PgVersion
	if pgVersion == 0 {
		pgVersion = config.DefaultPgVersion
	}

	conf := endpointConf{
		EndpointID:                   endpointID,
		BranchID:                     branchID,
		Mode:                         cfg.ComputeModePrimary,
		PgPort:                       pgPort,
		ExternalHTTPPort:             extPort,
		InternalHTTPPort:             intPort,
		PgVersion:                    pgVersion,
		Grpc:                         false,
		SkipPgCatalogUpdates:         true, // minimal path never runs catalog updates
		ReconfigureConcurrency:       1,
		DropSubscriptionsBeforeStart: false,
		Features:                     []string{},
		Cluster:                      nil,
		ComputeCtlConfig: cfg.ComputeCtlConfig{
			Jwks: cfg.BuildJWKS(pub),
			Tls:  nil,
		},
	}

	// The row carries everything: the handle as its name, the run config as its
	// extended_config, and the caller's --pg-setting overrides as its pg_settings.
	// There is nothing left for the launcher to keep on the side.
	extended, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("serializing endpoint config: %w", err)
	}
	task, err := b.api.CreateEndpoint(ctx, b.poolID, branch.ProjectId, branch.BranchId, client.EndpointCreate{
		Name:       endpointID,
		Extended:   extended,
		PgSettings: pgSettingsMap(args.PgSettings),
	})
	if err != nil {
		return fmt.Errorf("creating endpoint in controller: %w", err)
	}
	if err := awaitTask(ctx, b.api, task.Id, endpointTaskTimeout); err != nil {
		return fmt.Errorf("create endpoint %s: %w", endpointID, err)
	}
	return nil
}

// effectivePgSettings resolves what this endpoint's compute actually runs with:
// its branch's settings merged with its own, its own winning. Sorted by name so
// the generated postgresql.conf is byte-stable across runs — Go map iteration is
// not ordered, and an unstable config file would make every diff noise.
func effectivePgSettings(ctx context.Context, env *config.LocalEnv, le launcherEndpoint) ([][2]string, error) {
	b, err := newBranchAPI(env)
	if err != nil {
		return nil, err
	}
	branch, err := b.api.GetBranch(ctx, b.poolID, le.Project, le.Branch)
	if err != nil {
		return nil, err
	}
	var branchSettings map[string]string
	if branch.PgSettings != nil {
		branchSettings = *branch.PgSettings
	}
	var endpointSettings map[string]string
	if le.pgSettings != nil {
		endpointSettings = le.pgSettings
	}
	merged := make(map[string]string, len(branchSettings)+len(endpointSettings))
	for k, v := range branchSettings {
		merged[k] = v
	}
	for k, v := range endpointSettings {
		merged[k] = v
	}
	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, k := range names {
		out = append(out, [2]string{k, merged[k]})
	}
	return out, nil
}

// pgSettingsMap turns the repeatable --pg-setting flag into the object the API
// takes. A name given twice keeps the last value, matching how postgresql.conf
// resolves a repeated setting.
func pgSettingsMap(pairs [][2]string) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		out[kv[0]] = kv[1]
	}
	return out
}

func resolvePort(explicit *uint16, alloc func() (uint16, error)) (uint16, error) {
	if explicit != nil {
		return *explicit, nil
	}
	return alloc()
}

// checkConflictingEndpoints refuses a duplicate live Primary endpoint on the
// branch. Mirrors Rust `ComputeControlPlane::check_conflicting_endpoints`
// (Primary arm). The launcher only creates Primary endpoints.
func checkConflictingEndpoints(endpoints launcherEndpoints, branchID config.BranchID) error {
	for _, ep := range endpoints.all {
		id := ep.conf.EndpointID
		if ep.conf.BranchID == branchID && ep.conf.Mode == cfg.ComputeModePrimary {
			return fmt.Errorf(
				"attempting to create a duplicate primary endpoint on branch %s: endpoint %q exists already", branchID, id)
		}
	}
	return nil
}

// EndpointStart builds the compute config, spawns yb-compute-ctl, and waits
// for the compute to reach Running. Mirrors the `EndpointCmd::Start` arm of
// Rust `handle_endpoint` + `Endpoint::start`.
func EndpointStart(ctx context.Context, env *config.LocalEnv, endpointID string, timeout time.Duration) error {
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}
	le, err := endpoints.lookup(endpointID)
	if err != nil {
		return err
	}
	conf := le.conf
	// The compute's settings are the branch's baseline with this endpoint's own
	// merged over it — the same rule the deployed path applies (core.EffectivePgSettings).
	pgSettings, err := effectivePgSettings(ctx, env, le)
	if err != nil {
		return err
	}

	psConf := env.PageserverConfForBranch(conf.BranchID)
	if psConf == nil {
		return fmt.Errorf("no pageserver serves branch %s", conf.BranchID)
	}

	// Trust path: no storage auth token for the compute→pageserver link.
	var storageAuthToken *string

	connInfo, err := pageserverConnInfo(psConf, conf.Grpc, env.UseLocalTLS)
	if err != nil {
		return err
	}
	var pageserverConnstring *string
	if len(connInfo.Pageservers) > 0 && connInfo.Pageservers[0].LibpqURL != nil {
		s := *connInfo.Pageservers[0].LibpqURL
		pageserverConnstring = &s
	}

	// The branch's safekeepers become the spec's safekeeper connstrings.
	branchSks := safekeepersForTimeline(env, conf.BranchID)
	var safekeeperConnstrings []string
	for _, sk := range branchSks {
		sk := sk
		safekeeperConnstrings = append(safekeeperConnstrings, fmt.Sprintf("127.0.0.1:%d", sk.GetComputePort()))
	}

	// postgresql.conf: walproposer is wired to ALL env safekeepers'
	// compute ports (Rust `setup_pg_conf` reads `self.env.safekeepers`).
	var allSkPorts []uint16
	for _, sk := range env.Safekeepers {
		sk := sk
		allSkPorts = append(allSkPorts, sk.GetComputePort())
	}
	pgConf := cfg.BuildPostgresqlConf(cfg.PostgresqlConfParams{
		SharedBuffers:          conf.SharedBuffers,
		ListenAddr:             "127.0.0.1",
		Port:                   conf.PgPort,
		UseLocalTLS:            env.UseLocalTLS,
		LfcSize:                conf.LfcSize,
		PgSettings:             pgSettings,
		SafekeeperComputePorts: allSkPorts,
	})

	computeConfig, err := cfg.BuildComputeConfig(cfg.ComputeSpecParams{
		PostgresqlConf:               pgConf,
		CreateTestUser:               false,
		StorageBranchID:              conf.BranchID,
		EndpointID:                   endpointID,
		Mode:                         cfg.ComputeModePrimary,
		PageserverConnInfo:           connInfo,
		PageserverConnstring:         pageserverConnstring,
		SafekeeperConnstrings:        safekeeperConnstrings,
		StorageAuthToken:             storageAuthToken,
		SkipPgCatalogUpdates:         conf.SkipPgCatalogUpdates,
		DropSubscriptionsBeforeStart: conf.DropSubscriptionsBeforeStart,
		ReconfigureConcurrency:       conf.ReconfigureConcurrency,
		// The LFC state store, so a prewarming compute can address its blob.
		// Passed unconditionally: BuildComputeConfig only derives the store when
		// prewarm is actually enabled.
		YbConn:           env.Controller.Yb,
		Features:         nil,
		ComputeCtlConfig: conf.ComputeCtlConfig,
	})
	if err != nil {
		return err
	}

	return spawnComputeCtl(ctx, env, &conf, computeConfig, timeout)
}

// pageserverConnInfo builds the compute's pageserver connection block from a
// PageServerConf, mirroring Rust `local_pageserver_conf_to_conn_info`.
func pageserverConnInfo(conf *config.PageServerConf, grpc, useTLS bool) (cfg.PageserverConnectionInfo, error) {
	host, err := hostOf(conf.ListenPgAddr)
	if err != nil {
		return cfg.PageserverConnectionInfo{}, err
	}
	port, err := portOf(conf.ListenPgAddr)
	if err != nil {
		return cfg.PageserverConnectionInfo{}, err
	}
	sslmode := "?sslmode=disable"
	if useTLS {
		sslmode = ""
	}
	libpq := fmt.Sprintf("postgres://no_user@%s:%d%s", host, port, sslmode)
	item := cfg.PageserverConnectionItem{LibpqURL: &libpq}
	if conf.ListenGRPCAddr != nil {
		gh, err := hostOf(*conf.ListenGRPCAddr)
		if err != nil {
			return cfg.PageserverConnectionInfo{}, err
		}
		gp, err := portOf(*conf.ListenGRPCAddr)
		if err != nil {
			return cfg.PageserverConnectionInfo{}, err
		}
		grpcURL := fmt.Sprintf("grpc://no_user@%s:%d", gh, gp)
		item.GrpcURL = &grpcURL
	}
	prefer := cfg.PageserverProtocolLibpq
	if grpc {
		prefer = cfg.PageserverProtocolGrpc
	}
	return cfg.PageserverConnectionInfo{
		Pageservers:    []cfg.PageserverConnectionItem{item},
		PreferProtocol: prefer,
	}, nil
}

// computeJWT mints the compute_ctl status token (Rust `generate_jwt(None)`:
// compute_id set, scope + aud null).
func computeJWT(env *config.LocalEnv, endpointID string) (string, error) {
	return env.GenerateAuthToken(jwt.MapClaims{
		"compute_id": endpointID,
		"scope":      nil,
		"aud":        nil,
	})
}

// spawnComputeCtl writes config.json, launches yb-compute-ctl (hand-rolled
// pidfile, full inherited env — unlike the storage daemons), and polls
// /status until Running. Mirrors the process-launch half of Rust
// `Endpoint::start`.
func spawnComputeCtl(ctx context.Context, env *config.LocalEnv, conf *endpointConf, computeConfig cfg.ComputeConfig, timeout time.Duration) error {
	endpointDir := endpointDir(env, conf.EndpointID)
	if err := os.MkdirAll(endpointDir, 0o755); err != nil {
		return fmt.Errorf("create endpoint dir %q: %w", endpointDir, err)
	}
	pgdata := filepath.Join(endpointDir, "pgdata")
	if _, err := os.Stat(pgdata); err == nil {
		if err := os.RemoveAll(pgdata); err != nil {
			return fmt.Errorf("remove stale pgdata %q: %w", pgdata, err)
		}
	}

	configPath := filepath.Join(endpointDir, "config.json")
	if err := cfg.WriteComputeConfigJSON(configPath, computeConfig); err != nil {
		return err
	}

	logFile, err := os.OpenFile(filepath.Join(endpointDir, "compute.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open compute.log: %w", err)
	}
	defer logFile.Close()

	connStr := fmt.Sprintf("postgresql://cloud_admin@127.0.0.1:%d/postgres", conf.PgPort)
	pgBin := filepath.Join(env.PgBinDir(conf.PgVersion), "postgres")
	args := []string{
		"--external-http-port", strconv.Itoa(int(conf.ExternalHTTPPort)),
		"--internal-http-port", strconv.Itoa(int(conf.InternalHTTPPort)),
		"--pgdata", pgdata,
		"--connstr", connStr,
		"--config", configPath,
		"--pgbin", pgBin,
		"--compute-id", conf.EndpointID,
	}
	if conf.PrivilegedRoleName != nil {
		args = append(args, "--privileged-role-name", *conf.PrivilegedRoleName)
	}

	cmd := exec.Command(filepath.Join(env.YbAmpDistribDir, "yb-compute-ctl"), args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// compute_ctl inherits the launcher's full environment (Rust does NOT
	// env_clear it, unlike the storage daemons).
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn yb-compute-ctl: %w", err)
	}
	pid := cmd.Process.Pid

	// Record the pid so `endpoint stop` can find it (raw pidfile, no flock).
	if err := os.WriteFile(filepath.Join(endpointDir, "compute_ctl.pid"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("write compute_ctl.pid: %w", err)
	}

	token, err := computeJWT(env, conf.EndpointID)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	// external_http_address binds 0.0.0.0; connect there like Rust does.
	statusClient := client.NewComputeCtl(fmt.Sprintf("0.0.0.0:%d", conf.ExternalHTTPPort), token, httpClient())

	deadline := time.Now().Add(timeout)
	for {
		resp, err := statusClient.Status(ctx)
		if err == nil {
			// ComputeStatus is #[serde(rename_all = "snake_case")], so the wire
			// values are lowercase ("running"/"failed"/"init"/…), not "Running".
			switch resp.Status {
			case "running":
				return nil
			case "failed":
				_ = cmd.Process.Kill()
				msg := "<no error from compute_ctl>"
				if resp.Error != nil {
					msg = *resp.Error
				}
				return fmt.Errorf("compute startup failed: %s", msg)
			default:
				// Init / Empty / Configuration* / Terminated: keep polling.
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			if err != nil {
				return fmt.Errorf("timed out %s waiting to connect to compute_ctl HTTP: %w", timeout, err)
			}
			return fmt.Errorf("compute startup timed out %s; still not Running", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// EndpointStopMode selects how `endpoint stop` terminates the compute.
type EndpointStopMode string

const (
	// StopModeFast: pg_ctl stop -m fast.
	StopModeFast EndpointStopMode = "fast"
	// StopModeImmediate: pg_ctl stop -m immediate.
	StopModeImmediate EndpointStopMode = "immediate"
	// StopModeImmediateTerminate: compute_ctl /terminate?mode=immediate.
	StopModeImmediateTerminate EndpointStopMode = "immediate-terminate"
)

// EndpointStop stops an endpoint's compute and prints the termination LSN (or
// "null"). Mirrors the `EndpointCmd::Stop` arm of Rust `handle_endpoint` +
// `Endpoint::stop` (without the --destroy teardown, which the minimal port
// does not expose).
func EndpointStop(ctx context.Context, env *config.LocalEnv, endpointID string, mode EndpointStopMode, destroy bool) error {
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}
	le, err := endpoints.lookup(endpointID)
	if err != nil {
		return err
	}
	return stopEndpoint(ctx, env, le, mode, destroy)
}

// stopEndpoint is EndpointStop on an endpoint the caller has ALREADY resolved.
// The teardown paths (BranchDrop, StopAll) walk the full endpoint list and must
// act on each row they hold, so they cannot go back through name resolution —
// a name shared by two branches is ambiguous, and refusing to tear down the very
// endpoint you are holding would be absurd.
func stopEndpoint(ctx context.Context, env *config.LocalEnv, le launcherEndpoint, mode EndpointStopMode, destroy bool) error {
	conf := le.conf
	endpointID := conf.EndpointID
	endpointDir := endpointDir(env, endpointID)
	pgdata := filepath.Join(endpointDir, "pgdata")
	started := fileExists(pgdata)

	var lsn *string
	switch {
	case !started:
		// Never started — nothing to stop.
	case mode == StopModeImmediateTerminate:
		token, err := env.GenerateAuthToken(jwt.MapClaims{
			"scope": "compute_ctl:admin",
			"aud":   []string{"compute"},
		})
		if err != nil {
			return err
		}
		cc := client.NewComputeCtl(fmt.Sprintf("0.0.0.0:%d", conf.ExternalHTTPPort), token, httpClient())
		resp, err := cc.Terminate(ctx, "immediate")
		if err != nil {
			return fmt.Errorf("/terminate: %w", err)
		}
		lsn = resp.LSN
	default:
		if err := pgCtlStop(env, &conf, pgdata, string(mode)); err != nil {
			return err
		}
	}

	// Reap compute_ctl so the client port frees for the next compute.
	if started {
		sendSigterm := destroy || mode != StopModeFast
		if err := waitComputeCtlExit(endpointDir, sendSigterm); err != nil {
			return err
		}
	}

	// --destroy: tear down the co-located pageserver/safekeeper,
	// remove the endpoint directory, and drop the endpoint from the controller.
	// Mirrors Rust Endpoint::stop(destroy=true) + deprovision_colocated_storage.
	// Load-bearing for the suite: e.g. branch_ancestor cleans up its fork
	// endpoints with `endpoint stop --destroy`; without it, daemons accumulate
	// across scenarios and exhaust YugabyteDB connections.
	if destroy {
		if err := deprovisionColocatedStorage(env, endpointID); err != nil {
			return err
		}
		// Drop the controller's record before the directory: the directory holds
		// the endpoint id, so removing it first would strand the row.
		if err := deleteEndpointRecord(ctx, env, le); err != nil {
			return err
		}
		if err := os.RemoveAll(endpointDir); err != nil {
			return fmt.Errorf("remove endpoint dir %q: %w", endpointDir, err)
		}
	}

	if lsn != nil {
		fmt.Println(*lsn)
	} else {
		fmt.Println("null")
	}
	return nil
}

// deleteEndpointRecord drops the endpoint's row in the control plane. Idempotent:
// a record already gone (or one whose branch was dropped out from under it) is
// not an error — the compute is stopped either way, and `--destroy` must be able
// to finish cleaning up.
func deleteEndpointRecord(ctx context.Context, env *config.LocalEnv, le launcherEndpoint) error {
	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	task, err := b.api.DeleteEndpoint(ctx, le.Pool, le.Project, le.Branch, le.ID)
	if err != nil {
		if client.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete endpoint from controller: %w", err)
	}
	if err := awaitTask(ctx, b.api, task.Id, endpointTaskTimeout); err != nil {
		return fmt.Errorf("delete endpoint %s: %w", le.ID, err)
	}
	return nil
}

// deprovisionColocatedStorage stops and drops the pageserver, safekeeper(s) and
// instances co-located under an endpoint. Mirrors Rust
// Endpoint::deprovision_colocated_storage. Best-effort per daemon (logs and
// continues), then removes the safekeepers from config and
// persists (pageservers are not persisted — rediscovered from disk).
func deprovisionColocatedStorage(env *config.LocalEnv, endpointID string) error {
	endpointPath := endpointDir(env, endpointID)

	for i := range env.Pageservers {
		ps := &env.Pageservers[i]
		if strings.HasPrefix(ps.DataDir, endpointPath) {
			if err := proc.StopProcess("pageserver", ps.DataDir, proc.StopImmediate); err != nil {
				fmt.Fprintf(os.Stderr, "pageserver at %s stop failed: %v\n", ps.DataDir, err)
			}
		}
	}

	var keptSK []config.SafekeeperConf
	for _, sk := range env.Safekeepers {
		if sk.EndpointID == endpointID {
			if err := proc.StopProcess("safekeeper", env.SafekeeperDataDir(sk.EndpointID), proc.StopImmediate); err != nil {
				fmt.Fprintf(os.Stderr, "safekeeper for endpoint %s stop failed: %v\n", sk.EndpointID, err)
			}
			continue
		}
		keptSK = append(keptSK, sk)
	}

	env.Safekeepers = keptSK
	return env.PersistConfig()
}

// pgCtlStop runs `pg_ctl -D <pgdata> -w -m <mode> stop`, mirroring Rust
// `Endpoint::pg_ctl`.
func pgCtlStop(env *config.LocalEnv, conf *endpointConf, pgdata, mode string) error {
	pgCtl := filepath.Join(env.PgBinDir(conf.PgVersion), "pg_ctl")
	cmd := exec.Command(pgCtl, "-D", pgdata, "-w", "-m", mode, "stop")
	lib := env.PgLibDir(conf.PgVersion)
	cmd.Env = []string{
		"LD_LIBRARY_PATH=" + lib,
		"DYLD_LIBRARY_PATH=" + lib,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_ctl failed: %w, output: %s", err, string(out))
	}
	return nil
}

// waitComputeCtlExit reads compute_ctl.pid and waits for the process to exit,
// optionally SIGTERMing it first. Mirrors Rust
// `Endpoint::wait_for_compute_ctl_to_exit`.
func waitComputeCtlExit(endpointDir string, sendSigterm bool) error {
	data, err := os.ReadFile(filepath.Join(endpointDir, "compute_ctl.pid"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(string(bytesTrimSpace(data)))
	if err != nil {
		return fmt.Errorf("parse compute_ctl.pid: %w", err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if sendSigterm {
		_ = proc.Signal(syscall.SIGTERM)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil // gone
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("compute_ctl (pid %d) did not stop in 10s", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\n' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}
