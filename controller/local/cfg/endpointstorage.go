package cfg

import (
	"encoding/json"
	"fmt"

	"github.com/yugabyte/amp/controller/local/config"
)

// EndpointStorageParams is the input to WriteEndpointStorageJSON. Mirrors
// the fields Rust `EndpointStorage::from_conf` + `init` use
// (control_plane/src/endpoint_storage.rs).
type EndpointStorageParams struct {
	// Listen is the "host:port" the service binds (Rust `listen_addr()`).
	Listen string
	// Pemfile is the path to the JWT public key PEM (env.get_public_key_path).
	Pemfile string
	// RemoteStorageJSON, when set, is the verbatim JSON object for the
	// storage_kind block. When nil, YbConn must be set and the default
	// YbStore block is derived from it.
	RemoteStorageJSON *string
	// YbConn is the controller.yb block, used to derive the default YbStore
	// JSON when RemoteStorageJSON is nil.
	YbConn *config.YbConnConfig
}

// WriteEndpointStorageJSON writes endpoint_storage.json:
// `{"listen", "pemfile", "yb_storage_conn": <storage_kind>}`. Mirrors Rust
// `EndpointStorage::init` + `default_yb_store_json`.
func WriteEndpointStorageJSON(path string, p EndpointStorageParams) error {
	var storageObj any
	if p.RemoteStorageJSON != nil {
		if err := json.Unmarshal([]byte(*p.RemoteStorageJSON), &storageObj); err != nil {
			return fmt.Errorf("parse remote_storage_json: %w", err)
		}
	} else {
		if p.YbConn == nil {
			return fmt.Errorf(
				"endpoint_storage: RemoteStorageJSON is unset and YbConn is also unset; " +
					"cannot derive a default YbStore config")
		}
		storageObj = ybConnToStoreJSON(p.YbConn)
	}

	cfg := map[string]any{
		"listen":          p.Listen,
		"pemfile":         p.Pemfile,
		"yb_storage_conn": storageObj,
	}
	// Go's json.Marshal sorts map keys, matching serde_json's default
	// (BTreeMap) ordering — so the bytes line up with the Rust output too.
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("serialize endpoint_storage.json: %w", err)
	}
	return writeFile(path, data)
}

// ybConnToStoreJSON serializes a YbConnConfig to the JSON object the
// endpoint_storage service deserializes, adding the `type = "YbStore"`
// discriminator. The field names + skip rules match the Rust
// `ysql_client::YbConnConfig` serde impl (skip when None / false / default).
func ybConnToStoreJSON(c *config.YbConnConfig) map[string]any {
	// serde serializes user/dbname unconditionally (their `default` only
	// applies on deserialize); emit the raw field values.
	obj := map[string]any{
		"host_ports": c.HostPorts,
		"user":       c.User,
		"dbname":     c.Dbname,
	}
	if c.PasswordEnv != nil {
		obj["password_env"] = *c.PasswordEnv
	}
	if c.Sslmode != nil {
		obj["sslmode"] = *c.Sslmode
	}
	if c.Sslrootcert != nil {
		obj["sslrootcert"] = *c.Sslrootcert
	}
	if c.DisableTransactionalWrites {
		obj["disable_transactional_writes"] = true
	}
	if c.HotPath != nil && *c.HotPath != "tsclient" {
		obj["hot_path"] = *c.HotPath
	}
	if c.TserverRPCAddrs != nil {
		obj["tserver_rpc_addrs"] = *c.TserverRPCAddrs
	}
	obj["type"] = "YbStore"
	return obj
}
