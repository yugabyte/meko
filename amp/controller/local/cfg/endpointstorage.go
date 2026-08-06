package cfg

import (
	"encoding/json"
	"fmt"

	"github.com/yugabyte/amp/controller/local/config"
)

// defaultHotPath mirrors the Rust `HotPath::default()` (and
// `launch.DefaultYbHotPath`): the lane an absent `hot_path` key selects, so it
// is never rendered. Duplicated rather than imported because `launch` already
// imports this package.
const defaultHotPath = "yb_thin_client"

// EndpointStorageParams is the input to WriteEndpointStorageJSON.
type EndpointStorageParams struct {
	// Listen is the "host:port" the service binds.
	Listen string
	// YbConn is the controller.yb block the YbStore config is derived from.
	YbConn *config.YbConnConfig
}

// WriteEndpointStorageJSON writes the endpoint_storage.json the service parses:
// `{"listen", "yb_storage_conn": <YbStore block>}`.
func WriteEndpointStorageJSON(path string, p EndpointStorageParams) error {
	if p.YbConn == nil {
		return fmt.Errorf(
			"endpoint_storage: YbConn is unset; cannot derive the YbStore config")
	}

	cfg := map[string]any{
		"listen":          p.Listen,
		"yb_storage_conn": ybConnToStoreJSON(p.YbConn),
	}
	// Go's json.Marshal sorts map keys, which is the ordering serde_json
	// produces on the reading side too.
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
	// Only a non-default lane is emitted, mirroring
	// `launch.ybInlineTOML`. (endpoint_storage's YbStore backend talks YSQL
	// regardless — it holds no hot-path client — but the key stays in the
	// shared connection block, so keep the two renderers consistent.)
	if c.HotPath != nil && *c.HotPath != defaultHotPath {
		obj["hot_path"] = *c.HotPath
	}
	if c.TserverRPCAddrs != nil {
		obj["tserver_rpc_addrs"] = *c.TserverRPCAddrs
	}
	obj["type"] = "YbStore"
	return obj
}
