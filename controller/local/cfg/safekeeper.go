package cfg

import (
	"fmt"
	"time"

	"github.com/yugabyte/amp/controller/local/config"
)

// safekeeper defaults, from compute/libs/safekeeper_api/src/config.rs
// (`SafekeeperConfigToml::default` + the `defaults` module).
const (
	skDefaultHeartbeatTimeout        = 5000 * time.Millisecond // "5s"
	skDefaultGlobalDiskCheck         = 60 * time.Second        // "1m"
	skDefaultControlFileSaveInterval = 300 * time.Second       // "5m"
	skDefaultSslCertReloadPeriod     = 60 * time.Second        // "1m"
	skDefaultScaleFactor             = 1.0
	skDefaultMaxGlobalDiskUsageRatio = 0.0
	skDefaultLogFormat               = "plain"
	skDefaultSslKeyFile              = "server.key"
	skDefaultSslCertFile             = "server.crt"
	skDefaultListenPgAddr            = "127.0.0.1:5454"
	skDefaultListenHTTPAddr          = "127.0.0.1:7676"
)

// safekeeperConfigToml is the Go mirror of
// safekeeper_api::config::SafekeeperConfigToml. Field order matches the Rust
// struct. The `Option::is_none`-skipped fields are pointers with
// `omitempty`; every other field is always emitted (no omitempty), so the
// bool/int/float defaults appear verbatim like the Rust
// `#[serde(default, deny_unknown_fields)]` struct. Durations use
// config.HumantimeDuration so they render in humantime form
// ("5s"/"1m"/"5m"), matching the Rust `#[serde(with = "humantime_serde")]`.
type safekeeperConfigToml struct {
	ID       config.NodeID    `toml:"id"`
	BranchID *config.BranchID `toml:"branch_id,omitempty"`

	ListenPg           string  `toml:"listen_pg"`
	ListenPgTenantOnly *string `toml:"listen_pg_tenant_only,omitempty"`
	ListenHTTP         string  `toml:"listen_http"`
	ListenHTTPS        *string `toml:"listen_https,omitempty"`
	AdvertisePg        *string `toml:"advertise_pg,omitempty"`
	AvailabilityZone   *string `toml:"availability_zone,omitempty"`

	HeartbeatTimeout config.HumantimeDuration `toml:"heartbeat_timeout"`

	YbStorageConn   *string `toml:"yb_storage_conn,omitempty"`
	WalCacheSizeMib *uint64 `toml:"wal_cache_size_mib,omitempty"`
	ScaleFactor     float64 `toml:"scale_factor"`

	MaxTimelineDiskUsageBytes uint64                   `toml:"max_timeline_disk_usage_bytes"`
	GlobalDiskCheckInterval   config.HumantimeDuration `toml:"global_disk_check_interval"`
	MaxGlobalDiskUsageRatio   float64                  `toml:"max_global_disk_usage_ratio"`

	PgAuthPublicKeyPath           *string `toml:"pg_auth_public_key_path,omitempty"`
	PgTenantOnlyAuthPublicKeyPath *string `toml:"pg_tenant_only_auth_public_key_path,omitempty"`
	HTTPAuthPublicKeyPath         *string `toml:"http_auth_public_key_path,omitempty"`
	AuthTokenPath                 *string `toml:"auth_token_path,omitempty"`

	LogFormat               string                   `toml:"log_format"`
	CurrentThreadRuntime    bool                     `toml:"current_thread_runtime"`
	ControlFileSaveInterval config.HumantimeDuration `toml:"control_file_save_interval"`
	WalReaderFanout         bool                     `toml:"wal_reader_fanout"`
	MaxDeltaForFanout       *uint64                  `toml:"max_delta_for_fanout,omitempty"`

	SslKeyFile             string                   `toml:"ssl_key_file"`
	SslCertFile            string                   `toml:"ssl_cert_file"`
	SslCertReloadPeriod    config.HumantimeDuration `toml:"ssl_cert_reload_period"`
	SslCaFile              *string                  `toml:"ssl_ca_file,omitempty"`
	UseHTTPSSafekeeperAPI  bool                     `toml:"use_https_safekeeper_api"`
	EnableTLSWalServiceAPI bool                     `toml:"enable_tls_wal_service_api"`

	ForceMetricCollectionOnScrape bool `toml:"force_metric_collection_on_scrape"`
	Dev                           bool `toml:"dev"`
}

// defaultSafekeeperConfigToml reproduces SafekeeperConfigToml::default.
func defaultSafekeeperConfigToml() safekeeperConfigToml {
	return safekeeperConfigToml{
		ID:                            0,
		ListenPg:                      skDefaultListenPgAddr,
		ListenHTTP:                    skDefaultListenHTTPAddr,
		HeartbeatTimeout:              config.HumantimeDuration(skDefaultHeartbeatTimeout),
		ScaleFactor:                   skDefaultScaleFactor,
		MaxTimelineDiskUsageBytes:     0,
		GlobalDiskCheckInterval:       config.HumantimeDuration(skDefaultGlobalDiskCheck),
		MaxGlobalDiskUsageRatio:       skDefaultMaxGlobalDiskUsageRatio,
		LogFormat:                     skDefaultLogFormat,
		CurrentThreadRuntime:          false,
		ControlFileSaveInterval:       config.HumantimeDuration(skDefaultControlFileSaveInterval),
		WalReaderFanout:               false,
		SslKeyFile:                    skDefaultSslKeyFile,
		SslCertFile:                   skDefaultSslCertFile,
		SslCertReloadPeriod:           config.HumantimeDuration(skDefaultSslCertReloadPeriod),
		UseHTTPSSafekeeperAPI:         false,
		EnableTLSWalServiceAPI:        false,
		ForceMetricCollectionOnScrape: true,
		Dev:                           false,
	}
}

// SafekeeperParams is the input to WriteSafekeeperToml — the per-node bits
// the Rust `SafekeeperNode::start` assembles into a SafekeeperConfigToml
// (control_plane/src/safekeeper.rs ~187-210). The listen addresses are built
// from ListenAddr + the ports; availability_zone is derived as "sk-<id>".
type SafekeeperParams struct {
	ID       config.NodeID
	BranchID *config.BranchID
	// ListenAddr is the bind host (Rust defaults to "127.0.0.1" when the
	// conf's listen_addr is unset); listen_pg/listen_http/… are
	// "<ListenAddr>:<port>".
	ListenAddr       string
	PgPort           uint16
	PgTenantOnlyPort *uint16
	HTTPPort         uint16
	HTTPSPort        *uint16
	// YbStorageConn is the string-encoded YbStore inline table (a plain TOML
	// string value in safekeeper.toml, unlike pageserver.toml's inline
	// table).
	YbStorageConn *string
	// WalCacheSizeMib maps the SafekeeperConf's Option<u32> to the toml
	// usize; nil omits the key (daemon derives it from scale_factor).
	WalCacheSizeMib *uint32
	// ControlFileSaveInterval overrides the default 5m cadence (the
	// YB_SK_CONTROL_FILE_SAVE_INTERVAL env override); nil keeps the default.
	ControlFileSaveInterval *time.Duration

	// Auth (JWT) paths. In the minimal Trust path all are nil. When set,
	// AuthPublicKeyPath populates all three surface key paths (pg /
	// pg_tenant_only / http), matching Rust.
	AuthPublicKeyPath *string
	AuthTokenPath     *string
	SslCaFile         *string
}

// WriteSafekeeperToml writes safekeeper.toml. Mirrors Rust
// `SafekeeperNode::start`, which fills a SafekeeperConfigToml (defaults + the
// per-node overrides) and serializes it with `toml::to_string_pretty`.
func WriteSafekeeperToml(path string, p SafekeeperParams) error {
	cfg := defaultSafekeeperConfigToml()
	cfg.ID = p.ID
	cfg.BranchID = p.BranchID
	listenAddr := p.ListenAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1"
	}
	cfg.ListenPg = fmt.Sprintf("%s:%d", listenAddr, p.PgPort)
	if p.PgTenantOnlyPort != nil {
		s := fmt.Sprintf("%s:%d", listenAddr, *p.PgTenantOnlyPort)
		cfg.ListenPgTenantOnly = &s
	}
	cfg.ListenHTTP = fmt.Sprintf("%s:%d", listenAddr, p.HTTPPort)
	if p.HTTPSPort != nil {
		s := fmt.Sprintf("%s:%d", listenAddr, *p.HTTPSPort)
		cfg.ListenHTTPS = &s
	}
	az := fmt.Sprintf("sk-%d", p.ID)
	cfg.AvailabilityZone = &az
	cfg.YbStorageConn = p.YbStorageConn
	if p.WalCacheSizeMib != nil {
		v := uint64(*p.WalCacheSizeMib)
		cfg.WalCacheSizeMib = &v
	}
	if p.ControlFileSaveInterval != nil {
		cfg.ControlFileSaveInterval = config.HumantimeDuration(*p.ControlFileSaveInterval)
	}
	cfg.PgAuthPublicKeyPath = p.AuthPublicKeyPath
	cfg.PgTenantOnlyAuthPublicKeyPath = p.AuthPublicKeyPath
	cfg.HTTPAuthPublicKeyPath = p.AuthPublicKeyPath
	cfg.AuthTokenPath = p.AuthTokenPath
	cfg.SslCaFile = p.SslCaFile

	data, err := encodeTOML(cfg)
	if err != nil {
		return fmt.Errorf("serialize safekeeper.toml: %w", err)
	}
	return writeFile(path, data)
}
