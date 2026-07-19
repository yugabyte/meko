package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/proc"
)

// --- pageserver ---

// nodeRegisterRequest builds the controller node-registration payload for a
// pageserver, mirroring Rust `PageServerNode::node_register_request`. Ports
// come from the listen addresses; the advertised host is always "localhost".
func nodeRegisterRequest(conf *config.PageServerConf) (registry.NodeRegisterRequest, error) {
	pgPort, err := portOf(conf.ListenPgAddr)
	if err != nil {
		return registry.NodeRegisterRequest{}, err
	}
	httpPort, err := portOf(conf.ListenHTTPAddr)
	if err != nil {
		return registry.NodeRegisterRequest{}, err
	}
	req := registry.NodeRegisterRequest{
		NodeID:         conf.ID,
		ListenPgAddr:   "localhost",
		ListenPgPort:   pgPort,
		ListenHTTPAddr: "localhost",
		ListenHTTPPort: httpPort,
	}
	if conf.ListenHTTPSAddr != nil {
		hp, err := portOf(*conf.ListenHTTPSAddr)
		if err != nil {
			return registry.NodeRegisterRequest{}, err
		}
		req.ListenHTTPSPort = &hp
	}
	if conf.ListenGRPCAddr != nil {
		gp, err := portOf(*conf.ListenGRPCAddr)
		if err != nil {
			return registry.NodeRegisterRequest{}, err
		}
		host := "localhost"
		req.ListenGrpcAddr = &host
		req.ListenGrpcPort = &gp
	}
	return req, nil
}

// registerAndAttach registers the pageserver with the controller, bumps the
// branch's generation via the attach-hook, and persists the fresh generation
// into pageserver.toml. No-op when the pageserver has no baked branch
// identity. Mirrors Rust `PageServerNode::register_and_attach`.
func registerAndAttach(ctx context.Context, env *config.LocalEnv, conf *config.PageServerConf) error {
	if conf.BranchID == nil {
		return nil
	}
	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())
	req, err := nodeRegisterRequest(conf)
	if err != nil {
		return err
	}
	if err := ctrl.NodeRegister(ctx, req); err != nil {
		return fmt.Errorf("register pageserver node with controller: %w", err)
	}
	nodeID := conf.ID
	resp, err := ctrl.AttachHook(ctx, registry.AttachHookRequest{
		BranchID: *conf.BranchID,
		NodeID:   &nodeID,
	})
	if err != nil {
		return err
	}
	if resp.Generation == nil {
		return fmt.Errorf("attach_hook did not return a generation")
	}
	tomlPath := filepath.Join(conf.DataDir, "pageserver.toml")
	if err := cfg.SetPageserverIdentity(tomlPath, *conf.BranchID, *resp.Generation, nil); err != nil {
		return err
	}
	fmt.Printf("pageserver %d serves %s at generation %d\n", conf.ID, *conf.BranchID, *resp.Generation)
	return nil
}

// startPageserver registers+attaches then spawns yb-pageserver, mirroring
// Rust `PageServerNode::start` (register_and_attach + start_node).
func startPageserver(ctx context.Context, env *config.LocalEnv, conf *config.PageServerConf, timeout time.Duration) error {
	if err := registerAndAttach(ctx, env, conf); err != nil {
		return err
	}
	return startPageserverNode(env, conf, timeout)
}

// startPageserverNode spawns yb-pageserver (no attach), mirroring Rust
// `PageServerNode::start_node`.
func startPageserverNode(env *config.LocalEnv, conf *config.PageServerConf, timeout time.Duration) error {
	statusCheck := func() error {
		ps := client.NewPageserver(conf.ListenHTTPAddr, httpClient())
		return ps.Status(context.Background())
	}
	// Trust path: no auth token env. (Rust sets YBAMP_AUTH_TOKEN only when
	// http_auth_type != Trust, which never happens in the minimal port.)
	return proc.StartProcess(
		"pageserver",
		conf.DataDir,
		env.PageserverBin(),
		[]string{"-D", conf.DataDir},
		nil,
		proc.PidExpect,
		statusCheck,
		timeout,
	)
}

// stopPageserver signals the pageserver to stop.
func stopPageserver(conf *config.PageServerConf, immediate bool) error {
	mode := proc.StopFast
	if immediate {
		mode = proc.StopImmediate
	}
	return proc.StopProcess("pageserver", conf.DataDir, mode)
}

// --- safekeeper ---

// safekeeperControlFileSaveInterval reads the optional test-only cadence
// override (Rust YB_SK_CONTROL_FILE_SAVE_INTERVAL); nil keeps the default.
func safekeeperControlFileSaveInterval() (*time.Duration, error) {
	s, ok := os.LookupEnv("YB_SK_CONTROL_FILE_SAVE_INTERVAL")
	if !ok {
		return nil, nil
	}
	var d config.HumantimeDuration
	if err := d.UnmarshalText([]byte(s)); err != nil {
		return nil, fmt.Errorf("parse YB_SK_CONTROL_FILE_SAVE_INTERVAL=%q: %w", s, err)
	}
	dur := d.Duration()
	return &dur, nil
}

// startSafekeeper writes safekeeper.toml then spawns yb-safekeeper, mirroring
// Rust `SafekeeperNode::start`. It does NOT create the timeline (provisioning
// does that separately).
func startSafekeeper(env *config.LocalEnv, conf *config.SafekeeperConf, timeout time.Duration) error {
	dataDir := env.SafekeeperDataDir(conf.EndpointID)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create safekeeper datadir %q: %w", dataDir, err)
	}
	listenAddr := "127.0.0.1"
	if conf.ListenAddr != nil {
		listenAddr = *conf.ListenAddr
	}
	cfsi, err := safekeeperControlFileSaveInterval()
	if err != nil {
		return err
	}
	params := cfg.SafekeeperParams{
		ID:                      conf.ID,
		BranchID:                conf.BranchID,
		ListenAddr:              listenAddr,
		PgPort:                  conf.PgPort,
		PgTenantOnlyPort:        conf.PgTenantOnlyPort,
		HTTPPort:                conf.HTTPPort,
		HTTPSPort:               conf.HTTPSPort,
		YbStorageConn:           conf.YbStorageConn,
		WalCacheSizeMib:         conf.WalCacheSizeMib,
		ControlFileSaveInterval: cfsi,
	}
	if err := cfg.WriteSafekeeperToml(filepath.Join(dataDir, "safekeeper.toml"), params); err != nil {
		return err
	}

	// The safekeeper writes a fixed "safekeeper.pid" into its datadir (one
	// safekeeper per endpoint datadir), and proc derives the pidfile it polls
	// from this name — so it must be "safekeeper", not "safekeeper-<id>".
	name := "safekeeper"
	statusCheck := func() error {
		sk := client.NewSafekeeper(fmt.Sprintf("%s:%d", listenAddr, conf.HTTPPort), httpClient())
		_, err := sk.Status(context.Background())
		return err
	}
	return proc.StartProcess(
		name,
		dataDir,
		env.SafekeeperBin(),
		[]string{"-D", dataDir},
		nil,
		proc.PidExpect,
		statusCheck,
		timeout,
	)
}

// stopSafekeeper signals the safekeeper to stop.
func stopSafekeeper(env *config.LocalEnv, conf *config.SafekeeperConf, immediate bool) error {
	mode := proc.StopFast
	if immediate {
		mode = proc.StopImmediate
	}
	return proc.StopProcess("safekeeper", env.SafekeeperDataDir(conf.EndpointID), mode)
}

// --- endpoint_storage ---

// startEndpointStorage spawns yb-endpoint-storage for one provisioned
// instance, mirroring Rust `EndpointStorage::start`. config.json is written at
// `endpoint create` (endpointStorageInit). RUST_LOG=debug + PidCreate (the
// binary does not manage its own pidfile).
func startEndpointStorage(env *config.LocalEnv, conf *config.EndpointStorageConf, timeout time.Duration) error {
	dataDir := env.EndpointStorageDataDir(conf.EndpointID)
	configPath := filepath.Join(dataDir, "endpoint_storage.json")
	listen := conf.ListenAddr
	statusCheck := func() error {
		resp, err := httpClient().Get("http://" + listen + "/metrics")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("endpoint_storage /metrics: http %d", resp.StatusCode)
		}
		return nil
	}
	return proc.StartProcess(
		"endpoint_storage",
		dataDir,
		env.EndpointStorageBin(),
		[]string{configPath},
		map[string]string{"RUST_LOG": "debug"},
		proc.PidCreate,
		statusCheck,
		timeout,
	)
}

// stopEndpointStorage signals an endpoint_storage instance to stop.
func stopEndpointStorage(env *config.LocalEnv, conf *config.EndpointStorageConf, immediate bool) error {
	mode := proc.StopFast
	if immediate {
		mode = proc.StopImmediate
	}
	return proc.StopProcess("endpoint_storage", env.EndpointStorageDataDir(conf.EndpointID), mode)
}

// endpointStorageInit writes endpoint_storage.json for a provisioned instance,
// mirroring Rust `EndpointStorage::init`.
func endpointStorageInit(env *config.LocalEnv, conf *config.EndpointStorageConf) error {
	dataDir := env.EndpointStorageDataDir(conf.EndpointID)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create endpoint_storage datadir %q: %w", dataDir, err)
	}
	params := cfg.EndpointStorageParams{
		Listen:            conf.ListenAddr,
		Pemfile:           env.GetPublicKeyPath(),
		RemoteStorageJSON: conf.RemoteStorageJSON,
		YbConn:            env.Controller.Yb,
	}
	return cfg.WriteEndpointStorageJSON(filepath.Join(dataDir, "endpoint_storage.json"), params)
}

// --- controller ---

// startController provisions the external YB database, spawns the controller
// under controller_<id>/, and waits for /ready. Mirrors the external-YB path
// of Rust `Controller::start`.
func startController(ctx context.Context, env *config.LocalEnv, instanceID uint8, timeout time.Duration) error {
	instanceDir := filepath.Join(env.BaseDataDir, fmt.Sprintf("controller_%d", instanceID))
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		return fmt.Errorf("create controller instance dir %q: %w", instanceDir, err)
	}
	if env.Controller.Yb == nil {
		return fmt.Errorf("controller.yb must be configured (external YugabyteDB is the only supported path)")
	}
	if err := ensureExternalDatabase(ctx, env.Controller.Yb); err != nil {
		return err
	}
	dbURL, err := env.Controller.Yb.ToDatabaseURL()
	if err != nil {
		return fmt.Errorf("building database URL from controller.yb: %w", err)
	}
	listen, err := controllerBase(env)
	if err != nil {
		return err
	}
	bin, err := env.ControllerBin()
	if err != nil {
		return err
	}
	args := []string{"--dev", "--database-url", dbURL, "--listen", listen}
	statusCheck := func() error {
		return client.NewController(listen, httpClient()).Ready(context.Background())
	}
	fmt.Printf("Starting controller at http://%s\n", listen)
	return proc.StartProcess(
		"controller",
		instanceDir,
		bin,
		args,
		pgLibEnv(env),
		proc.PidCreate,
		statusCheck,
		timeout,
	)
}

// stopController signals the controller instance to stop.
func stopController(env *config.LocalEnv, instanceID uint8, immediate bool) error {
	mode := proc.StopFast
	if immediate {
		mode = proc.StopImmediate
	}
	return proc.StopProcess("controller", filepath.Join(env.BaseDataDir, fmt.Sprintf("controller_%d", instanceID)), mode)
}

// controllerInstances scans the repo dir for `controller_<n>` dirs, mirroring
// Rust `LocalEnv::controller_instances`.
func controllerInstances(env *config.LocalEnv) ([]uint8, error) {
	entries, err := os.ReadDir(env.BaseDataDir)
	if err != nil {
		return nil, err
	}
	var ids []uint8
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		const prefix = "controller_"
		if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
			continue
		}
		id, err := parseUint8(name[len(prefix):])
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseUint8(s string) (uint8, error) {
	var n uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + uint64(r-'0')
		if n > 255 {
			return 0, fmt.Errorf("out of range: %q", s)
		}
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return uint8(n), nil
}

// ensureExternalDatabase CREATE DATABASEs the controller's target db (and
// yb_storage if the target differs) on the external YB cluster, connecting to
// the `yugabyte` admin db. Idempotent (treats duplicate_database as success).
// Mirrors Rust `Controller::ensure_external_database` + `create_database`.
func ensureExternalDatabase(ctx context.Context, yb *config.YbConnConfig) error {
	dbname := yb.Dbname
	if dbname == "" {
		dbname = defaultYbDbname
	}
	if err := createDatabase(ctx, yb, dbname); err != nil {
		return err
	}
	if dbname != defaultYbDbname {
		if err := createDatabase(ctx, yb, defaultYbDbname); err != nil {
			return err
		}
	}
	return nil
}

// createDatabase runs `CREATE DATABASE <name>` against the admin `yugabyte`
// database on the cluster described by yb. "already exists" is success.
func createDatabase(ctx context.Context, yb *config.YbConnConfig, name string) error {
	admin := *yb
	admin.Dbname = "yugabyte"
	url, err := admin.ToDatabaseURL()
	if err != nil {
		return fmt.Errorf("building admin database URL from controller.yb: %w", err)
	}
	connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, url)
	if err != nil {
		return fmt.Errorf("connecting to YB admin database 'yugabyte': %w", err)
	}
	defer conn.Close(context.Background())

	stmt := fmt.Sprintf(`CREATE DATABASE "%s"`, sqlIdentEscape(name))
	if _, err := conn.Exec(connCtx, stmt); err != nil {
		var pgErr *pgconn.PgError
		// 42P04 = duplicate_database.
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" {
			return nil
		}
		return fmt.Errorf("creating database %q: %w", name, err)
	}
	fmt.Printf("Created database '%s'\n", name)
	return nil
}

// sqlIdentEscape doubles embedded double-quotes for a quoted SQL identifier.
func sqlIdentEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
