// Package deploy places compute endpoints on a runtime behind a runtime-agnostic
// Deployer interface, so the controller's create-endpoint workflow is identical
// whether endpoints run as Docker containers (today) or Kubernetes pods (later).
// This is the "provisioning interface" of amp-controller.md §principle 3:
// runtime-specific logic lives here, never in core or the API handlers.
package deploy

import (
	"context"
	"fmt"
	"strings"

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
	// ConnectivityNLBInternal (VPC-routable), ConnectivityNLBExternal
	// (internet-facing), or the ingress pair, which gives the same two exposures
	// from a SHARED load balancer. The NLB modes add a second, Postgres-only
	// Service in front of a load balancer of their own; the
	// pageserver/safekeeper/supervisor ports are never published on it. Only the
	// Kubernetes deployer implements this.
	Connectivity string
	// AllowedCIDRs restricts which sources may reach that load balancer
	// (loadBalancerSourceRanges). Empty leaves it unrestricted, which for
	// ConnectivityNLBExternal means Postgres is reachable from the internet
	// behind password auth alone.
	AllowedCIDRs []string
	// Ingress carries the shard slot this endpoint holds, for the ingress_*
	// modes only. Allocated before the deploy rather than discovered after it,
	// which is what lets the address be known up front instead of waited for.
	Ingress IngressSlot
	// DNSName is an nlb_* endpoint's published name, as stored on its record —
	// see DedicatedLBDNSName. Empty, or unqualified, publishes nothing and leaves
	// the load balancer's own address as the client address.
	DNSName string
	// PasswordVerifier is the password for the compute's default user, as `pg_authid`
	// stores it (see core.SCRAMPasswordVerifier). Set on the create path only: the
	// plaintext is never persisted and the verifier is never read back, so a
	// REDEPLOY renders it empty and the supervisor leaves the password alone.
	PasswordVerifier string
	// Extensions are the externalized PostgreSQL extensions to mount into the
	// endpoint as read-only ImageVolumes (compute/architecture/extensions.md).
	// The Kubernetes deployer renders one volume per entry and derives
	// extension_control_path / dynamic_library_path (and shared_preload_libraries
	// for Preload entries) from them; the Docker deployer does not support
	// ImageVolumes and ignores this.
	Extensions []Extension
	// Labels are the endpoint's effective tags (its cluster's merged with its
	// own), rendered as labels on every object of its release. Only the
	// Kubernetes deployer implements this.
	Labels map[string]string
	// Identity is IdentityLabels of the endpoint, rendered alongside Labels.
	Identity map[string]string
	// IdentityAnnotations is IdentityAnnotations of the endpoint, rendered on its
	// pod. Only the Kubernetes deployer implements this.
	IdentityAnnotations map[string]string
}

// IngressSlot is an endpoint's place on a shared ingress shard: which shard,
// which port on it, and the name published for the endpoint. Zero for every mode
// but ingress_*.
type IngressSlot struct {
	Shard string
	Port  int
	// DNSName is the endpoint's own published name. With the port it is the
	// whole client-facing address, and unlike a load balancer's IP it survives
	// the shard being replaced.
	DNSName string
	// ShardHost and RecordType are what the endpoint's DNS record points at:
	// a CNAME to the shard's load-balancer hostname, or an A to its address.
	ShardHost  string
	RecordType string
}

// EntryPointName is the Traefik entryPoint a shard declares for one port, and
// the name an endpoint's route binds to.
//
// It is NOT the single source for both. The shard chart writes the name itself,
// in Go template (`--entryPoints.pg-<port>`), and never calls this. So the two
// spellings are two things kept equal by a test that reads the chart and
// compares — TestEntryPointNameMatchesTheShardChart — rather than one thing by
// construction. Worth the test because the failure is silent: Traefik accepts a
// route naming an entryPoint that does not exist and then never serves it, which
// looks like a routing bug rather than a typo.
func EntryPointName(port int) string { return fmt.Sprintf("pg-%d", port) }

// DocumentDBEntryPoint is the one entryPoint every shard declares for the
// documentdb gateway, shared by all its endpoints and routed by SNI. Spelled
// twice like EntryPointName, and held equal to the shard chart by the same test.
const DocumentDBEntryPoint = "documentdb"

// PublishableDNSName reports whether an endpoint's allocated name can be handed
// to a client, which is decided by the NAME ITSELF rather than by current config.
//
// A name minted where its family had no zone is unqualified (`ep-abc12345`); one
// minted with a zone carries it (`ep-abc12345.int.example`). That distinction is
// durable, and it has to be, because the decision is remade on every redeploy:
// a spec assembled from the allocation row has only the stored name to go on, so
// anything derived from today's zones would differ from what was actually
// published. Two failures that rules out — both silent:
//
//   - A redeploy of a zone-less endpoint would take the bare name as its address
//     and SetEndpointAddress would overwrite a working <shardHost>:<port> with
//     `ep-abc12345:20007`, which resolves nowhere.
//   - Adding a zone to an existing fleet would make every pre-existing row look
//     publishable while its stored name is still unqualified, so external-dns
//     would be handed records outside the zone it is pinned to.
//
// Giving an existing fleet published names therefore means reissuing them, which
// is a migration rather than a config change. Said here because the alternative
// is discovering it the first time somebody sets a zone.
func PublishableDNSName(name string) bool { return strings.Contains(name, ".") }

// DedicatedLBDNSName is the name published for an nlb_* endpoint, or "" where
// its family has no zone. Minted once at create and stored on the endpoint's
// record, so a zone set later does not rename an endpoint clients already hold.
//
// The full release name rather than the ingress path's short `ep-xxxxxxxx`: the
// short form needs the allocation table's unique index to stay unique, and this
// one is unique by construction. Being the release name also tells an operator
// which Service a record belongs to, and it can never equal an ingress name,
// which shares the zone.
func DedicatedLBDNSName(endpointID uuid.UUID, zone string) string {
	if zone == "" {
		return ""
	}
	return EndpointName(endpointID) + "." + zone
}

// Client reachability modes for EndpointSpec.Connectivity. These strings are
// also the chart's `service.connectivity` values and the API's enum, so one
// vocabulary runs end to end and nothing has to translate between layers.
//
// That end-to-end claim is not yet true of the ingress pair below: the API enum
// and the endpoint chart still know only the first three, and each catches up
// later in this series. Until then an ingress_* value cannot reach
// CreateEndpoint — the API rejects it as invalid_request — which is the only
// reason it is safe for the chart to render nothing for it.
const (
	ConnectivityClusterIP   = "cluster_ip"
	ConnectivityNLBInternal = "nlb_internal"
	ConnectivityNLBExternal = "nlb_external"

	// The ingress modes put the endpoint on a SHARED load balancer — a port on a
	// shard — rather than provisioning one of its own. Same exposure as the
	// nlb_* pair either side of the cluster boundary; the difference is who owns
	// the load balancer, which is why they are separate modes rather than an
	// implementation detail of the existing two.
	//
	// An endpoint's address becomes its own DNS name, which hides WHICH shard it
	// is on — but only the host half. The port comes from the target shard's own
	// range, so moving an endpoint between shards still changes its address, and
	// that is why nothing moves them on this path
	// (controller/architecture/ac-ingress.md).
	ConnectivityIngressInternal = "ingress_internal"
	ConnectivityIngressExternal = "ingress_external"
)

// IsIngress reports whether mode puts the endpoint on a shared ingress shard.
func IsIngress(mode string) bool {
	return mode == ConnectivityIngressInternal || mode == ConnectivityIngressExternal
}

// IsDedicatedLB reports whether mode provisions the endpoint its own cloud load
// balancer.
func IsDedicatedLB(mode string) bool {
	return mode == ConnectivityNLBInternal || mode == ConnectivityNLBExternal
}

// Extension is one externalized extension to mount into an endpoint: its name
// (the /extensions/<Name> mount subdir), the OCI image — built by
// deploy/extensions, or a third-party one consumed as-is — and whether it needs
// a shared_preload_libraries entry.
//
// The remaining fields describe a third-party image whose tree is not laid out
// the way deploy/extensions lays one out. Every path is relative to the mount
// root; the zero value means the amp layout, so an amp-built extension leaves
// them all empty.
type Extension struct {
	Name    string
	Image   string
	Preload bool
	// Library is the shared-library basename when it differs from Name — the
	// shared_preload_libraries entry the chart adds for a Preload extension.
	// Empty means Name.
	Library string
	// SharePath is the sharedir BASE for extension_control_path (PG18 appends
	// "/extension" itself); LibPath is the dynamic_library_path entry.
	SharePath string
	LibPath   string
	// LibraryPath are dirs of system shared libraries the extension's .so links
	// and the endpoint image does not carry; the supervisor appends them to
	// LD_LIBRARY_PATH.
	LibraryPath []string
	// EnvPaths are runtime data dirs by environment variable name (PROJ_DATA).
	EnvPaths map[string]string
	// ABIExtraExempt lists modules allowed to differ from the server in
	// FMGR_ABI_EXTRA — an image built against stock PostgreSQL.
	ABIExtraExempt []string
	// Settings are server settings the extension needs the compute started
	// with (documentdb's cron.database_name, say); the chart merges them in.
	Settings map[string]string
}

// Identity label keys. They live in the controller's own domain, which the API
// refuses as a tag key, so a caller's tag can never shadow or spoof one.
const (
	LabelEndpointID    = "amp.yugabyte.com/endpoint-id"
	LabelBranchID      = "amp.yugabyte.com/branch-id"
	LabelClusterID     = "amp.yugabyte.com/cluster-id"
	LabelStoragePoolID = "amp.yugabyte.com/storage-pool-id"
	LabelEndpointType  = "amp.yugabyte.com/ep-type"

	// The names are annotations rather than labels because a name is free text,
	// which a label value need not accept.
	AnnotationStoragePoolName = "amp.yugabyte.com/storage-pool-name"
	AnnotationClusterName     = "amp.yugabyte.com/cluster-name"
	AnnotationBranchName      = "amp.yugabyte.com/branch-name"
	AnnotationEndpointName    = "amp.yugabyte.com/endpoint-name"
)

// IdentityLabels are the labels that say which endpoint an object belongs to,
// so its objects can be selected by endpoint, branch, cluster, pool or role
// without parsing the release name. A zero id is left out rather than rendered
// as the nil UUID, which would select every such object together.
func IdentityLabels(e core.EndpointDeploy) map[string]string {
	out := map[string]string{}
	for k, id := range map[string]uuid.UUID{
		LabelEndpointID:    e.ID,
		LabelBranchID:      e.BranchID,
		LabelClusterID:     e.ClusterID,
		LabelStoragePoolID: e.StoragePoolID,
	} {
		if id != uuid.Nil {
			out[k] = id.String()
		}
	}
	if e.Role != "" {
		out[LabelEndpointType] = e.Role
	}
	return out
}

// IdentityAnnotations are the identity values that cannot be label values. An
// empty name is left out.
func IdentityAnnotations(e core.EndpointDeploy) map[string]string {
	var out map[string]string
	for k, name := range map[string]string{
		AnnotationStoragePoolName: e.StoragePoolName,
		AnnotationClusterName:     e.ClusterName,
		AnnotationBranchName:      e.BranchName,
		AnnotationEndpointName:    e.Name,
	} {
		if name == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = name
	}
	return out
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
		Ingress: IngressSlot{
			Shard:      e.IngressShard,
			Port:       e.IngressPort,
			DNSName:    e.IngressDNSName,
			ShardHost:  e.IngressShardHost,
			RecordType: e.IngressRecordType,
		},
		DNSName:             e.DNSName,
		Extensions:          mounts,
		Labels:              e.Labels,
		Identity:            IdentityLabels(e),
		IdentityAnnotations: IdentityAnnotations(e),
		// No PasswordVerifier: there is nowhere to read one from. The password is
		// already set in the branch's catalog, and re-rendering an empty verifier
		// is what stops a redeploy resetting it.
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
// DocumentDBGatewayRouter reports whether a client can reach an endpoint's documentdb
// gateway at that endpoint's OWN host on the gateway port. It is not always
// true: whatever fronts the endpoint has to publish the gateway port on that same
// host, and a NodePort Service, for one, allocates it a high port of its own. A
// deployer that does not implement this is assumed to route (Docker and local
// both run the gateway on the same host as the compute).
type DocumentDBGatewayRouter interface {
	DocumentDBGatewayReachable(e core.Endpoint) bool
}

type ConnectivitySupporter interface {
	SupportsConnectivity() bool
}

// SourceRangesUpdater is implemented by a deployer that can replace an nlb_*
// endpoint's allowlist on its live load-balancer Service. It patches that one
// field and nothing else, for the reason ResizeEndpoint does not patch the pod
// template: re-rendering the release would also apply whatever else the stored
// record says — a resized endpoint's new size, for one — and roll the pod.
// Durability across a later redeploy is the caller's job (it persists the list).
type SourceRangesUpdater interface {
	UpdateSourceRanges(ctx context.Context, name string, cidrs []string) error
}

// IngressSupporter is implemented by a deployer that can place an endpoint on a
// shared ingress shard. Separate from ConnectivitySupporter because the two ask
// different questions: that one is "can this driver render an exposure at all",
// this one is "does THIS deployment route endpoints through shards".
//
// Deliberately one deployment-wide answer and not a per-family one. Whether a
// given family can be served is the allocator's question, not the deployer's —
// it depends on the shard rows in ingress_shards and on that family's zone, and
// ingress.Allocate already answers it per family. A second, coarser copy of that
// judgement here could only disagree with the one actually enforced.
//
// It answers only whether the deployment opted in. Whether the deploy path can
// actually place an endpoint on a shard is IngressPlacer, below — the two are
// separate because the switch lands before the placement does.
type IngressSupporter interface {
	SupportsIngress() bool
}

// IngressPlacer is implemented by a deployer whose CreateEndpoint knows what to
// do with an EndpointSpec.Ingress — render the endpoint's route onto its shard
// and report the address it answers on.
//
// Separate from IngressSupporter because a deployment can have the switch on
// while the deploy path still has no idea what an ingress endpoint is, and that
// is exactly the window this series passes through: the flag arrives before the
// placement. Accepting the mode in that window would record an exposure the
// endpoint does not have — the create succeeds, the endpoint comes back
// reporting ingress_internal, and its address is the ordinary in-cluster one.
// That is the failure ConnectivitySupporter exists to prevent for the nlb_*
// modes, and it needs preventing here for the same reason.
//
// The Kubernetes deployer starts satisfying this when the lifecycle wiring
// lands, so the API opens to the ingress modes exactly when something can honour
// them rather than on a date.
type IngressPlacer interface {
	PlacesIngress() bool
}

// ComputePasswordApplier is implemented by a Deployer whose endpoints do NOT
// apply the compute password, so the API can stop demanding one. Opt-OUT, unlike
// ConnectivitySupporter: a deployer that does not implement it does apply it,
// which is what the Docker and Kubernetes runtimes do.
//
// LocalDeployer is the case this exists for. A yb_amp_local compute is reached
// over loopback with a caller-rendered spec, which makes the supervisor skip
// remote_auth entirely (see Prerender in endpoint_supervisor), so the password
// would never be set — and requiring one would be demanding a credential nothing
// consumes and nobody can use.
type ComputePasswordApplier interface {
	AppliesComputePassword() bool
}

// AppliesComputePassword reports whether endpoints placed by dep actually set
// the compute password the create request carries. True for a deployer that says
// nothing, since that is every runtime whose supervisor runs remote_auth.
func AppliesComputePassword(dep Deployer) bool {
	p, ok := dep.(ComputePasswordApplier)
	return !ok || p.AppliesComputePassword()
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
