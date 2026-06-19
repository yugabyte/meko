// Package deploy places compute endpoints on a runtime behind a runtime-agnostic
// Deployer interface, so the controller's create-endpoint workflow is identical
// whether endpoints run as Docker containers (today) or Kubernetes pods (later).
// This is the "provisioning interface" of yblight-controller.md §principle 3:
// runtime-specific logic lives here, never in core or the API handlers.
package deploy

import "context"

// YBConn is how an endpoint reaches the YugabyteDB storage universe.
type YBConn struct {
	HostPorts string // host:port[,host:port] as the endpoint sees YB
	User      string
	Password  string
	DBName    string // the storage database (shared with the storage controller)
}

// EndpointSpec is what the controller wants an endpoint to be: a compute bound to
// a (tenant, timeline) on the storage universe, optionally coordinated by a
// storage controller.
type EndpointSpec struct {
	// Name is a stable, unique handle (the container/pod name).
	Name string
	// Storage-layer ids: 32 lowercase hex digits, no dashes (TenantId/TimelineId).
	TenantID   string
	TimelineID string
	PgVersion  int
	YB         YBConn
	// StorageControllerURL, when non-empty, makes the endpoint register its
	// pageserver + safekeeper with that controller and take its generation from
	// the controller's attach-hook. Empty => SC-free self-bootstrap.
	StorageControllerURL string
	// Bootstrap forces initdb of the (supplied) timeline on first start. Ignored
	// for a branch (AncestorTimelineID set): a branch clones from its ancestor.
	Bootstrap bool
	// AncestorTimelineID, when non-empty, makes this a branch: the endpoint's
	// pageserver creates the timeline as a fork of the ancestor at AncestorLSN
	// (lineage reads serve the pre-fork pages) instead of running initdb. The id
	// is 32 lowercase hex digits; AncestorLSN is the storage layer's "X/Y" form.
	AncestorTimelineID string
	AncestorLSN        string
}

// Endpoint is a running compute: an id (for teardown) and the address a client
// connects to over the Postgres wire (user cloud_admin, database postgres).
type Endpoint struct {
	ID      string
	Address string // host:port
}

// Deployer places and removes compute endpoints on a runtime. CreateEndpoint
// blocks until the compute is accepting connections, then returns its address.
type Deployer interface {
	CreateEndpoint(ctx context.Context, spec EndpointSpec) (Endpoint, error)
	DeleteEndpoint(ctx context.Context, id string) error
}
