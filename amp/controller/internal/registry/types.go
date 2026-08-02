// Package registry serves the storage-layer control API that yb_amp_local and
// the integration harness drive. It carries the launcher's endpoint records plus
// the branch-state seeding the controller's own task framework calls in-process;
// branches are ordinary /api/v1 resources. The wire types here are
// JSON-compatible with the launcher's client.
package registry

import "encoding/json"

// BranchCreateRecord records how to materialize a branch whose id + name are
// allocated but whose pageserver/safekeeper don't exist yet.
type BranchCreateRecord struct {
	BranchName       string  `json:"branch_name"`
	PgVersion        int     `json:"pg_version"`
	AncestorBranchID *string `json:"ancestor_branch_id"`
	StartLSN         string  `json:"start_lsn"`
}

// ComputeEndpoint is a persisted compute-endpoint record; spec is opaque JSON.
type ComputeEndpoint struct {
	EndpointID string          `json:"endpoint_id"`
	BranchID   string          `json:"branch_id"`
	Spec       json.RawMessage `json:"spec"`
}
