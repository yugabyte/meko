package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pelletier/go-toml/v2"
)

// LocalEnv is the in-memory launcher environment, mirroring Rust
// `LocalEnv`. It is the on-disk config plus the base data directory (where
// the config file lives) and the discovered pageservers. Fields that only
// matter for TLS / local-Postgres / the memoized HTTP client are omitted
// (out of scope for the minimal port).
type LocalEnv struct {
	// BaseDataDir is the base directory for all nodes. It is not stored in
	// the config file; it is where the config file itself lives (see
	// BasePath).
	BaseDataDir string

	PgDistribDir    string
	YbAmpDistribDir string
	DefaultBranchID *BranchID
	PrivateKeyPath  string
	PublicKeyPath   string

	Controller YbAmpControllerConf

	// Pageservers are discovered by LoadConfig from the pageserver.toml
	// under each endpoints/<name>/pageserver/. Empty on a fresh repo.
	Pageservers []PageServerConf
	// Safekeepers are the provisioned safekeepers persisted in the config.
	Safekeepers []SafekeeperConf
	// PageserverTemplates / SafekeeperTemplates are the init-time confs
	// that endpoint create clones.
	PageserverTemplates []YbAmpLocalInitPageserverConf
	SafekeeperTemplates []SafekeeperConf

	EndpointStorage  EndpointStorageConf
	EndpointStorages []EndpointStorageConf

	// ControlPlaneAPI is required (Rust unwraps the Option on load).
	ControlPlaneAPI      string
	ControlPlaneHooksAPI *string

	UseLocalTLS bool
}

// BasePath reproduces Rust `base_path`: the `.yb_amp` base directory. It is
// read from YB_AMP_REPO_DIR (which must be an absolute path); if that env
// var is unset, `$PWD/.yb_amp` is used. Unlike the Rust version (which
// panics), a non-absolute YB_AMP_REPO_DIR returns an error.
func BasePath() (string, error) {
	if val := os.Getenv("YB_AMP_REPO_DIR"); val != "" {
		if !filepath.IsAbs(val) {
			return "", fmt.Errorf("YB_AMP_REPO_DIR must be an absolute path, got %q", val)
		}
		return val, nil
	}
	pwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine current directory: %w", err)
	}
	// Rust canonicalizes $PWD (resolving symlinks). EvalSymlinks matches
	// that; fall back to the raw (already absolute) cwd if it fails.
	if resolved, err := filepath.EvalSymlinks(pwd); err == nil {
		pwd = resolved
	}
	return filepath.Join(pwd, ".yb_amp"), nil
}

// --- binary path helpers ---

// EndpointStorageBin is the yb-endpoint-storage binary in the yb-amp
// distribution dir.
func (e *LocalEnv) EndpointStorageBin() string {
	return filepath.Join(e.YbAmpDistribDir, "yb-endpoint-storage")
}

// PageserverBin is the yb-pageserver binary (the process carries the yb-
// prefix; the crate/folder are bare `pageserver`).
func (e *LocalEnv) PageserverBin() string {
	return filepath.Join(e.YbAmpDistribDir, "yb-pageserver")
}

// SafekeeperBin is the yb-safekeeper binary.
func (e *LocalEnv) SafekeeperBin() string {
	return filepath.Join(e.YbAmpDistribDir, "yb-safekeeper")
}

// ControllerBin is the `controller` binary, always run from the same
// directory as the running yb_amp_local binary (so compat tests running an
// older pageserver/safekeeper still get the latest controller). Mirrors
// Rust `controller_bin` (which uses env::current_exe()).
func (e *LocalEnv) ControllerBin() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("determine current executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "controller"), nil
}

// --- postgres dir helpers ---

// PgDistribDirRaw returns the pg_distrib_dir as configured.
func (e *LocalEnv) PgDistribDirRaw() string {
	return e.PgDistribDir
}

// PgVersionDir is the versioned distribution dir, e.g. `<pg_distrib_dir>/v18`.
func (e *LocalEnv) PgVersionDir(pgVersion int) string {
	return filepath.Join(e.PgDistribDir, "v"+strconv.Itoa(pgVersion))
}

// PgDir joins dirName onto the versioned distribution dir.
func (e *LocalEnv) PgDir(pgVersion int, dirName string) string {
	return filepath.Join(e.PgVersionDir(pgVersion), dirName)
}

// PgBinDir is `<pg_distrib_dir>/v<N>/bin`.
func (e *LocalEnv) PgBinDir(pgVersion int) string {
	return e.PgDir(pgVersion, "bin")
}

// PgLibDir is `<pg_distrib_dir>/v<N>/lib`.
func (e *LocalEnv) PgLibDir(pgVersion int) string {
	return e.PgDir(pgVersion, "lib")
}

// --- data-dir helpers ---

// EndpointsPath is `<base>/endpoints`.
func (e *LocalEnv) EndpointsPath() string {
	return filepath.Join(e.BaseDataDir, "endpoints")
}

// PageserverDataDir is `endpoints/<endpointID>/pageserver`.
func (e *LocalEnv) PageserverDataDir(endpointID string) string {
	return filepath.Join(e.EndpointsPath(), endpointID, "pageserver")
}

// SafekeeperDataDir is `endpoints/<endpointID>/safekeeper`.
func (e *LocalEnv) SafekeeperDataDir(endpointID string) string {
	return filepath.Join(e.EndpointsPath(), endpointID, "safekeeper")
}

// EndpointStorageDataDir is `endpoints/<endpointID>/endpoint_storage`.
func (e *LocalEnv) EndpointStorageDataDir(endpointID string) string {
	return filepath.Join(e.EndpointsPath(), endpointID, "endpoint_storage")
}

// --- key path helpers ---

// GetPrivateKeyPath resolves the private key path (relative paths are
// resolved against BaseDataDir), mirroring Rust `get_private_key_path`.
func (e *LocalEnv) GetPrivateKeyPath() string {
	if filepath.IsAbs(e.PrivateKeyPath) {
		return e.PrivateKeyPath
	}
	return filepath.Join(e.BaseDataDir, e.PrivateKeyPath)
}

// GetPublicKeyPath resolves the public key path, mirroring Rust
// `get_public_key_path`.
func (e *LocalEnv) GetPublicKeyPath() string {
	if filepath.IsAbs(e.PublicKeyPath) {
		return e.PublicKeyPath
	}
	return filepath.Join(e.BaseDataDir, e.PublicKeyPath)
}

// --- config load / persist ---

// LoadConfig constructs a LocalEnv from on-disk state, mirroring Rust
// `LocalEnv::load_config`. It parses `<repopath>/config`, then discovers
// pageservers by scanning endpoints/*/pageserver/. control_plane_api is
// required.
func LoadConfig(repopath string) (*LocalEnv, error) {
	if _, err := os.Stat(repopath); err != nil {
		return nil, fmt.Errorf(
			"yb-amp config is not found in %s. You need to run 'yb_amp_local init' first", repopath)
	}
	onDisk, err := Load(filepath.Join(repopath, "config"))
	if err != nil {
		return nil, err
	}
	if onDisk.ControlPlaneAPI == nil {
		return nil, fmt.Errorf("config is missing required `control_plane_api`")
	}
	env := &LocalEnv{
		BaseDataDir:          repopath,
		PgDistribDir:         onDisk.PgDistribDir,
		YbAmpDistribDir:      onDisk.YbAmpDistribDir,
		DefaultBranchID:      onDisk.DefaultBranchID,
		PrivateKeyPath:       onDisk.PrivateKeyPath,
		PublicKeyPath:        onDisk.PublicKeyPath,
		Controller:           onDisk.Controller,
		Safekeepers:          onDisk.Safekeepers,
		PageserverTemplates:  onDisk.PageserverTemplates,
		SafekeeperTemplates:  onDisk.SafekeeperTemplates,
		EndpointStorage:      onDisk.EndpointStorage,
		EndpointStorages:     onDisk.EndpointStorages,
		ControlPlaneAPI:      *onDisk.ControlPlaneAPI,
		ControlPlaneHooksAPI: onDisk.ControlPlaneHooksAPI,
		UseLocalTLS:          onDisk.UseLocalTLS,
	}
	// The source of truth for a pageserver's runtime config is the
	// pageserver.toml under its endpoint's pageserver/ subdir.
	pageservers, err := DiscoverPageservers(env.EndpointsPath())
	if err != nil {
		return nil, err
	}
	env.Pageservers = pageservers
	return env, nil
}

// toOnDisk builds the OnDiskConfig for persistence, mirroring Rust
// `persist_config`. Pageservers are never serialized (discovered from
// disk); control_plane_compute_hook_api is always None.
func (e *LocalEnv) toOnDisk() OnDiskConfig {
	api := e.ControlPlaneAPI
	return OnDiskConfig{
		PgDistribDir:               e.PgDistribDir,
		YbAmpDistribDir:            e.YbAmpDistribDir,
		DefaultBranchID:            e.DefaultBranchID,
		PrivateKeyPath:             e.PrivateKeyPath,
		PublicKeyPath:              e.PublicKeyPath,
		Controller:                 e.Controller,
		Safekeepers:                e.Safekeepers,
		PageserverTemplates:        e.PageserverTemplates,
		SafekeeperTemplates:        e.SafekeeperTemplates,
		EndpointStorage:            e.EndpointStorage,
		EndpointStorages:           e.EndpointStorages,
		ControlPlaneAPI:            &api,
		ControlPlaneHooksAPI:       e.ControlPlaneHooksAPI,
		ControlPlaneComputeHookAPI: nil,
		UseLocalTLS:                e.UseLocalTLS,
	}
}

// PersistConfig writes the launcher config to `<base>/config`, mirroring
// Rust `persist_config`.
func (e *LocalEnv) PersistConfig() error {
	onDisk := e.toOnDisk()
	return onDisk.Persist(filepath.Join(e.BaseDataDir, "config"))
}

// pageserverConfigTomlSubset is the subset of pageserver.toml the launcher
// reads at discovery. Unknown keys are ignored (unlike PageServerConf).
type pageserverConfigTomlSubset struct {
	ListenPgAddr    string    `toml:"listen_pg_addr"`
	ListenHTTPAddr  string    `toml:"listen_http_addr"`
	ListenHTTPSAddr *string   `toml:"listen_https_addr"`
	ListenGRPCAddr  *string   `toml:"listen_grpc_addr"`
	PgAuthType      AuthType  `toml:"pg_auth_type"`
	HTTPAuthType    AuthType  `toml:"http_auth_type"`
	GRPCAuthType    AuthType  `toml:"grpc_auth_type"`
	NoSync          bool      `toml:"no_sync"`
	BranchID        *BranchID `toml:"branch_id"`
}

// DiscoverPageservers scans endpoints/*/pageserver/ for provisioned
// pageservers, reconstructing each PageServerConf from its pageserver.toml
// (listen addrs, auth, branch id). The DataDir is the discovered
// endpoints/<name>/pageserver path (which is how a pageserver is
// addressed). Endpoint dirs without a pageserver/ subdir are skipped.
// Mirrors Rust `discover_pageservers`.
func DiscoverPageservers(endpointsPath string) ([]PageServerConf, error) {
	var pageservers []PageServerConf
	entries, err := os.ReadDir(endpointsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return pageservers, nil
		}
		return nil, fmt.Errorf("open endpoints dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dataDir := filepath.Join(endpointsPath, entry.Name(), "pageserver")
		configTomlPath := filepath.Join(dataDir, "pageserver.toml")
		if _, err := os.Stat(configTomlPath); err != nil {
			// Endpoint that doesn't own a pageserver.
			continue
		}
		configBytes, err := os.ReadFile(configTomlPath)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", configTomlPath, err)
		}
		var configToml pageserverConfigTomlSubset
		if err := toml.Unmarshal(configBytes, &configToml); err != nil {
			return nil, fmt.Errorf("parse pageserver.toml %q: %w", configTomlPath, err)
		}
		pageservers = append(pageservers, PageServerConf{
			ListenPgAddr:    configToml.ListenPgAddr,
			ListenHTTPAddr:  configToml.ListenHTTPAddr,
			ListenHTTPSAddr: configToml.ListenHTTPSAddr,
			ListenGRPCAddr:  configToml.ListenGRPCAddr,
			PgAuthType:      configToml.PgAuthType,
			HTTPAuthType:    configToml.HTTPAuthType,
			GRPCAuthType:    configToml.GRPCAuthType,
			NoSync:          configToml.NoSync,
			BranchID:        configToml.BranchID,
			DataDir:         dataDir,
		})
	}
	return pageservers, nil
}

// PageserverConfForBranch returns the pageserver dedicated to branchID (one
// pageserver process per branch), mirroring Rust
// `pageserver_conf_for_timeline`.
func (e *LocalEnv) PageserverConfForBranch(branchID BranchID) *PageServerConf {
	for i := range e.Pageservers {
		if e.Pageservers[i].BranchID != nil && *e.Pageservers[i].BranchID == branchID {
			return &e.Pageservers[i]
		}
	}
	return nil
}

// PageserverConfForEndpoint returns the pageserver co-located under
// endpointID (one pageserver process per endpoint, keyed by its datadir).
func (e *LocalEnv) PageserverConfForEndpoint(endpointID string) (*PageServerConf, error) {
	want := e.PageserverDataDir(endpointID)
	for i := range e.Pageservers {
		if e.Pageservers[i].DataDir == want {
			return &e.Pageservers[i], nil
		}
	}
	var have bytes.Buffer
	for i, ps := range e.Pageservers {
		if i > 0 {
			have.WriteByte(',')
		}
		have.WriteString(ps.DataDir)
	}
	return nil, fmt.Errorf("could not find pageserver for endpoint %q, have %s", endpointID, have.String())
}
