// Package deploy places compute endpoints on a runtime behind a runtime-agnostic
// Deployer interface, so the controller's create-endpoint workflow is identical
// whether endpoints run as Docker containers (today) or Kubernetes pods (later).
// This is the "provisioning interface" of amp-controller.md §principle 3:
// runtime-specific logic lives here, never in core or the API handlers.
package deploy

import "context"

// safekeeperHTTPPort is the port every endpoint's co-located safekeeper serves
// its HTTP API on. The same in both runtimes — the Docker container's internal
// port and the Helm chart's ports.safekeeperHttp — so the controller can address
// it uniformly from an endpoint's reachable host.
const safekeeperHTTPPort = 7676

// YBConn is how an endpoint reaches the YugabyteDB storage universe.
type YBConn struct {
	HostPorts string // host:port[,host:port] as the endpoint sees YB
	User      string
	Password  string
	DBName    string // the storage database (shared with the controller)
}

// EndpointSpec is what the controller wants an endpoint to be: a compute bound to
// a branch on the storage universe, optionally coordinated by a storage
// controller.
type EndpointSpec struct {
	// Name is a stable, unique handle (the container/pod name).
	Name string
	// BranchID is the storage-layer branch id this compute serves: 32 lowercase
	// hex digits, no dashes (the flat single-id storage contract — no tenant).
	BranchID  string
	PgVersion int
	YB        YBConn
	// ControllerURL, when non-empty, makes the endpoint register its
	// pageserver + safekeeper with that controller and take its generation from
	// the controller's attach-hook. Empty => controller-free self-bootstrap.
	ControllerURL string
	// Bootstrap forces initdb of the (supplied) branch on first start. Ignored
	// for a branch fork (AncestorBranchID set): a fork clones from its ancestor.
	Bootstrap bool
	// AncestorBranchID, when non-empty, makes this a fork: the endpoint's
	// pageserver creates the branch as a fork of the ancestor at AncestorLSN
	// (lineage reads serve the pre-fork pages) instead of running initdb. The id
	// is 32 lowercase hex digits; AncestorLSN is the storage layer's "X/Y" form.
	AncestorBranchID string
	AncestorLSN      string
	// CPU and Memory are the compute reserved for the endpoint pod, as Kubernetes
	// resource quantities (e.g. "2", "4Gi"). Each is applied as BOTH the request
	// and the limit on the endpoint container, so the pod gets Guaranteed QoS.
	// Empty defaults to 2 cores / 4Gi at the deployer (matching the chart default).
	CPU    string
	Memory string
}

// ResizeSpec is a request to change an existing endpoint's compute reservation.
type ResizeSpec struct {
	// Name is the endpoint's deploy/release name (Endpoint.ID / EndpointSpec.Name).
	Name string
	// CPU and Memory are the new Kubernetes resource quantities (e.g. "4", "8Gi"),
	// applied as BOTH request and limit on the endpoint container so the pod keeps
	// its Guaranteed QoS class (in-place resize cannot change QoS).
	CPU    string
	Memory string
}

// ResizeOutcome is the state of the in-place resize of the running pod.
type ResizeOutcome string

const (
	// ResizeCompleted: the kubelet applied the new resources to the running pod
	// with no restart.
	ResizeCompleted ResizeOutcome = "completed"
	// ResizePending: the resize was accepted but not yet applied (e.g. the node
	// lacks spare capacity right now). The kubelet retries the in-place resize
	// when capacity frees (no restart). The new size is also persisted to config,
	// so a *controller-driven* redeploy lands it. NB: an involuntary pod recreate
	// (node loss / eviction) does NOT — the Deployment template still holds the
	// old size until a redeploy re-renders it (tracked: bare-ReplicaSet follow-up).
	ResizePending ResizeOutcome = "pending"
	// ResizeInfeasible: the kubelet cannot apply the resize on the current node.
	ResizeInfeasible ResizeOutcome = "infeasible"
	// ResizeUnsupported: the runtime does not support in-place resize (Docker).
	ResizeUnsupported ResizeOutcome = "unsupported"
)

// ResizeResult reports the outcome of the in-place resize of the running pod.
// It is best-effort status: even when the in-place step is Pending/Infeasible,
// the new size is persisted to the endpoint config, so the endpoint converges to
// it on the next *controller-driven* redeploy (not on an involuntary recreate —
// see the ResizePending note above).
type ResizeResult struct {
	Outcome ResizeOutcome
	Message string
}

// Endpoint is a running compute: an id (for teardown) and the address a client
// connects to over the Postgres wire (user cloud_admin, database postgres).
type Endpoint struct {
	ID      string
	Address string // host:port — the primary (e.g. Kubernetes LoadBalancer) address
	// TailnetAddress is the endpoint's Tailscale (tailnet) host:port, populated
	// when the endpoint Service is exposed on the tailnet (see KubernetesConfig.
	// TailnetSuffix). Empty when not tailnet-exposed.
	TailnetAddress string
}

// Deployer places and removes compute endpoints on a runtime. CreateEndpoint
// blocks until the compute is accepting connections, then returns its address.
type Deployer interface {
	CreateEndpoint(ctx context.Context, spec EndpointSpec) (Endpoint, error)
	DeleteEndpoint(ctx context.Context, id string) error
	// ResizeEndpoint changes the compute reservation of an existing endpoint by
	// resizing the running pod IN PLACE via the pod `resize` subresource (k8s
	// 1.33+), returning whether that in-place step completed. It does NOT patch the
	// workload template (that would trigger a rollout and recreate the pod);
	// durability across a controller-driven redeploy is the caller's job (it
	// persists the size to config). Idempotent. Runtimes without in-place support
	// return ResizeUnsupported.
	ResizeEndpoint(ctx context.Context, spec ResizeSpec) (ResizeResult, error)
	// EndpointSafekeeperURL returns the base URL of the co-located safekeeper's
	// HTTP API for the endpoint identified by id (the Endpoint.ID CreateEndpoint
	// returned — the deploy/release name). The controller reaches it over the same
	// network/DNS the controller dials (CONTROLLER_REACHABLE_HOST), at the fixed
	// safekeeper HTTP port, to resolve a branch's default fork point from the
	// ancestor's own safekeeper (#133). It is a pure function of id and the
	// deployer's static config; it does not check that the endpoint is live.
	EndpointSafekeeperURL(id string) string
}
