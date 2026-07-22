// Package registry serves the storage-layer control API that yb_amp_local and
// the integration harness drive: generation authority (attach-hook) plus the
// branch / endpoint / pending-init registries, backed by the
// `registry` schema in the shared yb_storage database. The wire types
// here are JSON-compatible with the launcher's client.
package registry

import "encoding/json"

// AttachHookRequest bumps (attach) or clears (detach) a branch's generation.
// attach = true increments the generation; false detaches (the generation
// counter is preserved, staying monotonic across the branch's lifecycle).
// generation_override / config apply only when the branch row is first created.
type AttachHookRequest struct {
	BranchID           string          `json:"branch_id"`
	Attach             bool            `json:"attach"`
	GenerationOverride *int32          `json:"generation_override"`
	Config             json.RawMessage `json:"config"`
}

// AttachHookResponse carries the newly issued generation (null on detach). The
// wire field is "gen".
type AttachHookResponse struct {
	Generation *uint32 `json:"gen"`
}

// InspectRequest asks for a branch's current attachment.
type InspectRequest struct {
	BranchID string `json:"branch_id"`
}

// InspectResponse reports the branch's current generation, or null when the
// branch is unattached.
type InspectResponse struct {
	Generation *uint32 `json:"generation"`
}

// BranchRegisterRequest records a launcher-created branch's start LSN, and
// optionally sets its branch-name alias in the same write.
type BranchRegisterRequest struct {
	StartLSN   string  `json:"start_lsn"`
	BranchName *string `json:"branch_name"`
}

// SetBranchNameRequest sets (or clears) a branch's name alias.
type SetBranchNameRequest struct {
	BranchName *string `json:"branch_name"`
}

// BranchNameEntry maps a branch id to its human-readable alias.
type BranchNameEntry struct {
	BranchID   string `json:"branch_id"`
	BranchName string `json:"branch_name"`
}

// PendingBranchInitRequest records the recipe for a branch whose id + name are
// allocated but whose pageserver/safekeeper don't exist yet.
type PendingBranchInitRequest struct {
	BranchName       string  `json:"branch_name"`
	PgVersion        int     `json:"pg_version"`
	AncestorBranchID *string `json:"ancestor_branch_id"`
	StartLSN         string  `json:"start_lsn"`
}

// PendingBranchInit is one entry in the pending-init list, carrying its
// branch id.
type PendingBranchInit struct {
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

// BranchCreateResponseShard describes a branch's current generation.
type BranchCreateResponseShard struct {
	BranchID   string `json:"branch_id"`
	Generation uint32 `json:"generation"`
}

// BranchCreateResponse is returned by the branch create / import endpoints.
type BranchCreateResponse struct {
	Shards []BranchCreateResponseShard `json:"shards"`
}
