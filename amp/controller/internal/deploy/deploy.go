// Package deploy places compute endpoints on a runtime behind a runtime-agnostic
// Deployer interface, so the controller's create-endpoint workflow is identical
// whether endpoints run as Docker containers (today) or Kubernetes pods (later).
// This is the "provisioning interface" of amp-controller.md §principle 3:
// runtime-specific logic lives here, never in core or the API handlers.
package deploy

import (
	"context"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// safekeeperHTTPPort is the port every endpoint's co-located safekeeper serves
// its HTTP API on. The same in both runtimes — the Docker container's internal
// port and the Helm chart's ports.safekeeperHttp — so the controller can address
// it uniformly from an endpoint's reachable host.
const safekeeperHTTPPort = 7676

// supervisorHTTPPort is the port an endpoint's supervisor serves its settings
// API on (ports.supervisorHttp in deploy/helm/yb-amp-endpoint). Served only
// where a controller exists to call it, which is exactly the case the
// controller is in when it calls.
const supervisorHTTPPort = 9899

// branchIDValue renders a branch id for a deployer's wire format. uuid.Nil must
// render as "" — the chart documents an empty branchId as "bootstrap a fresh
// branch" and the supervisor's env_opt discards empty values, so emitting
// "00000000-0000-0000-0000-000000000000" would pass every one of those checks and
// deploy a pod claiming to serve a branch that cannot exist.
func branchIDValue(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

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
	// EndpointID is this endpoint's `amp_registry.endpoints.id`, handed to the
	// pod so the supervisor can read the endpoint's own override columns
	// (pg_settings / pageserver_settings / safekeeper_settings) straight from the
	// registry. It is recoverable from Name (which is "yb-ep-<id>"), but is
	// passed explicitly so the pod never has to parse its release name. Empty =>
	// the pod runs on derived defaults with no override lookup.
	EndpointID string
	// BranchID is the storage-layer branch id this compute serves (the flat
	// single-id storage contract — no tenant). uuid.Nil means "unset", which both
	// deployers render as the empty string — see branchIDValue.
	BranchID  uuid.UUID
	PgVersion int
	YB        YBConn
	// ControllerURL, when non-empty, points the endpoint at that controller for
	// its branch bookkeeping. Empty => controller-free self-bootstrap. The
	// pageserver's generation is not involved: it claims its own.
	ControllerURL string
	// CPU and Memory are the compute reserved for the endpoint pod, as Kubernetes
	// resource quantities (e.g. "2", "4Gi"). Each is applied as BOTH the request
	// and the limit on the endpoint container, so the pod gets Guaranteed QoS.
	// Empty defaults to 2 cores / 4Gi at the deployer. That is not the chart
	// default and not what a new endpoint gets — it is the shape the pods written
	// before memory was stored are already running, kept so a redeploy does not
	// resize them (see defaultEndpointMemory in kubernetes.go).
	CPU    string
	Memory string
	// Version overrides the deployer's configured endpoint version for this
	// endpoint. It is an opaque string the deployer knows how to run — today the
	// endpoint image tag, which each deployer maps onto its own image reference in
	// one place. Empty = the deployer default (KubernetesConfig.EndpointImageTag,
	// the tag of DockerConfig.EndpointImage).
	Version string
	// Connectivity is how clients reach this endpoint's Postgres port:
	// ConnectivityClusterIP (the default, in-cluster and tailnet only),
	// ConnectivityNLBInternal (VPC-routable) or ConnectivityNLBExternal
	// (internet-facing). The NLB modes add a SECOND, Postgres-only Service in
	// front of a load balancer; the pageserver/safekeeper/supervisor ports are
	// never published on it. Only the Kubernetes deployer implements this.
	Connectivity string
	// AllowedCIDRs restricts which sources may reach that load balancer
	// (loadBalancerSourceRanges). Empty leaves it unrestricted, which for
	// ConnectivityNLBExternal means Postgres is reachable from the internet
	// behind password auth alone.
	AllowedCIDRs []string
	// Extensions are the externalized PostgreSQL extensions to mount into the
	// endpoint as read-only ImageVolumes (compute/architecture/extensions.md).
	// The Kubernetes deployer renders one volume per entry and derives
	// extension_control_path / dynamic_library_path (and shared_preload_libraries
	// for Preload entries) from them; the Docker deployer does not support
	// ImageVolumes and ignores this.
	Extensions []Extension
}

// Client reachability modes for EndpointSpec.Connectivity. These strings are
// also the chart's `service.connectivity` values and the API's enum, so one
// vocabulary runs end to end and nothing has to translate between layers.
const (
	ConnectivityClusterIP   = "cluster_ip"
	ConnectivityNLBInternal = "nlb_internal"
	ConnectivityNLBExternal = "nlb_external"
)

// Extension is one externalized extension to mount into an endpoint: its name
// (the /extensions/<Name> mount subdir), the OCI image built by
// deploy/extensions, and whether it needs a shared_preload_libraries entry.
type Extension struct {
	Name    string
	Image   string
	Preload bool
}

// EndpointName is the deployer's stable instance handle for an endpoint (the
// container/pod/Helm-release name), keyed by the endpoint id. "yb-ep-" + a
// lowercase-hyphen uuid is a valid DNS-1123 name (42 chars, under Helm's 53-char
// limit).
func EndpointName(endpointID uuid.UUID) string {
	return "yb-ep-" + endpointID.String()
}

// EndpointSpecFrom renders an endpoint's deploy spec: every field comes from the
// endpoint's own desired state, or from one of the three inputs a row cannot
// carry — the YB connection, the controller URL, and the resolved mounts.
func EndpointSpecFrom(e core.EndpointDeploy, yb YBConn, controllerURL string, mounts []Extension) EndpointSpec {
	return EndpointSpec{
		Name:          EndpointName(e.ID),
		EndpointID:    e.ID.String(),
		BranchID:      e.BranchID,
		PgVersion:     e.PgVersion,
		YB:            yb,
		ControllerURL: controllerURL,
		CPU:           e.CPU,
		Memory:        e.Memory,
		Version:       e.Version,
		Connectivity:  e.Connectivity,
		AllowedCIDRs:  e.AllowedCIDRs,
		Extensions:    mounts,
		// No pg_settings here: the endpoint's supervisor reads the branch's and
		// the endpoint's straight from amp_registry and merges them itself, so a
		// change is a registry write rather than a redeploy. EndpointID above is
		// what it needs to find them.
	}
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
	// Version is the version the deployer actually ran: the spec's Version when it
	// asked for one, otherwise the deployer's own configured default. The caller
	// persists this rather than re-deriving it, so the row records a fact instead
	// of a request the deployer may have defaulted. Empty means the deployer has
	// nothing to report (it placed nothing, or its image reference names no
	// version), and the caller leaves the recorded version as it found it.
	Version string
}

// ConnectivitySupporter is implemented by a Deployer that can honour a
// per-endpoint nlb_* connectivity mode. A deployer that does not implement it
// silently ignores EndpointSpec.Connectivity, so the API rejects the nlb_* modes
// up front rather than persisting an exposure the endpoint will never have.
type ConnectivitySupporter interface {
	SupportsConnectivity() bool
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
	// EndpointSupervisorURL returns the base URL of the endpoint's supervisor
	// settings API, addressed exactly as EndpointSafekeeperURL addresses the
	// safekeeper. The controller POSTs to it after writing a settings row, so a
	// running endpoint re-reads it rather than waiting for its next restart.
	//
	// Empty when the deployer has no way to reach one, which the caller must
	// treat as "cannot be told" rather than an error: the row is written either
	// way, and an endpoint that was not told picks the change up when it starts.
	EndpointSupervisorURL(id string) string
}
