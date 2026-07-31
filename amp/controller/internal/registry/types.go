// Package registry serves the storage-layer control API that yb_amp_local and
// the integration harness drive: the branch and endpoint registries, backed by the
// `amp_registry` schema in the shared yb_storage database. The wire types
// here are JSON-compatible with the launcher's client.
package registry

import "encoding/json"

// SetBranchNameRequest sets (or clears) a branch's name alias.
type SetBranchNameRequest struct {
	BranchName *string `json:"branch_name"`
}

// BranchNameEntry maps a branch id to its human-readable alias.
type BranchNameEntry struct {
	BranchID   string `json:"branch_id"`
	BranchName string `json:"branch_name"`
}

// BranchCreateRecord records how to materialize a branch whose id + name are
// allocated but whose pageserver/safekeeper don't exist yet.
type BranchCreateRecord struct {
	BranchName       string  `json:"branch_name"`
	PgVersion        int     `json:"pg_version"`
	AncestorBranchID *string `json:"ancestor_branch_id"`
	StartLSN         string  `json:"start_lsn"`
}

// BranchRecord is one entry in the branch list, carrying its branch id.
type BranchRecord struct {
	BranchID         string  `json:"branch_id"`
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
