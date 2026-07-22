package launch

import (
	"crypto/rand"
	"encoding/hex"
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
	defaultEndpointStgPort    uint16 = 9993
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
	YbHotPath      string // "tsclient" (default) or "ysql"
	PortBase       *uint16
	NumPageservers uint16 // default 1; only the single-PS path is supported
	Force          InitForceMode
}

// generateBranchID returns a fresh 32-hex-char (128-bit) branch id, mirroring
// Rust `BranchId::generate`.
func generateBranchID() (config.BranchID, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate branch id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// zeroBranchID is the all-zeros default_branch_id baked into the default init
// config (Rust `BranchId::from_array([0; 16])`).
const zeroBranchID = "00000000000000000000000000000000"

// ybInlineTOML renders the shared [*.yb] connection block as the inline-table
// string stored on the pageserver/safekeeper templates, mirroring Rust
// `YbConnConfig::to_inline_toml`. Only the fields the default init config sets
// are emitted (host_ports/user/dbname always; hot_path only when ysql).
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
	if c.HotPath != nil && *c.HotPath == "ysql" {
		parts = append(parts, `hot_path = "ysql"`)
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// Init materializes a fresh `.yb_amp/` repo (config + ed25519 keypair +
// endpoints dir) for the default (no-`--config`) path. It runs WITHOUT the
// repo lock (the dir may not exist yet). Mirrors Rust `handle_init` +
// `LocalEnv::init`.
func Init(args InitArgs) error {
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
	esPort := defaultEndpointStgPort
	cplanePort := defaultControlPlanePort
	if args.PortBase != nil {
		b := *args.PortBase
		skPg = b + 100
		skHTTP = b + 200
		psPg = b + 300
		psHTTP = b + 400
		psGrpc = b + 500
		esPort = b + 600
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

	esAddr, err := setAddrPort("127.0.0.1:0", esPort)
	if err != nil {
		return err
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

	def := zeroBranchID
	controllerConf := config.DefaultYbAmpControllerConf()
	ybCopy := yb
	controllerConf.Yb = &ybCopy
	cplaneAPI := controlPlaneAPI

	onDisk := config.OnDiskConfig{
		PgDistribDir:        pgDistribDir,
		YbAmpDistribDir:     ybAmpDistribDir,
		DefaultBranchID:     &def,
		PrivateKeyPath:      "auth_private_key.pem",
		PublicKeyPath:       "auth_public_key.pem",
		Controller:          controllerConf,
		Safekeepers:         []config.SafekeeperConf{},
		PageserverTemplates: []config.YbAmpLocalInitPageserverConf{psTemplate},
		SafekeeperTemplates: []config.SafekeeperConf{skTemplate},
		EndpointStorage: config.EndpointStorageConf{
			ListenAddr: esAddr,
		},
		EndpointStorages: nil,
		ControlPlaneAPI:  &cplaneAPI,
	}

	// endpoints dir (the PS+SK pairs are provisioned under it at
	// `endpoint create`).
	if err := os.MkdirAll(filepath.Join(base, "endpoints"), 0o755); err != nil {
		return fmt.Errorf("create endpoints dir: %w", err)
	}

	if err := onDisk.Persist(filepath.Join(base, "config")); err != nil {
		return err
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
	if args.YbHotPath == "ysql" {
		hp := "ysql"
		c.HotPath = &hp
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
