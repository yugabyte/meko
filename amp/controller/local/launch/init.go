package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yugabyte/amp/controller/local/config"
)

// InitForceMode selects how `init` treats a pre-existing repo dir. The string
// values match the Rust `InitForceMode` clap value-enum spellings.
type InitForceMode string

const (
	// ForceMustNotExist fails if the repo dir already exists (the default).
	ForceMustNotExist InitForceMode = "must-not-exist"
	// ForceEmptyDirOk allows an existing but empty repo dir.
	ForceEmptyDirOk InitForceMode = "empty-dir-ok"
	// ForceRemoveAllContents empties an existing repo dir (keeping the dir
	// itself, so a symlinked datadir survives).
	ForceRemoveAllContents InitForceMode = "remove-all-contents"
)

// Default port lanes (Rust DEFAULT_* constants). control_plane_api lives at
// the base; each service family gets a 100-port lane off --port-base.
const (
	defaultSafekeeperPgPort   uint16 = 5454
	defaultSafekeeperHTTPPort uint16 = 7676
	defaultPageserverPgPort   uint16 = 64000
	defaultPageserverHTTPPort uint16 = 9898
	defaultPageserverGrpcPort uint16 = 51051
	defaultControlPlanePort   uint16 = 1234
)

const (
	defaultYbHost   = "127.0.0.1"
	defaultYbPort   = 5433
	defaultYbUser   = "yugabyte"
	defaultYbDbname = "yb_storage"
)

// InitArgs are the flags of `yb_amp_local init` (the default, no-`--config`
// path — the only one the harness uses).
type InitArgs struct {
	YbDbname       string
	YbHost         string
	YbPort         uint16
	PortBase       *uint16
	NumPageservers uint16 // default 1; only the single-PS path is supported
	Force          InitForceMode
}

// ybInlineTOML renders the shared [*.yb] connection block as the inline-table
// string stored on the pageserver/safekeeper templates, mirroring Rust
// `YbConnConfig::to_inline_toml`.
func ybInlineTOML(c *config.YbConnConfig) string {
	q := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}
	parts := []string{
		"host_ports = " + q(c.HostPorts),
		"user = " + q(c.User),
		"dbname = " + q(c.Dbname),
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// Init materializes a fresh `.yb_amp/` repo (config + ed25519 keypair +
// endpoints dir) for the default (no-`--config`) path. It runs WITHOUT the
// repo lock (the dir may not exist yet). Mirrors Rust `handle_init` +
// `LocalEnv::init`.
// Init writes a fresh .yb_amp repo dir and resets the external yb_storage
// database to match it.
//
// The database reset lives here rather than in `start` because the two halves
// of a local instance have to be reset together. Pageservers are rediscovered
// from endpoints/*/pageserver/pageserver.toml on every load and read their
// amp_storage.branch_state row at startup; dropping the database while those
// directories survive leaves each one pointing at a row that no longer
// exists. init is the one command that has already emptied the on-disk
// state (prepareBaseDir), so resetting the database here keeps the pair
// consistent by construction.
func Init(ctx context.Context, args InitArgs) error {
	base, err := config.BasePath()
	if err != nil {
		return err
	}

	if err := prepareBaseDir(base, args.Force); err != nil {
		return err
	}

	yb := defaultYbConn(args)
	inline := ybInlineTOML(&yb)

	// Port lanes off --port-base.
	skPg := defaultSafekeeperPgPort
	skHTTP := defaultSafekeeperHTTPPort
	psPg := defaultPageserverPgPort
	psHTTP := defaultPageserverHTTPPort
	psGrpc := defaultPageserverGrpcPort
	cplanePort := defaultControlPlanePort
	if args.PortBase != nil {
		b := *args.PortBase
		skPg = b + 100
		skHTTP = b + 200
		psPg = b + 300
		psHTTP = b + 400
		psGrpc = b + 500
		cplanePort = b
	}
	controlPlaneAPI := fmt.Sprintf("http://127.0.0.1:%d/upcall/v1/", cplanePort)

	// Single-pageserver path (--num-pageservers>1 is out of scope). The
	// template carries the shared yb_storage_conn inline table + no_sync.
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", psGrpc)
	inlineCopyP := inline
	psTemplate := config.YbAmpLocalInitPageserverConf{
		ListenPgAddr:   fmt.Sprintf("127.0.0.1:%d", psPg),
		ListenHTTPAddr: fmt.Sprintf("127.0.0.1:%d", psHTTP),
		ListenGRPCAddr: &grpcAddr,
		PgAuthType:     config.AuthTrust,
		HTTPAuthType:   config.AuthTrust,
		GRPCAuthType:   config.AuthTrust,
		NoSync:         true,
		YbStorageConn:  &inlineCopyP,
	}

	inlineCopyS := inline
	skTemplate := config.SafekeeperConf{
		PgPort:        skPg,
		HTTPPort:      skHTTP,
		YbStorageConn: &inlineCopyS,
	}

	// distrib dirs: pg from POSTGRES_DISTRIB_DIR (or $cwd/pg_install);
	// yb-amp binaries from the launcher's own dir.
	pgDistribDir := os.Getenv("POSTGRES_DISTRIB_DIR")
	if pgDistribDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("determine current directory: %w", err)
		}
		pgDistribDir = filepath.Join(cwd, "pg_install")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("determine current executable: %w", err)
	}
	ybAmpDistribDir := filepath.Dir(exe)

	// Generate the JWT keypair (relative paths, resolved against base).
	if err := config.GenerateAuthKeys(
		filepath.Join(base, "auth_private_key.pem"),
		filepath.Join(base, "auth_public_key.pem"),
	); err != nil {
		return fmt.Errorf("generate auth keys: %w", err)
	}

	controllerConf := config.DefaultYbAmpControllerConf()
	ybCopy := yb
	controllerConf.Yb = &ybCopy
	cplaneAPI := controlPlaneAPI

	onDisk := config.OnDiskConfig{
		PgDistribDir:        pgDistribDir,
		YbAmpDistribDir:     ybAmpDistribDir,
		PrivateKeyPath:      "auth_private_key.pem",
		PublicKeyPath:       "auth_public_key.pem",
		Controller:          controllerConf,
		Safekeepers:         []config.SafekeeperConf{},
		PageserverTemplates: []config.YbAmpLocalInitPageserverConf{psTemplate},
		SafekeeperTemplates: []config.SafekeeperConf{skTemplate},
		ControlPlaneAPI:     &cplaneAPI,
	}

	// endpoints dir (the PS+SK pairs are provisioned under it at
	// `endpoint create`).
	if err := os.MkdirAll(filepath.Join(base, "endpoints"), 0o755); err != nil {
		return fmt.Errorf("create endpoints dir: %w", err)
	}

	if err := onDisk.Persist(filepath.Join(base, "config")); err != nil {
		return err
	}

	// Reset the database last: prepareBaseDir has emptied the repo dir, so there
	// are no endpoints left holding branch_state rows, and the controller's
	// migrations rebuild the schema on the next `start`. Doing this at init
	// rather than at start is what stops a database created by an older checkout
	// from silently outliving the code that expects its schema.
	//
	// Only for a loopback cluster. Pointing init at a remote YB is legitimate,
	// and refusing to initialise because someone else's database cannot be
	// dropped would be worse than leaving it alone — so say what was skipped and
	// carry on.
	local, err := yb.LoopbackOnly()
	if err != nil {
		return fmt.Errorf("checking whether controller.yb is loopback: %w", err)
	}
	if local {
		fmt.Println("Recreating the yb_storage database to match this fresh repo")
		if err := recreateExternalDatabase(ctx, &yb); err != nil {
			return fmt.Errorf("reset external controller database: %w", err)
		}
	} else {
		fmt.Printf("Leaving the database on %s untouched: not a loopback host. "+
			"If it holds a schema from an older checkout, reset it yourself.\n", yb.HostPorts)
	}
	return nil
}

// defaultYbConn builds the shared [*.yb] connection block from the init flags,
// mirroring Rust `default_yb_conn_config`.
func defaultYbConn(args InitArgs) config.YbConnConfig {
	host := args.YbHost
	if host == "" {
		host = defaultYbHost
	}
	port := args.YbPort
	if port == 0 {
		port = defaultYbPort
	}
	dbname := args.YbDbname
	if dbname == "" {
		dbname = defaultYbDbname
	}
	c := config.YbConnConfig{
		HostPorts: fmt.Sprintf("%s:%d", host, port),
		User:      defaultYbUser,
		Dbname:    dbname,
	}
	return c
}

// prepareBaseDir applies the force policy to a possibly-existing repo dir,
// then ensures it exists. Mirrors the force handling in Rust `LocalEnv::init`.
func prepareBaseDir(base string, force InitForceMode) error {
	if force == "" {
		force = ForceMustNotExist
	}
	if _, err := os.Stat(base); err == nil {
		switch force {
		case ForceMustNotExist:
			return fmt.Errorf("directory %q already exists. Perhaps already initialized?", base)
		case ForceEmptyDirOk:
			entries, err := os.ReadDir(base)
			if err != nil {
				return fmt.Errorf("check if directory is empty: %w", err)
			}
			if len(entries) > 0 {
				return fmt.Errorf("directory not empty: %q", base)
			}
		case ForceRemoveAllContents:
			fmt.Printf("removing all contents of %q\n", base)
			entries, err := os.ReadDir(base)
			if err != nil {
				return fmt.Errorf("read %q: %w", base, err)
			}
			for _, e := range entries {
				if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
					return fmt.Errorf("remove %q: %w", filepath.Join(base, e.Name()), err)
				}
			}
		default:
			return fmt.Errorf("unknown force mode %q", force)
		}
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fmt.Errorf("create repo dir %q: %w", base, err)
	}
	return nil
}
