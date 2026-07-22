package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// YbAmpControllerConf mirrors Rust `YbAmpControllerConf`: the subset of the
// controller's config the launcher knows about. The humantime durations
// serialize as strings ("10s"/"30s"/"1s"). The bool fields have no
// skip rule in Rust, so they are always emitted (no `omitempty`).
type YbAmpControllerConf struct {
	// MaxOffline is the heartbeat timeout before a node is marked offline.
	MaxOffline   HumantimeDuration `toml:"max_offline"`
	MaxWarmingUp HumantimeDuration `toml:"max_warming_up"`
	// Yb is the YugabyteDB connection for the controller's persistence
	// layer. When set, the launcher passes it through as `--yb-config` and
	// skips the local-Postgres bootstrap.
	Yb                           *YbConnConfig     `toml:"yb,omitempty"`
	HeartbeatInterval            HumantimeDuration `toml:"heartbeat_interval"`
	UseHTTPSPageserverAPI        bool              `toml:"use_https_pageserver_api"`
	TimelinesOntoSafekeepers     bool              `toml:"timelines_onto_safekeepers"`
	UseHTTPSSafekeeperAPI        bool              `toml:"use_https_safekeeper_api"`
	UseLocalComputeNotifications bool              `toml:"use_local_compute_notifications"`
	TimelineSafekeeperCount      *int              `toml:"timeline_safekeeper_count,omitempty"`
}

// DefaultYbAmpControllerConf reproduces Rust `YbAmpControllerConf::default`.
func DefaultYbAmpControllerConf() YbAmpControllerConf {
	return YbAmpControllerConf{
		MaxOffline:                   HumantimeDuration(10 * time.Second),
		MaxWarmingUp:                 HumantimeDuration(30 * time.Second),
		Yb:                           nil,
		HeartbeatInterval:            HumantimeDuration(1000 * time.Millisecond),
		UseHTTPSPageserverAPI:        false,
		TimelinesOntoSafekeepers:     true,
		UseHTTPSSafekeeperAPI:        false,
		UseLocalComputeNotifications: true,
		TimelineSafekeeperCount:      nil,
	}
}

// EndpointStorageConf mirrors Rust `EndpointStorageConf`.
type EndpointStorageConf struct {
	// ListenAddr is a socket address ("host:port"), default 127.0.0.1:9993.
	ListenAddr string `toml:"listen_addr"`
	// RemoteStorageJSON, if set, is written verbatim as the `storage_kind`
	// block of endpoint_storage.json.
	RemoteStorageJSON *string `toml:"remote_storage_json,omitempty"`
	// EndpointID is the endpoint whose folder holds this instance's
	// datadir. Empty for the init-time template (skipped when empty).
	EndpointID string `toml:"endpoint_id,omitempty"`
}

// endpointStorageDefaultAddr reproduces Rust
// `ENDPOINT_STORAGE_DEFAULT_ADDR`.
const endpointStorageDefaultAddr = "127.0.0.1:9993"

// DefaultEndpointStorageConf reproduces Rust `EndpointStorageConf::default`.
func DefaultEndpointStorageConf() EndpointStorageConf {
	return EndpointStorageConf{
		ListenAddr:        endpointStorageDefaultAddr,
		RemoteStorageJSON: nil,
		EndpointID:        "",
	}
}

// PageServerConf mirrors Rust `PageServerConf`: the subset of pageserver
// config the launcher tracks. It is never serialized into `.yb_amp/config`
// (the OnDiskConfig `pageservers` field is skip-serializing and rejected
// on load); it is reconstructed by discovery from each pageserver's
// `pageserver.toml`. DataDir is where that file was found (and keyed by the
// endpoint) and is not part of any on-disk table.
type PageServerConf struct {
	ListenPgAddr    string   `toml:"listen_pg_addr"`
	ListenHTTPAddr  string   `toml:"listen_http_addr"`
	ListenHTTPSAddr *string  `toml:"listen_https_addr,omitempty"`
	ListenGRPCAddr  *string  `toml:"listen_grpc_addr,omitempty"`
	PgAuthType      AuthType `toml:"pg_auth_type"`
	HTTPAuthType    AuthType `toml:"http_auth_type"`
	GRPCAuthType    AuthType `toml:"grpc_auth_type"`
	NoSync          bool     `toml:"no_sync"`
	// BranchID is the single branch this pageserver serves, read from the
	// `branch_id` key of its pageserver.toml. Nil until assigned.
	BranchID *BranchID `toml:"branch_id,omitempty"`
	// DataDir is the pageserver's data directory
	// (endpoints/<name>/pageserver), captured at discovery. Not on-disk.
	DataDir string `toml:"-"`
}

// SafekeeperConf mirrors Rust `SafekeeperConf`. It is `#[serde(default)]`
// with no per-field skip rules, so pg_port/http_port/auth_enabled/
// endpoint_id are always emitted; the optional fields are omitted when
// nil.
type SafekeeperConf struct {
	PgPort           uint16  `toml:"pg_port"`
	PgTenantOnlyPort *uint16 `toml:"pg_tenant_only_port,omitempty"`
	HTTPPort         uint16  `toml:"http_port"`
	HTTPSPort        *uint16 `toml:"https_port,omitempty"`
	BackupThreads    *uint32 `toml:"backup_threads,omitempty"`
	AuthEnabled      bool    `toml:"auth_enabled"`
	ListenAddr       *string `toml:"listen_addr,omitempty"`
	// YbStorageConn, if set, is the shared `[*.yb]` block as a TOML inline
	// string that the safekeeper accepts to mirror WAL into YugabyteDB.
	YbStorageConn   *string   `toml:"yb_storage_conn,omitempty"`
	WalCacheSizeMib *uint32   `toml:"wal_cache_size_mib,omitempty"`
	BranchID        *BranchID `toml:"branch_id,omitempty"`
	// EndpointID is always emitted (even empty), matching Rust.
	EndpointID string `toml:"endpoint_id"`
}

// GetComputePort reproduces Rust `SafekeeperConf::get_compute_port`.
func (s *SafekeeperConf) GetComputePort() uint16 {
	if s.PgTenantOnlyPort != nil {
		return *s.PgTenantOnlyPort
	}
	return s.PgPort
}

// YbAmpLocalInitPageserverConf mirrors Rust
// `YbAmpLocalInitPageserverConf`: the init-time pageserver template that
// `endpoint create` clones. `yb_storage_conn` is a dedicated field written
// verbatim into pageserver.toml (it bypasses the flatten round-trip that
// would mangle its inline table). Any keys not otherwise modeled are
// captured in Other (Rust `#[serde(flatten)] other`) so unknown
// pageserver.toml keys survive a round-trip.
//
// This type implements go-toml's unstable Marshaler/Unmarshaler interfaces
// so the flatten works; Load/Persist enable those interfaces on their
// encoder/decoder.
type YbAmpLocalInitPageserverConf struct {
	ListenPgAddr    string
	ListenHTTPAddr  string
	ListenHTTPSAddr *string
	ListenGRPCAddr  *string
	PgAuthType      AuthType
	HTTPAuthType    AuthType
	GRPCAuthType    AuthType
	// NoSync is skipped on the wire when false (Rust skip_serializing_if).
	NoSync bool
	// YbStorageConn is written verbatim as `yb_storage_conn = <value>`.
	YbStorageConn *string
	// Other holds any additional pageserver.toml keys (the serde flatten).
	Other map[string]any
}

// MarshalTOML implements go-toml's unstable.Marshaler. It emits the known
// fields (applying the skip rules) plus the flattened Other keys.
func (c YbAmpLocalInitPageserverConf) MarshalTOML() ([]byte, error) {
	m := map[string]any{
		"listen_pg_addr":   c.ListenPgAddr,
		"listen_http_addr": c.ListenHTTPAddr,
		"pg_auth_type":     string(c.PgAuthType),
		"http_auth_type":   string(c.HTTPAuthType),
		"grpc_auth_type":   string(c.GRPCAuthType),
	}
	if c.ListenHTTPSAddr != nil {
		m["listen_https_addr"] = *c.ListenHTTPSAddr
	}
	if c.ListenGRPCAddr != nil {
		m["listen_grpc_addr"] = *c.ListenGRPCAddr
	}
	if c.NoSync {
		m["no_sync"] = true
	}
	if c.YbStorageConn != nil {
		m["yb_storage_conn"] = *c.YbStorageConn
	}
	for k, v := range c.Other {
		m[k] = v
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalTOML implements go-toml's Unmarshaler. It routes the known keys
// into fields and captures the rest in Other.
func (c *YbAmpLocalInitPageserverConf) UnmarshalTOML(data []byte) error {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return err
	}
	other := map[string]any{}
	for k, v := range m {
		switch k {
		case "listen_pg_addr":
			c.ListenPgAddr, _ = v.(string)
		case "listen_http_addr":
			c.ListenHTTPAddr, _ = v.(string)
		case "listen_https_addr":
			s, _ := v.(string)
			c.ListenHTTPSAddr = &s
		case "listen_grpc_addr":
			s, _ := v.(string)
			c.ListenGRPCAddr = &s
		case "pg_auth_type":
			s, _ := v.(string)
			c.PgAuthType = AuthType(s)
		case "http_auth_type":
			s, _ := v.(string)
			c.HTTPAuthType = AuthType(s)
		case "grpc_auth_type":
			s, _ := v.(string)
			c.GRPCAuthType = AuthType(s)
		case "no_sync":
			c.NoSync, _ = v.(bool)
		case "yb_storage_conn":
			s, _ := v.(string)
			c.YbStorageConn = &s
		default:
			other[k] = v
		}
	}
	if len(other) > 0 {
		c.Other = other
	}
	return nil
}

// OnDiskConfig is the on-disk state stored in `.yb_amp/config` (TOML). It
// mirrors Rust `OnDiskConfig` field-for-field for a byte-compatible
// round-trip, including the skip rules:
//   - `pageservers` is never serialized and is rejected on load (the
//     pageserver.toml files are authoritative);
//   - `endpoint_storages` is omitted when empty;
//   - `use_local_tls` is omitted when false;
//   - the humantime durations in `controller` serialize as strings.
//
// The pageservers field is intentionally absent from this struct; Load
// rejects a `pageservers` key explicitly.
type OnDiskConfig struct {
	PgDistribDir               string                         `toml:"pg_distrib_dir"`
	YbAmpDistribDir            string                         `toml:"yb_amp_distrib_dir"`
	DefaultBranchID            *BranchID                      `toml:"default_branch_id,omitempty"`
	PrivateKeyPath             string                         `toml:"private_key_path"`
	PublicKeyPath              string                         `toml:"public_key_path"`
	Controller                 YbAmpControllerConf            `toml:"controller"`
	Safekeepers                []SafekeeperConf               `toml:"safekeepers"`
	PageserverTemplates        []YbAmpLocalInitPageserverConf `toml:"pageserver_templates"`
	SafekeeperTemplates        []SafekeeperConf               `toml:"safekeeper_templates"`
	EndpointStorage            EndpointStorageConf            `toml:"endpoint_storage"`
	EndpointStorages           []EndpointStorageConf          `toml:"endpoint_storages,omitempty"`
	ControlPlaneAPI            *string                        `toml:"control_plane_api,omitempty"`
	ControlPlaneHooksAPI       *string                        `toml:"control_plane_hooks_api,omitempty"`
	ControlPlaneComputeHookAPI *string                        `toml:"control_plane_compute_hook_api,omitempty"`
	UseLocalTLS                bool                           `toml:"use_local_tls,omitempty"`
}

// defaultOnDiskConfig returns an OnDiskConfig pre-populated with the same
// field defaults serde applies via `#[serde(default)]`. Load decodes into
// this so that keys absent from the file keep their non-zero defaults
// (notably the controller durations and the endpoint_storage listen addr).
func defaultOnDiskConfig() OnDiskConfig {
	return OnDiskConfig{
		Controller:      DefaultYbAmpControllerConf(),
		EndpointStorage: DefaultEndpointStorageConf(),
	}
}

// Load reads and parses the TOML config file at path into an OnDiskConfig.
// It reproduces the Rust loader's rejection of a `pageservers` key.
func Load(path string) (*OnDiskConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return unmarshalOnDisk(data)
}

func unmarshalOnDisk(data []byte) (*OnDiskConfig, error) {
	// Reproduce Rust `fail_if_pageservers_field_specified`: reject a
	// `pageservers` key. pageserver.toml is now authoritative.
	var probe map[string]any
	if err := toml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if _, ok := probe["pageservers"]; ok {
		return nil, fmt.Errorf(
			"the 'pageservers' field is no longer used; pageserver.toml is now authoritative; " +
				"please remove the `pageservers` from your .yb_amp/config")
	}

	cfg := defaultOnDiskConfig()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.EnableUnmarshalerInterface()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Persist serializes the config to TOML and writes it to path.
func (c *OnDiskConfig) Persist(path string) error {
	data, err := c.marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write config file into path %q: %w", path, err)
	}
	return nil
}

func (c *OnDiskConfig) marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.EnableMarshalerInterface() // for the pageserver_templates flatten
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("serialize config: %w", err)
	}
	return buf.Bytes(), nil
}
