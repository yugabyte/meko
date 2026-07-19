package cfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"

	"github.com/yugabyte/amp/controller/local/config"
)

// PageserverParams is the input to WritePageserverToml: the init-time
// pageserver template plus the per-node overrides the Rust
// `pageserver_init_make_toml` injects (control_plane/src/pageserver.rs
// ~90-186). The written pageserver.toml carries ONLY these keys — it is not
// a full pageserver_api::config::ConfigToml; the daemon fills every other
// field from its own defaults.
type PageserverParams struct {
	// Conf is the init-time template (id, listen addrs, auth types, no_sync,
	// yb_storage_conn, and any passthrough Other keys). Its `id` is written
	// to identity.toml, NOT pageserver.toml (the Rust code removes it).
	Conf config.YbAmpLocalInitPageserverConf

	// PgDistribDir is written as `pg_distrib_dir` (the Postgres install root).
	PgDistribDir string
	// SafekeeperPgAddr is the host:port of the co-located safekeeper this
	// pageserver streams WAL from, written as `safekeeper_pg_addr`.
	SafekeeperPgAddr string
	// ControlPlaneAPI is the controller upcall base URL, written as
	// `control_plane_api`.
	ControlPlaneAPI string
	// ControlPlaneAPIToken, when set (JWT auth), is written as
	// `control_plane_api_token`. Nil in the minimal Trust path.
	ControlPlaneAPIToken *string
	// AuthValidationPublicKeyPath, when set (any surface uses JWT), is
	// written as `auth_validation_public_key_path`. Nil in Trust path.
	AuthValidationPublicKeyPath *string
	// SslCaFile, when set (local TLS), is written as `ssl_ca_file`. Nil in
	// the no-TLS path.
	SslCaFile *string
}

// WritePageserverToml writes a fresh pageserver.toml (no identity block).
// Mirrors Rust `PageServerNode::pageserver_init_make_toml` +
// `pageserver_init`: the file it produces is the set of override fragments
// (pg_distrib_dir / safekeeper_pg_addr / control_plane_api / optional JWT +
// TLS keys / verbatim yb_storage_conn) plus the serialized init conf with
// `id` and `yb_storage_conn` removed. The single-branch identity
// (branch_id / generation / [timeline_init]) is baked in afterwards by
// SetPageserverIdentity, matching the Rust two-step (init then
// set_timeline_identity).
//
// yb_storage_conn is required (LocalFs was removed): the RemoteStorageConfig
// inline table the daemon reads to reach YugabyteDB. It is stored as a
// verbatim TOML inline-table string on the template; this writer re-parses
// it into a structured sub-table. That is byte-different from the Rust
// verbatim write but semantically identical — the pageserver's
// unknown-field check (compute/pageserver/src/config/ignored_fields.rs)
// compares dotted key paths and treats an inline table and a `[table]`
// identically, so both forms are accepted.
func WritePageserverToml(path string, p PageserverParams) error {
	doc, err := pageserverTomlMap(p)
	if err != nil {
		return err
	}
	data, err := encodeTOML(doc)
	if err != nil {
		return fmt.Errorf("serialize pageserver.toml: %w", err)
	}
	return writeFile(path, data)
}

// pageserverTomlMap builds the ordered key set of a fresh pageserver.toml.
func pageserverTomlMap(p PageserverParams) (map[string]any, error) {
	c := p.Conf
	if c.YbStorageConn == nil {
		return nil, fmt.Errorf(
			"pageserver yb_storage_conn must be configured (LocalFs was removed): " +
				"set PageserverParams.Conf.YbStorageConn to a YbStore inline table")
	}
	ybConn, err := parseInlineTable(*c.YbStorageConn)
	if err != nil {
		return nil, fmt.Errorf("parse pageserver yb_storage_conn %q: %w", *c.YbStorageConn, err)
	}

	m := map[string]any{
		// Override fragments (Rust `overrides` vec).
		"pg_distrib_dir":     p.PgDistribDir,
		"safekeeper_pg_addr": p.SafekeeperPgAddr,
		"control_plane_api":  p.ControlPlaneAPI,
		"yb_storage_conn":    ybConn,
		// Serialized init conf, minus `id` and `yb_storage_conn`.
		"listen_pg_addr":   c.ListenPgAddr,
		"listen_http_addr": c.ListenHTTPAddr,
		"pg_auth_type":     string(c.PgAuthType),
		"http_auth_type":   string(c.HTTPAuthType),
		"grpc_auth_type":   string(c.GRPCAuthType),
	}
	if p.ControlPlaneAPIToken != nil {
		m["control_plane_api_token"] = *p.ControlPlaneAPIToken
	}
	if p.AuthValidationPublicKeyPath != nil {
		m["auth_validation_public_key_path"] = *p.AuthValidationPublicKeyPath
	}
	if p.SslCaFile != nil {
		m["ssl_ca_file"] = *p.SslCaFile
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
	for k, v := range c.Other {
		m[k] = v
	}
	return m, nil
}

// TimelineInit is the on-disk `[timeline_init]` block of pageserver.toml:
// how to create the configured branch's timeline when its
// `pageserver.timeline_state` row does not exist yet. Mirrors Rust
// `pageserver_api::config::TimelineInitConfig` (serde tag = "mode",
// rename_all = "snake_case").
//
// NOTE: the Branch variant carries ancestor_branch_id + ancestor_lsn ONLY
// (no pg_version); ancestor_lsn serializes as its "X/Y" string.
type TimelineInit struct {
	// Mode is "bootstrap" or "branch".
	Mode string
	// PgVersion is set for Mode=="bootstrap" (serialized as a bare integer).
	PgVersion int
	// AncestorBranchID / AncestorLsn are set for Mode=="branch".
	AncestorBranchID string
	AncestorLsn      config.Lsn
}

const (
	timelineInitBootstrap = "bootstrap"
	timelineInitBranch    = "branch"
)

// BootstrapInit builds the Bootstrap timeline-init recipe (run initdb and
// import the resulting pgdata).
func BootstrapInit(pgVersion int) TimelineInit {
	return TimelineInit{Mode: timelineInitBootstrap, PgVersion: pgVersion}
}

// BranchInit builds the Branch timeline-init recipe (point at an ancestor
// branch at ancestorLsn).
func BranchInit(ancestorBranchID config.BranchID, ancestorLsn config.Lsn) TimelineInit {
	return TimelineInit{
		Mode:             timelineInitBranch,
		AncestorBranchID: ancestorBranchID,
		AncestorLsn:      ancestorLsn,
	}
}

// toMap renders the TimelineInit as the `[timeline_init]` table.
func (t TimelineInit) toMap() (map[string]any, error) {
	switch t.Mode {
	case timelineInitBootstrap:
		return map[string]any{"mode": timelineInitBootstrap, "pg_version": int64(t.PgVersion)}, nil
	case timelineInitBranch:
		return map[string]any{
			"mode":               timelineInitBranch,
			"ancestor_branch_id": t.AncestorBranchID,
			"ancestor_lsn":       t.AncestorLsn,
		}, nil
	default:
		return nil, fmt.Errorf("unknown timeline_init mode %q", t.Mode)
	}
}

// SetPageserverIdentity bakes the single-branch identity into an existing
// pageserver.toml (read-modify-write), injecting/overwriting `branch_id`,
// `generation` and — when init is non-nil — the `[timeline_init]` table.
// Any other keys already in the file are preserved. Mirrors Rust
// `PageServerNode::set_timeline_identity` + `merge_config_fragment`.
func SetPageserverIdentity(path string, branchID config.BranchID, generation uint32, init *TimelineInit) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %q: %w", path, err)
	}
	doc["branch_id"] = branchID
	doc["generation"] = int64(generation)
	if init != nil {
		initMap, err := init.toMap()
		if err != nil {
			return err
		}
		doc["timeline_init"] = initMap
	}
	out, err := encodeTOML(doc)
	if err != nil {
		return fmt.Errorf("serialize %q: %w", path, err)
	}
	return writeFile(path, out)
}

// WriteIdentityToml writes identity.toml (`id = <NodeId>`). Mirrors Rust
// `pageserver_make_identity_toml`.
func WriteIdentityToml(path string, id config.NodeID) error {
	// Rust emits `id=<n>` via toml_edit; a plain `id = <n>` parses
	// identically. Format by hand to keep it a bare integer.
	return writeFile(path, []byte(fmt.Sprintf("id = %d\n", id)))
}

// NodeMetadata is the metadata.json the pageserver reads on startup to
// register with the controller. Mirrors Rust
// `pageserver_api::config::NodeMetadata` field-for-field, including the
// `host`/`port` renames (from postgres_host/postgres_port). Every field is
// emitted (grpc_host/grpc_port/https_port as null when nil). The Rust type
// has a `#[serde(flatten)] other`; the launcher always passes an empty map,
// so no extra keys are written.
type NodeMetadata struct {
	PostgresHost string  `json:"host"`
	PostgresPort uint16  `json:"port"`
	GrpcHost     *string `json:"grpc_host"`
	GrpcPort     *uint16 `json:"grpc_port"`
	HTTPHost     string  `json:"http_host"`
	HTTPPort     uint16  `json:"http_port"`
	HTTPSPort    *uint16 `json:"https_port"`
	// Other holds any deployment-tool extras (the serde flatten). Empty in
	// the launcher; when non-empty its keys are merged at the top level.
	Other map[string]any `json:"-"`
}

// WriteMetadataJSON writes metadata.json. Mirrors Rust `pageserver_init`'s
// hand-crafted `serde_json::to_vec(&NodeMetadata{...})` (compact JSON).
func WriteMetadataJSON(path string, m NodeMetadata) error {
	data, err := marshalNodeMetadata(m)
	if err != nil {
		return fmt.Errorf("serialize metadata.json: %w", err)
	}
	return writeFile(path, data)
}

func marshalNodeMetadata(m NodeMetadata) ([]byte, error) {
	if len(m.Other) == 0 {
		return json.Marshal(m)
	}
	// Merge the flattened `other` keys at the top level, matching serde's
	// `#[serde(flatten)]`.
	base, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	for k, v := range m.Other {
		obj[k] = v
	}
	return json.Marshal(obj)
}

// parseInlineTable parses a TOML inline-table literal (e.g.
// `{ host_ports = "127.0.0.1:5433", user = "yugabyte" }`) into a map.
func parseInlineTable(s string) (map[string]any, error) {
	var m map[string]any
	if err := toml.Unmarshal([]byte("v = "+s), &m); err != nil {
		return nil, err
	}
	v, ok := m["v"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("not an inline table: %q", s)
	}
	return v, nil
}

// encodeTOML marshals v to TOML with the same encoder settings the config
// package uses (unstable Marshaler interface enabled for flatten types).
func encodeTOML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.EnableMarshalerInterface()
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
