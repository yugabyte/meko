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

	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/proc"
)

// The pageserver and the safekeeper are the supervisor's children. The launcher
// asks the endpoint's supervisor rather than signalling them, since that is the
// only way a stop sticks. See supervisor.go.

// --- safekeeper ---

// writeSafekeeperFiles creates the safekeeper's datadir and writes its
// safekeeper.toml.
//
// Provisioning-time, not start-time: the supervisor starts the safekeeper from a
// config that must already be on disk. Rendering it here also keeps the
// launcher's local-only knobs (its port allocations) on the launcher's side of
// the line.
func writeSafekeeperFiles(env *config.LocalEnv, conf *config.SafekeeperConf) error {
	dataDir := env.SafekeeperDataDir(conf.EndpointID)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create safekeeper datadir %q: %w", dataDir, err)
	}
	listenAddr := "127.0.0.1"
	if conf.ListenAddr != nil {
		listenAddr = *conf.ListenAddr
	}
	params := cfg.SafekeeperParams{
		BranchID:          conf.BranchID,
		ListenAddr:        listenAddr,
		PgPort:            conf.PgPort,
		PgTenantOnlyPort:  conf.PgTenantOnlyPort,
		HTTPPort:          conf.HTTPPort,
		HTTPSPort:         conf.HTTPSPort,
		YbStorageConn:     conf.YbStorageConn,
		WalCacheSizeMib:   conf.WalCacheSizeMib,
		RequireWriteToken: fencingOverride(),
	}
	return cfg.WriteSafekeeperToml(filepath.Join(dataDir, "safekeeper.toml"), params)
}

// --- controller ---

// startController provisions the external YB database, spawns the controller
// under controller_<id>/, waits for /ready, and provisions the launcher's
// resource ladder. Mirrors the external-YB path of Rust `Controller::start`,
// plus the ladder step the unified branch/endpoint tables require.
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
	// --deploy-driver=local: the controller keeps endpoint records but launches
	// nothing, because the launcher spawns compute-ctl itself. Without it the
	// product endpoint API answers 501 (deploy.LocalDeployer).
	args := []string{
		"--database-url", dbURL, "--listen", listen,
		"--deploy", "--deploy-driver", "local",
	}
	statusCheck := func() error {
		return client.NewAPI(listen, httpClient()).Ready(context.Background())
	}
	fmt.Printf("Starting controller at http://%s\n", listen)
	if err := proc.StartProcess(
		"controller",
		instanceDir,
		bin,
		args,
		pgLibEnv(env),
		proc.PidCreate,
		statusCheck,
		timeout,
	); err != nil {
		return err
	}

	// The branch and endpoint tables are the control plane's own and are
	// cluster-scoped, so the launcher needs a cluster before it can declare a
	// branch. Provision it now that the controller answers, while we are the only
	// thing running.
	mutated, err := EnsureLadder(ctx, env)
	if err != nil {
		return fmt.Errorf("provision the launcher's control-plane cluster: %w", err)
	}
	if mutated {
		persistLadder(env)
	}
	return nil
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

// recreateExternalDatabase DROPs and re-CREATEs the controller's target db (and
// yb_storage if the target differs), so a fresh `yb_amp_local start` always gets
// a schema the controller's migrations just built rather than whatever an older
// checkout left behind. A stale database is not a loud failure: the daemons come
// up and individual queries fail later on columns that do not exist yet.
//
// This DISCARDS all local storage state. It is deliberately reachable only from
// StartAll's provisioning phase — `controller start` keeps using
// ensureExternalDatabase, so restarting just the controller does not wipe data.
//
// Refuses to run against anything but loopback. HostPorts is free-form, and
// dropping a database on a shared cluster because someone pointed a dev config
// at it is not a recoverable mistake.
func recreateExternalDatabase(ctx context.Context, yb *config.YbConnConfig) error {
	local, err := yb.LoopbackOnly()
	if err != nil {
		return fmt.Errorf("checking whether controller.yb is loopback: %w", err)
	}
	if !local {
		return fmt.Errorf(
			"refusing to drop and recreate databases on non-loopback host_ports %q: "+
				"yb_amp_local only does this for a local dev cluster", yb.HostPorts)
	}

	dbname := yb.Dbname
	if dbname == "" {
		dbname = defaultYbDbname
	}
	// Only the database this config names — deliberately NOT also yb_storage when
	// they differ. ensureExternalDatabase creates both because an extra empty
	// database is harmless; dropping one is not. The integration harness runs
	// against an ephemeral `yb_storage_integration_<run id>`
	// (test/integration/compute/run.sh) precisely so concurrent runs and a
	// developer's own cluster stay isolated, and reaching past the named database
	// to drop the shared default would break exactly that.
	if err := dropDatabase(ctx, yb, dbname); err != nil {
		return err
	}
	return createDatabase(ctx, yb, dbname)
}

// dropDatabase runs `DROP DATABASE IF EXISTS <name> WITH (FORCE)` against the
// admin `yugabyte` database. FORCE terminates existing backends, so a leftover
// connection from a previous run does not block startup; IF EXISTS makes the
// first-ever start a no-op rather than an error.
func dropDatabase(ctx context.Context, yb *config.YbConnConfig, name string) error {
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

	stmt := fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, sqlIdentEscape(name))
	if _, err := conn.Exec(connCtx, stmt); err != nil {
		return fmt.Errorf("dropping database %q: %w", name, err)
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
