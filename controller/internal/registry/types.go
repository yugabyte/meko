// Package registry serves the storage-layer control API that yb_light_local and
// the integration harness drive: generation authority (attach-hook) plus the
// branch / node / endpoint / pending-init registries, backed by the
// `registry` schema in the shared yb_storage database. The wire types
// here are JSON-compatible with the launcher's client (control_plane) and with
// pageserver_api::controller_api.
package registry

import "encoding/json"

// AttachHookRequest bumps (or, for a detach, clears) a branch's generation.
// node_id present = attach to that pageserver and increment; absent = detach.
// generation_override / config apply only when the branch row is first created.
type AttachHookRequest struct {
	BranchID           string          `json:"branch_id"`
	NodeID             *int64          `json:"node_id"`
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

// InspectResponse reports the current (generation, node_id) attachment as a
// two-element array, or null when the branch is unattached.
type InspectResponse struct {
	Attachment *[2]uint64 `json:"attachment"`
}

// NodeRegisterRequest self-registers a pageserver into the node registry.
type NodeRegisterRequest struct {
	NodeID          int64   `json:"node_id"`
	ListenPgAddr    string  `json:"listen_pg_addr"`
	ListenPgPort    uint16  `json:"listen_pg_port"`
	ListenGrpcAddr  *string `json:"listen_grpc_addr"`
	ListenGrpcPort  *uint16 `json:"listen_grpc_port"`
	ListenHTTPAddr  string  `json:"listen_http_addr"`
	ListenHTTPPort  uint16  `json:"listen_http_port"`
	ListenHTTPSPort *uint16 `json:"listen_https_port"`
	NodeIPAddr      *string `json:"node_ip_addr"`
}

// NodeConfigureRequest updates a node's availability. availability is one of
// "Active" / "WarmingUp" / "Offline", or null to leave it unchanged.
type NodeConfigureRequest struct {
	NodeID       int64   `json:"node_id"`
	Availability *string `json:"availability"`
}

// NodeDescribeResponse is one entry in the node-list response.
type NodeDescribeResponse struct {
	ID              int64   `json:"id"`
	Availability    string  `json:"availability"`
	ListenHTTPAddr  string  `json:"listen_http_addr"`
	ListenHTTPPort  uint16  `json:"listen_http_port"`
	ListenHTTPSPort *uint16 `json:"listen_https_port"`
	ListenPgAddr    string  `json:"listen_pg_addr"`
	ListenPgPort    uint16  `json:"listen_pg_port"`
	ListenGrpcAddr  *string `json:"listen_grpc_addr"`
	ListenGrpcPort  *uint16 `json:"listen_grpc_port"`
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

// BranchCreateResponseShard describes a branch's attachment (its owning node and
// current generation).
type BranchCreateResponseShard struct {
	BranchID   string `json:"branch_id"`
	NodeID     int64  `json:"node_id"`
	Generation uint32 `json:"generation"`
}

// BranchCreateResponse is returned by the branch create / import endpoints.
type BranchCreateResponse struct {
	Shards []BranchCreateResponseShard `json:"shards"`
}
