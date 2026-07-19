// Package cfg writes the per-daemon config files the yb-amp launcher
// materializes under `.yb_amp/endpoints/<id>/` before spawning each storage
// daemon. It is the Go port of the file-writing halves of the Rust
// `control_plane` launcher:
//
//   - pageserver.toml / identity.toml / metadata.json
//     (control_plane/src/pageserver.rs)
//   - safekeeper.toml (control_plane/src/safekeeper.rs, whose shape is
//     safekeeper_api::config::SafekeeperConfigToml)
//   - endpoint_storage.json (control_plane/src/endpoint_storage.rs)
//   - the compute's config.json + in-spec postgresql.conf
//     (control_plane/src/endpoint.rs, whose shapes are
//     compute_api::spec::ComputeSpec / responses::ComputeConfig)
//
// These files are parsed by the Rust daemons, so field names, TOML/JSON
// types and nesting must match the Rust structs exactly — a wrong key or a
// string-where-a-table-is-expected silently breaks the daemon. The tests
// assert semantic equality against real golden files captured from a live
// `yb_amp_local` bring-up (see testdata/golden).
//
// Scope is the minimal yb-amp launcher path: external YugabyteDB (a YbStore
// connection is mandatory), Trust auth for the storage daemons, no local
// TLS, a single pageserver, Primary-mode compute. The JWT/TLS knobs are
// still modeled as optional params so the writers stay faithful, but the
// launcher never sets them in the minimal port.
package cfg

import (
	"fmt"
	"os"
)

// writeFile writes data to path with 0o644 perms, wrapping errors with the
// path (matching the launcher's context-rich error messages).
func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
