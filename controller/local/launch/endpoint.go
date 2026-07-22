package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/proc"
)

// endpointBasePort is the compute port-allocation base (Rust
// ComputeControlPlane::base_port).
const endpointBasePort uint16 = 55431

// endpointConf is the launcher's persisted compute-endpoint record, stored as
// the opaque `spec` JSON of the controller's endpoint row. It mirrors Rust
// `control_plane::endpoint::EndpointConf` field-for-field (JSON), so a mixed
// old-Rust / new-Go deployment round-trips. Only the Primary path is written.
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
	PgSettings                   [][2]string          `json:"pg_settings"`
}

// loadEndpoints fetches all persisted endpoint records from the controller,
// mirroring Rust `ComputeControlPlane::load`.
func loadEndpoints(ctx context.Context, env *config.LocalEnv) (map[string]endpointConf, error) {
	base, err := controllerBase(env)
	if err != nil {
		return nil, err
	}
	records, err := client.NewController(base, httpClient()).EndpointList(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing endpoints from controller: %w", err)
	}
	out := make(map[string]endpointConf, len(records))
	for _, rec := range records {
		var conf endpointConf
		if err := json.Unmarshal(rec.Spec, &conf); err != nil {
			return nil, fmt.Errorf("deserializing spec for endpoint %s: %w", rec.EndpointID, err)
		}
		out[rec.EndpointID] = conf
	}
	return out, nil
}

// nextCandidatePort is the starting point for compute port allocation: one
// above the highest port any existing endpoint uses (Rust
// `ComputeControlPlane::next_candidate_port`).
func nextCandidatePort(endpoints map[string]endpointConf) uint16 {
	var max uint16
	found := false
	for _, ep := range endpoints {
		for _, p := range []uint16{ep.PgPort, ep.ExternalHTTPPort, ep.InternalHTTPPort} {
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
	EndpointID       string // optional positional; defaults to "ep-<branch_name>"
	BranchID         string
	BranchName       string
	PgVersion        int
	PgPort           *uint16
	ExternalHTTPPort *uint16
	InternalHTTPPort *uint16
	LSN              string      // Static mode not supported in the minimal port
	ConfigOnly       bool        // accepted for compat; ignored (Rust ignores it too)
	PgSettings       [][2]string // repeatable --pg-setting NAME=VALUE
}

// EndpointCreate provisions the branch's pageserver + safekeeper pair and this
// endpoint's endpoint_storage, then records the endpoint in the controller.
// Mirrors the `EndpointCmd::Create` arm of Rust `handle_endpoint`. The pair +
// endpoint_storage are started sequentially here (the Rust concurrency is a
// latency optimization, not a correctness requirement).
func EndpointCreate(ctx context.Context, env *config.LocalEnv, args EndpointCreateArgs) error {
	if args.LSN != "" {
		return fmt.Errorf("static (--lsn) endpoints are not supported in this build; only Primary mode")
	}
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}

	branchName := args.BranchName
	if branchName == "" {
		branchName = DefaultBranchName
	}
	endpointID := args.EndpointID
	if endpointID == "" {
		endpointID = "ep-" + branchName
	}

	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())

	// Resolve the branch id: explicit --branch-id wins; else by registered
	// name; else by a pending-init recipe's name.
	branchID := args.BranchID
	if branchID == "" {
		id, err := getBranchTimelineID(ctx, env, ctrl, branchName)
		if err != nil {
			return err
		}
		if id == "" {
			pending, err := ctrl.ListPendingInits(ctx)
			if err != nil {
				return err
			}
			for _, p := range pending {
				if p.BranchName == branchName {
					id = p.BranchID
					break
				}
			}
		}
		if id == "" {
			return fmt.Errorf("Found no branch id for branch name '%s'", branchName)
		}
		branchID = id
	}

	// Primary mode only; conflict check unless allow_multiple (not exposed).
	if err := checkConflictingEndpoints(endpoints, branchID); err != nil {
		return err
	}

	// Reserve this endpoint's endpoint_storage instance, init its config.
	esConf, err := allocateEndpointStorageConf(env, endpointID)
	if err != nil {
		return err
	}
	esPresent := fileExists(env.EndpointStorageBin())
	if esPresent {
		fmt.Printf("Provisioning endpoint_storage (%s) for endpoint '%s'\n", esConf.ListenAddr, endpointID)
		if err := endpointStorageInit(env, &esConf); err != nil {
			return fmt.Errorf("endpoint_storage init: %w", err)
		}
	} else {
		fmt.Printf("%s binary not found; skipping endpoint_storage provisioning (ignore if this is a compatibility test)\n", env.EndpointStorageBin())
	}

	// Provision the branch's pair (consumes the pending init). On its own
	// failure it cleans up the daemons it started (provisionBootstrap/Branch);
	// on success they are recorded in env below.
	if err := provisionTimelineForEndpoint(ctx, env, endpointID, branchID); err != nil {
		return err
	}

	// The pair is committed to env now. On any failure past this point the
	// config mutation is discarded and a retry reuses these ports, so stop the
	// co-located daemons (pair + endpoint_storage) to avoid orphans. stop* on a
	// not-yet-started daemon is a no-op.
	success := false
	defer func() {
		if success {
			return
		}
		endpointDir := filepath.Join(env.EndpointsPath(), endpointID)
		if esPresent {
			_ = stopEndpointStorage(env, &esConf, true)
		}
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

	// Start endpoint_storage after a successful pair provisioning.
	if esPresent {
		if err := startEndpointStorage(env, &esConf, 30*time.Second); err != nil {
			return fmt.Errorf("start endpoint_storage: %w", err)
		}
		env.EndpointStorages = append(env.EndpointStorages, esConf)
	}

	// Record the endpoint (allocate ports + persist spec in the controller).
	if err := newEndpoint(ctx, env, ctrl, endpoints, endpointID, branchID, args); err != nil {
		return err
	}
	success = true
	return nil
}

// newEndpoint allocates the endpoint's compute ports, builds its EndpointConf,
// and persists it via the controller. Mirrors Rust
// `ComputeControlPlane::new_endpoint`.
func newEndpoint(ctx context.Context, env *config.LocalEnv, ctrl *client.Controller, endpoints map[string]endpointConf, endpointID string, branchID config.BranchID, args EndpointCreateArgs) error {
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
		PgSettings: append([][2]string{}, args.PgSettings...),
	}

	spec, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("serializing endpoint config: %w", err)
	}
	if err := ctrl.EndpointCreate(ctx, registry.ComputeEndpoint{
		EndpointID: endpointID,
		BranchID:   branchID,
		Spec:       spec,
	}); err != nil {
		return fmt.Errorf("creating endpoint in controller: %w", err)
	}
	return nil
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
func checkConflictingEndpoints(endpoints map[string]endpointConf, branchID config.BranchID) error {
	for id, ep := range endpoints {
		if ep.BranchID == branchID && ep.Mode == cfg.ComputeModePrimary {
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
	conf, ok := endpoints[endpointID]
	if !ok {
		return fmt.Errorf("endpoint %s not found", endpointID)
	}

	psConf := env.PageserverConfForBranch(conf.BranchID)
	if psConf == nil {
		return fmt.Errorf("no pageserver serves branch %s", conf.BranchID)
	}

	// Trust path: no storage auth token for the compute→pageserver link.
	var storageAuthToken *string

	// Mint the endpoint_storage token (valid 24h).
	esToken, err := env.GenerateAuthToken(jwt.MapClaims{
		"branch_id":   conf.BranchID,
		"endpoint_id": endpointID,
		"exp":         time.Now().Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		return fmt.Errorf("mint endpoint_storage token: %w", err)
	}

	// Point the compute at this endpoint's own endpoint_storage (fallback to
	// the template addr when none was provisioned, e.g. a compat test).
	esAddr := env.EndpointStorage.ListenAddr
	for _, es := range env.EndpointStorages {
		if es.EndpointID == endpointID {
			esAddr = es.ListenAddr
			break
		}
	}

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
		PgSettings:             conf.PgSettings,
		SafekeeperComputePorts: allSkPorts,
	})

	esAddrCopy := esAddr
	esTokenCopy := esToken
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
		EndpointStorageAddr:          &esAddrCopy,
		EndpointStorageToken:         &esTokenCopy,
		Features:                     nil,
		ComputeCtlConfig:             conf.ComputeCtlConfig,
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
	endpointDir := filepath.Join(env.EndpointsPath(), conf.EndpointID)
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
	conf, ok := endpoints[endpointID]
	if !ok {
		return fmt.Errorf("postgres endpoint %s is not found", endpointID)
	}
	endpointDir := filepath.Join(env.EndpointsPath(), endpointID)
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

	// --destroy: tear down the co-located pageserver/safekeeper/endpoint_storage,
	// remove the endpoint directory, and drop the endpoint from the controller.
	// Mirrors Rust Endpoint::stop(destroy=true) + deprovision_colocated_storage.
	// Load-bearing for the suite: e.g. branch_ancestor cleans up its fork
	// endpoints with `endpoint stop --destroy`; without it, daemons accumulate
	// across scenarios and exhaust YugabyteDB connections.
	if destroy {
		if err := deprovisionColocatedStorage(env, endpointID); err != nil {
			return err
		}
		if err := os.RemoveAll(endpointDir); err != nil {
			return fmt.Errorf("remove endpoint dir %q: %w", endpointDir, err)
		}
		base, err := controllerBase(env)
		if err != nil {
			return err
		}
		if err := client.NewController(base, httpClient()).EndpointDelete(ctx, endpointID); err != nil {
			return fmt.Errorf("delete endpoint from controller: %w", err)
		}
	}

	if lsn != nil {
		fmt.Println(*lsn)
	} else {
		fmt.Println("null")
	}
	return nil
}

// deprovisionColocatedStorage stops and drops the pageserver, safekeeper(s) and
// endpoint_storage instance co-located under an endpoint. Mirrors Rust
// Endpoint::deprovision_colocated_storage. Best-effort per daemon (logs and
// continues), then removes the safekeepers/endpoint_storages from config and
// persists (pageservers are not persisted — rediscovered from disk).
func deprovisionColocatedStorage(env *config.LocalEnv, endpointID string) error {
	endpointPath := filepath.Join(env.EndpointsPath(), endpointID)

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

	var keptES []config.EndpointStorageConf
	for _, es := range env.EndpointStorages {
		if es.EndpointID == endpointID {
			if err := proc.StopProcess("endpoint_storage", env.EndpointStorageDataDir(es.EndpointID), proc.StopImmediate); err != nil {
				fmt.Fprintf(os.Stderr, "endpoint_storage for endpoint %s stop failed: %v\n", endpointID, err)
			}
			continue
		}
		keptES = append(keptES, es)
	}

	env.Safekeepers = keptSK
	env.EndpointStorages = keptES
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
