// Package launch is the lifecycle-orchestration layer of the Go yb_amp_local
// launcher: the Go port of the command handlers in
// control_plane/src/bin/yb_amp_local.rs plus the node-level start/provision
// helpers they call (control_plane/src/{pageserver,safekeeper,controller,
// endpoint,endpoint_storage}.rs).
//
// It sits on top of the lower layers:
//
//   - config — the on-disk config model + LocalEnv + key/JWT helpers,
//   - proc   — background-process spawn/stop + the repo lock,
//   - client — the controller / pageserver / safekeeper / compute_ctl HTTP
//     clients,
//   - cfg    — the per-daemon config-file writers.
//
// Scope is the minimal yb-amp path: external YugabyteDB always, Trust auth
// for the storage daemons, no local TLS, a single pageserver per branch,
// Primary-mode compute. The CLI surface (cmd/yb_amp_local) wires cobra
// subcommands onto the handlers here.
package launch

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/controller/local/config"
)

// DefaultBranchName is the alias given to a root branch (Rust
// DEFAULT_BRANCH_NAME).
const DefaultBranchName = "main"

// defaultPageserverID is the id of the first pageserver (Rust
// DEFAULT_PAGESERVER_ID); it is the fallback the port-allocator reuses for
// the main branch's pageserver.
const defaultPageserverID config.NodeID = 1

// defaultSafekeeperID is the id of the first safekeeper (Rust
// DEFAULT_SAFEKEEPER_ID).
const defaultSafekeeperID config.NodeID = 1

// httpClient is the shared HTTP client for all controller / daemon calls. A
// generous timeout matches the launcher's blocking, one-shot usage.
func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// controllerBase parses env.ControlPlaneAPI (e.g.
// "http://127.0.0.1:1234/upcall/v1/") and returns the bare "host:port" the
// controller listens on, mirroring Rust `Controller::dispatch_inner` (which
// strips the /upcall path prefix and rebuilds host:port).
func controllerBase(env *config.LocalEnv) (string, error) {
	u, err := url.Parse(env.ControlPlaneAPI)
	if err != nil {
		return "", fmt.Errorf("parse control_plane_api %q: %w", env.ControlPlaneAPI, err)
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", fmt.Errorf("control_plane_api %q has no host:port", env.ControlPlaneAPI)
	}
	return host + ":" + port, nil
}

// portOf returns the trailing ":port" of a "host:port" address.
func portOf(addr string) (uint16, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0, fmt.Errorf("no port in address %q", addr)
	}
	p, err := strconv.ParseUint(addr[idx+1:], 10, 16)
	if err != nil {
		return 0, fmt.Errorf("bad port in address %q: %w", addr, err)
	}
	return uint16(p), nil
}

// hostOf returns the leading "host" of a "host:port" address.
func hostOf(addr string) (string, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", fmt.Errorf("no port in address %q", addr)
	}
	return addr[:idx], nil
}

// bumpAddrPort maps `host:port` → `host:(port+delta)`, mirroring Rust
// `bump_addr_port`.
func bumpAddrPort(addr string, delta uint16) (string, error) {
	host, err := hostOf(addr)
	if err != nil {
		return "", err
	}
	port, err := portOf(addr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, port+delta), nil
}

// setAddrPort replaces the port of a `host:port` address.
func setAddrPort(addr string, port uint16) (string, error) {
	host, err := hostOf(addr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, port), nil
}

// safekeepersForTimeline returns the safekeepers serving branchID: the
// dedicated ones if any are provisioned, else the legacy set (no branch_id).
// Mirrors Rust `LocalEnv::safekeepers_for_timeline`.
func safekeepersForTimeline(env *config.LocalEnv, branchID config.BranchID) []config.SafekeeperConf {
	var dedicated []config.SafekeeperConf
	for _, sk := range env.Safekeepers {
		if sk.BranchID != nil && *sk.BranchID == branchID {
			dedicated = append(dedicated, sk)
		}
	}
	if len(dedicated) > 0 {
		return dedicated
	}
	var legacy []config.SafekeeperConf
	for _, sk := range env.Safekeepers {
		if sk.BranchID == nil {
			legacy = append(legacy, sk)
		}
	}
	return legacy
}

// mutableState captures the persisted-mutable fields of a LocalEnv so a
// command can persist the config only when it actually changed, mirroring
// Rust's `original_env != env` guard in `main`. Pageservers are intentionally
// omitted: they are never serialized (rediscovered from pageserver.toml).
type mutableState struct {
	defaultBranch string
	hasDefault    bool
	safekeepers   []config.SafekeeperConf
	endpointStgs  []config.EndpointStorageConf
}

// SnapshotMutable records the persisted-mutable state of env before a command
// runs.
func SnapshotMutable(env *config.LocalEnv) any {
	s := mutableState{
		safekeepers:  append([]config.SafekeeperConf(nil), env.Safekeepers...),
		endpointStgs: append([]config.EndpointStorageConf(nil), env.EndpointStorages...),
	}
	if env.DefaultBranchID != nil {
		s.defaultBranch = *env.DefaultBranchID
		s.hasDefault = true
	}
	return s
}

// MutableChanged reports whether env's persisted-mutable state differs from
// the snapshot taken by SnapshotMutable.
func MutableChanged(before any, env *config.LocalEnv) bool {
	prev, ok := before.(mutableState)
	if !ok {
		return true
	}
	return !reflect.DeepEqual(prev, SnapshotMutable(env).(mutableState))
}

// pgLibEnv is the LD_LIBRARY_PATH/DYLD_LIBRARY_PATH pair pointing at the
// Postgres v18 lib dir, forwarded to child processes that link against libpq
// (the controller). Mirrors the env pairs the Rust node helpers set.
func pgLibEnv(env *config.LocalEnv) map[string]string {
	lib := env.PgLibDir(config.DefaultPgVersion)
	return map[string]string{
		"LD_LIBRARY_PATH":   lib,
		"DYLD_LIBRARY_PATH": lib,
	}
}

// fileExists reports whether path exists (used to skip endpoint_storage in
// compatibility tests where the binary is absent).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
