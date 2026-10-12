package deploy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/helm"
)

// defaultPgVersion backs an endpoint whose spec omits a version (the chart's
// own default is the same). yb-amp is Postgres v18 only.
const defaultPgVersion = 18

// defaultEndpointCPU / defaultEndpointMemory back an endpoint whose spec omits a
// resource reservation — applied as request == limit for Guaranteed QoS.
//
// This is not the shape a new endpoint gets. The control plane resolves both
// dimensions before it plans the deploy, holding them to a memory-per-core floor
// (ResolveEndpointShape), so a spec reaching here without memory is one
// written before it did. 4Gi is what those pods have been running, and handing
// them the current default instead would resize every one of them on its next
// redeploy — a change nobody asked for, arriving whenever something happens to
// redeploy it.
const (
	defaultEndpointCPU    = "2"
	defaultEndpointMemory = "4Gi"
)

// endpointContainerName is the compute container in the endpoint pod (the chart's
// `- name: endpoint`). It is the container the controller resizes; the resize
// PATCH bodies address it by this name (strategic-merge merge key).
const endpointContainerName = "endpoint"

// resizeSettleTimeout bounds how long ResizeEndpoint re-reads the pod waiting for
// the kubelet to clear the transient PodResizeInProgress condition, so the
// reported outcome is Completed rather than Pending in the common (node has
// capacity) case. Short and best-effort — the durable size is already persisted.
const resizeSettleTimeout = 10 * time.Second

// resizeSettlePollInterval paces the settle-loop re-reads so it doesn't hammer
// the apiserver with back-to-back GETs while the kubelet applies the change
// (mirrors the paced polling in serviceLBAddress). Short, since the whole settle
// budget is only resizeSettleTimeout and reporting Completed promptly matters.
const resizeSettlePollInterval = 200 * time.Millisecond

// resizeInProgressMsg marks a transient, still-settling resize (the kubelet is
// applying it, or has not observed it yet). classifyResize returns it and the
// settle loop re-polls on exactly this state — so it is a shared sentinel, not
// just display text.
const resizeInProgressMsg = "in progress"

// serviceTypeLoadBalancer is the endpoint Service type that gets an external IP;
// the deployer then reports that IP as the endpoint's client address.
const serviceTypeLoadBalancer = "LoadBalancer"

// serviceTypeNodePort is the endpoint Service type reachable at every node's IP
// on an allocated high port; the deployer reports "<nodeIP>:<nodePort>" as the
// client address (no cloud LB to provision).
const serviceTypeNodePort = "NodePort"

// lbAddressPollTimeout bounds how long CreateEndpoint waits for a LoadBalancer
// Service to receive its ingress IP before falling back to the Service DNS. A
// short, fixed cap (independent of the longer helm --wait deploy timeout) so a
// slow cloud LB doesn't keep the synchronous create blocked indefinitely.
const lbAddressPollTimeout = 2 * time.Minute

// lbAddressPollInterval is how often serviceLBAddress re-reads the Service while
// waiting for that ingress address. Kept short: the address usually lands within
// a second or two of the first miss, and the create is synchronous, so a coarse
// interval just adds dead wait to every LB-backed endpoint. The budget above,
// not this interval, is what bounds a genuinely slow cloud LB.
const lbAddressPollInterval = 500 * time.Millisecond

// KubernetesConfig is the static configuration of the Helm-backed deployer: the
// cluster/chart/image coordinates that are the same for every endpoint. The
// per-endpoint identity (branch id/pg version, the backing YB connection,
// the controller URL, bootstrap) arrives in the EndpointSpec.
type KubernetesConfig struct {
	Namespace      string // namespace endpoints are deployed into
	ChartPath      string // path to the yb-amp-endpoint Helm chart
	HelmBin        string // helm binary; bare name resolved via PATH, a path used verbatim
	KubeconfigPath string // KUBECONFIG for helm; empty = default discovery (in-cluster or ~/.kube)

	EndpointImageRepo string // endpoint container image repository
	EndpointImageTag  string // endpoint container image tag
	// The documentdb gateway sidecar's image. An empty tag means "use
	// EndpointImageTag": CI builds both from one commit, so they share it.
	DocumentDBGatewayImageRepo string
	DocumentDBGatewayImageTag  string
	PullSecret                 string // imagePullSecrets name in the namespace (empty = none)
	ComputePort                int    // compute Postgres port the endpoint Service publishes
	ServiceType                string // endpoint Service type: "ClusterIP" (default), "NodePort", or "LoadBalancer"
	// ServiceAnnotations are set on the endpoint Service (e.g. GKE internal-LB annotations).
	ServiceAnnotations map[string]string
	// IngressEnabled is whether this deployment routes endpoints through shared
	// ingress shards at all, and it is the whole of what the deployer knows about
	// ingress. The DNS zones deliberately do not appear here: the deployer never
	// mints a name — the allocator does, and hands the finished address down — so
	// a copy of the zones on this struct could only go stale.
	IngressEnabled bool
	// Cloud overrides the cloud detected from node provider IDs ("aws", "gcp",
	// "azure"), which selects the load-balancer annotations the endpoint chart
	// renders for the nlb_* connectivity modes. Empty (the normal case) = detect.
	Cloud string
	// NodeSelector, when non-empty, pins endpoint pods to nodes carrying these
	// labels — e.g. a node pool with local-NVMe-SSD ephemeral storage, so the
	// endpoint's emptyDir /data lands on NVMe instead of the node boot disk.
	NodeSelector map[string]string

	// TailnetSuffix, when non-empty AND the endpoint Service is exposed on the
	// tailnet (ServiceAnnotations has "tailscale.com/expose"="true"), is the
	// Tailscale MagicDNS suffix (e.g. "tail5955.ts.net") used to derive the
	// endpoint's tailnet address "<hostname>.<suffix>:<computePort>", where
	// <hostname> is the operator's device name — the explicit
	// "tailscale.com/hostname" annotation if set, else "<namespace>-<release>".
	// Empty disables tailnet address reporting.
	TailnetSuffix string

	// StorageClass, when non-empty, provisions the endpoint's data volume as a
	// PersistentVolumeClaim of that class (a network block volume — EBS/PD) of
	// size DiskSize, instead of an ephemeral emptyDir. Empty = emptyDir.
	StorageClass string
	DiskSize     string
	// DiskGiBPerVCPU is the data-volume size per endpoint vCPU. 0 = the built-in
	// 20 GiB/vCPU policy, which every pool is expected to be provisioned to
	// honour; set it only for a pool built to a deliberately different
	// disk-per-core shape. The controller cannot check the figure against the
	// nodes it will schedule onto (#584), so a ratio the pool cannot back
	// surfaces as an unschedulable endpoint rather than an error here.
	DiskGiBPerVCPU int
	// DiskSizeOverride, when non-empty, is the flat data-volume size every
	// endpoint gets regardless of its CPU — the per-vCPU ratio and the DiskSize
	// floor are both bypassed. For test clusters whose nodes cannot back the
	// ratio (#602); empty (the default) keeps the CPU-proportional policy.
	DiskSizeOverride string
	// LFCOffloadIntervalSeconds turns on LFC offload at this interval and
	// autoprewarm with it; 0 leaves both at the chart's default (off).
	LFCOffloadIntervalSeconds int

	// Optional: deliver the backing YugabyteDB password from a pre-existing
	// Secret in the namespace instead of inline (keeps the plaintext off the
	// controller). When ExistingSecret is empty, the spec's inline YB.Password
	// is used.
	BackingYBExistingSecret    string
	BackingYBExistingSecretKey string

	// TLS to the backing YugabyteDB. Empty or "disable" = off. Otherwise every
	// release carries a cert-manager Certificate (CN = the YB user) from the
	// issuer below, mounted with the CA bundle ConfigMap; the chart fixes the
	// paths, so the connection block only needs the mode.
	BackingYBTLSMode              string
	BackingYBTLSIssuerKind        string
	BackingYBTLSIssuerName        string
	BackingYBTLSIssuerGroup       string
	BackingYBTLSCABundleConfigMap string
	BackingYBTLSCertDuration      string
	BackingYBTLSCertRenewBefore   string

	// Server TLS on the compute's Postgres listener. "issue" gives each endpoint
	// its own Certificate from the issuer below; "existing-secret" mounts
	// ComputeTLSSecret instead. Empty or "disable" = off.
	ComputeTLSMode              string
	ComputeTLSIssuerKind        string
	ComputeTLSIssuerName        string
	ComputeTLSIssuerGroup       string
	ComputeTLSSecret            string
	ComputeTLSCertDuration      string
	ComputeTLSCertRenewBefore   string
	ComputeTLSIncludeServiceDNS bool
	// EndpointUserSplit sets the chart's userSplit.enabled.
	EndpointUserSplit bool

	Timeout time.Duration // how long to wait for a deploy/retire to converge (helm --wait)
}

// Where the endpoint chart mounts the TLS material; the connection block
// names these paths and the chart's deployment template mounts them.
const (
	endpointTLSCAPath   = "/etc/amp/tls/ca/ca.crt"
	endpointTLSCertPath = "/etc/amp/tls/client/tls.crt"
	endpointTLSKeyPath  = "/etc/amp/tls/client/tls.key"
)

// KubernetesDeployer is the Helm-backed Deployer: one Helm release (one pod) per
// endpoint (amp-controller.md §5.4, "one pod per endpoint"). It is the
// Kubernetes sibling of [DockerDeployer], implementing the same [Deployer]
// interface.
type KubernetesDeployer struct {
	helm   *helm.Client
	cfg    KubernetesConfig
	logger *slog.Logger
	// cloudMu/cloud memoize which cloud this cluster runs on, read from a node's
	// spec.providerID. A cluster does not migrate between clouds under us, so a
	// successful answer is looked up once rather than on every endpoint create.
	// Only success is cached: k8sAPIGet is best-effort and returns nothing on a
	// transient apiserver error, and caching that would refuse every later
	// nlb_internal create until the controller restarts.
	cloudMu sync.Mutex
	cloud   string
}

// NewKubernetesDeployer builds the deployer from its static config.
func NewKubernetesDeployer(cfg KubernetesConfig, logger *slog.Logger) *KubernetesDeployer {
	return &KubernetesDeployer{
		helm:   helm.New(cfg.HelmBin, cfg.KubeconfigPath, logger),
		cfg:    cfg,
		logger: logger,
	}
}

var (
	_ Deployer = (*KubernetesDeployer)(nil)
	// The API gates the ingress_* modes on this, so a deployer that stops being
	// able to place one has to be a compile error rather than a silent reopening
	// of the window where the mode is accepted and nothing honours it.
	_ IngressPlacer = (*KubernetesDeployer)(nil)
	// The API accepts an allowlist change on any deployer that renders the load
	// balancer, so the one that does must also be able to change it.
	_ SourceRangesUpdater = (*KubernetesDeployer)(nil)
)

// SupportsConnectivity reports that this deployer renders the per-endpoint
// load-balancer Service, satisfying deploy.ConnectivitySupporter. The docker and
// local deployers do not implement it.
func (k *KubernetesDeployer) SupportsConnectivity() bool { return true }

// SupportsIngress reports whether this deployment routes endpoints through
// shared ingress shards, satisfying deploy.IngressSupporter.
//
// It reports the switch and nothing more. It is NOT a promise that a slot can be
// had: capacity is a question about rows in ingress_shards, which only the
// allocator can answer, and it refuses per family. --deploy-ingress-enabled sets
// it, and endpointConnectivity reads it to reject the ingress_* modes up front,
// so a caller gets a 400 rather than an endpoint that deploys unreachable.
func (k *KubernetesDeployer) SupportsIngress() bool { return k.cfg.IngressEnabled }

// PlacesIngress reports that CreateEndpoint can actually put an endpoint on a
// shard, satisfying deploy.IngressPlacer. It is what opens the API to the
// ingress_* modes, and it belongs to THIS commit rather than an earlier one
// because this is where the route starts being rendered: values() carries the
// slot (ingressValuesFor) and the endpoint chart has an IngressRouteTCP to put
// it on. Before this, accepting the mode would have recorded an exposure the
// endpoint did not have — a create that succeeds, a GET reporting
// ingress_internal, and an address nothing routes to.
//
// Unconditional rather than read from config, because it is a property of this
// code — the rendering below exists — not of how a deployment is configured.
// Whether the deployment opted in is SupportsIngress; whether a slot is free is
// the allocator's answer.
func (k *KubernetesDeployer) PlacesIngress() bool { return true }

// DocumentDBGatewayReachable reports whether a client reaches this endpoint's
// documentdb gateway at the endpoint's own host on the gateway port, which holds
// only where whatever fronts the endpoint publishes that port on the same host:
//
//   - cluster_ip: the default ClusterIP Service, which carries a `documentdb`
//     port whenever the gateway renders. A NodePort Service does not qualify: it
//     allocates each port its own nodePort, so the address carries the COMPUTE
//     port's and the gateway's is a different one.
//   - nlb_*: the -lb Service publishes the gateway beside Postgres. Its address
//     falls back to the Service-type one while the load balancer provisions,
//     which still reaches the gateway unless it is a nodePort — the one fallback
//     not on the compute port.
//   - ingress_*: the shard routes the gateway by the endpoint's published name
//     over SNI, so an endpoint whose family had no zone, addressed by its shard's
//     shared host, has no gateway route.
//
// Where it fails, the address on the gateway port names something nothing listens
// on, so the API reports no documentdb_address rather than a dead one. A
// connectivity mode this does not recognise is treated as unreachable for the
// same reason.
func (k *KubernetesDeployer) DocumentDBGatewayReachable(e core.Endpoint) bool {
	switch {
	case IsIngress(e.Connectivity):
		return PublishableDNSName(e.IngressDNSName)
	case IsDedicatedLB(e.Connectivity):
		_, port, err := net.SplitHostPort(e.Address)
		return err == nil && port == strconv.Itoa(k.cfg.ComputePort)
	case e.Connectivity == "" || e.Connectivity == ConnectivityClusterIP:
		return k.cfg.ServiceType == "" || k.cfg.ServiceType == "ClusterIP"
	default:
		return false
	}
}

// CreateEndpoint installs/updates the endpoint's Helm release and waits for it
// to be ready, then returns the stable in-cluster client address. The release
// name (= spec.Name, a DNS-1123 label) is also the object/Service name via
// fullnameOverride, so the address is a pure function of the name and config.
func (k *KubernetesDeployer) CreateEndpoint(ctx context.Context, spec EndpointSpec) (Endpoint, error) {
	release := spec.Name
	// Checked before anything is installed. A zero slot means the spec was
	// rendered from a record that lost it — a redeploy projecting no allocation,
	// say — and deploying anyway would report ":0" as the endpoint's address,
	// which SetEndpointAddress then writes over the address clients are using.
	// That unpublishes a live endpoint with no error anywhere. Refusing up front
	// leaves it exactly as it was.
	if IsIngress(spec.Connectivity) &&
		(spec.Ingress.Port == 0 || (spec.Ingress.DNSName == "" && spec.Ingress.ShardHost == "")) {
		return Endpoint{}, fmt.Errorf(
			"endpoint %s is %s but its spec carries no ingress slot (shard %q, port %d); "+
				"deploying would publish an address nothing answers on",
			release, spec.Connectivity, spec.Ingress.Shard, spec.Ingress.Port)
	}
	// The data volume's size is proportional to the endpoint's CPU.
	vals := k.values(spec, release)
	// Only needed for the load-balanced modes, and it costs a node list — so it is
	// resolved here rather than in values(), which has no context to do it with.
	if IsDedicatedLB(spec.Connectivity) {
		vals.Service.Cloud = k.detectCloud(ctx)
	}
	// On a redeploy the record is already published. Rendering without its
	// target would have helm delete it, and external-dns withdraw the name, until
	// the second pass below put it back — so the target is read first.
	if d := vals.Service.DNS; d != nil {
		ip, hostname, err := k.loadBalancerIngress(ctx, release+"-lb")
		if err := preRenderDNSTarget(d, ip, hostname, err); err != nil {
			return Endpoint{}, fmt.Errorf("read load balancer of %s before redeploying its DNS record: %w", release, err)
		}
	}
	if err := k.applyDiskSizing(&vals); err != nil {
		return Endpoint{}, err
	}
	// The deploy namespace is the controller's own namespace (the chart sets
	// AMP_DEPLOY_NAMESPACE to .Release.Namespace), so it always exists — no
	// --create-namespace, which would otherwise demand cluster-scoped
	// namespaces:create the controller need not hold on a shared cluster.
	//
	// The watcher runs for the length of the wait: helm removes the pod when it
	// gives up, and the pod is the only thing that knows why the deploy failed.
	watch := k.watchDeploy(ctx, release)
	err := k.helm.UpgradeInstall(ctx, helm.Release{
		Name:      release,
		Chart:     k.cfg.ChartPath,
		Namespace: k.cfg.Namespace,
		Values:    vals,
		Wait:      true,
		Timeout:   k.cfg.Timeout,
	})
	watch.close()
	if err != nil {
		// Only a wait expiry is the workload's failure to explain. Every other way
		// helm fails — a chart render refusal, an unwritable values file, a
		// cancelled context — already carries its cause, and on most of them no pod
		// exists at all, so a pod diagnosis in front would be noise or a lie.
		if !errors.Is(err, helm.ErrWaitTimeout) {
			return Endpoint{}, fmt.Errorf("helm deploy %s: %w", release, err)
		}
		return Endpoint{}, k.explainDeployFailure(ctx, release, watch, err)
	}
	// A per-endpoint load balancer is the DURABLE client address: the AWS load
	// balancer controller re-registers the pod when it is replaced, so unlike the
	// Service DNS the other branches fall back to, it is reachable from outside
	// the cluster. It also survives a controller-driven redeploy, but only because
	// core.EndpointDeploy carries Connectivity/AllowedCIDRs — without them the
	// redeploy renders service.connectivity empty, helm prunes the -lb Service and
	// the load balancer is deleted. Checked before the Service-type switch because
	// it is a per-endpoint choice, not the controller-wide default.
	// A shared-ingress endpoint's address was decided before this deploy started:
	// the allocator picked the shard and port, and the name is derived from the
	// endpoint's id. So there is nothing to poll for — no cloud resource is being
	// provisioned by this create at all, which is the point of the design and the
	// reason an ingress create does not pay the ~2 minutes serviceLBAddress can
	// spend below.
	if IsIngress(spec.Connectivity) {
		if err := k.verifyReady(ctx, release); err != nil {
			return Endpoint{}, fmt.Errorf("verify endpoint %s: %w", release, err)
		}
		// An unpublishable name means there was no zone to mint it in, so the
		// honest address is the shard's own. The endpoint is equally reachable
		// either way; what the name buys is the freedom to move it later.
		//
		// Decided from the name rather than from its emptiness, because only the
		// create path ever blanked it — every redeploy assembles the spec from the
		// allocation row, which stores the bare name, and would otherwise publish
		// `ep-abc12345:20007` over a working address.
		host := spec.Ingress.DNSName
		if !PublishableDNSName(host) {
			host = spec.Ingress.ShardHost
		}
		addr := fmt.Sprintf("%s:%d", host, spec.Ingress.Port)
		k.logger.Info("endpoint ready", "endpoint", release, "address", addr,
			"connectivity", spec.Connectivity, "shard", spec.Ingress.Shard)
		return Endpoint{ID: release, Address: addr, TailnetAddress: k.tailnetAddress(release), Version: vals.Image.Tag}, nil
	}
	var addr, lb string
	dnsTarget := ""
	if d := vals.Service.DNS; d != nil {
		dnsTarget = d.Target
	}
	if IsDedicatedLB(spec.Connectivity) {
		if lb = k.serviceLBAddress(ctx, release+"-lb"); lb != "" {
			addr = publishLBName(vals.Service.DNS, lb)
		} else {
			// Provisioning a cloud load balancer routinely outruns
			// lbAddressPollTimeout. Fall through rather than fail the create: the
			// endpoint is up and serving on the Service-type address, and no DNS
			// record is published, since there is nothing yet for it to point at.
			//
			// Nothing re-polls on its own. The recorded address stays the fallback —
			// the Service DNS, which serves in-cluster clients but not the ones this
			// mode exists for — until the endpoint's next controller-driven redeploy
			// re-runs this resolution (#1380). See "Endpoint stuck on the in-cluster
			// address after an nlb_* create" in amp/runbooks/controller_runbook.md.
			k.logger.Warn("endpoint load balancer not ready in time; falling back to the Service-type address",
				"endpoint", release, "connectivity", spec.Connectivity)
		}
	}
	if addr == "" {
		// Only the *client* address. The controller reaches the endpoint's
		// pageserver/safekeeper by the stable Service DNS (CONTROLLER_REACHABLE_HOST
		// set in the chart), so a pod reschedule re-registers the same address (no
		// 409, #121).
		external := ""
		switch k.cfg.ServiceType {
		case serviceTypeLoadBalancer:
			external = k.serviceLBAddress(ctx, release)
		case serviceTypeNodePort:
			external = k.nodePortClientAddress(ctx, release)
		}
		addr = clientAddress(k.cfg.ServiceType, k.address(release), external)
	}
	// SECOND PASS. The release rendered above could not know this address: a
	// LoadBalancer's is assigned by the cloud after the Service exists, so at
	// render time there was nothing to put in the certificate's SANs or in an
	// nlb_* endpoint's DNS record. Now there is.
	//
	// Re-rendered through helm rather than patched onto the objects, because
	// helm owns them — a direct patch survives exactly until the next redeploy,
	// which is the kind of fix that works in a demo and not in a fleet. The
	// values are the durable record.
	//
	// Costs a reissue and a reload, not a restart: only the Certificate's spec
	// moves, so cert-manager reissues, the kubelet propagates, and the
	// supervisor's watcher picks the new pair up and confirms it on the wire
	// (compute/architecture/single-container-endpoint.md).
	dnsChanged := vals.Service.DNS != nil && vals.Service.DNS.Target != dnsTarget
	if err := k.renderClientAddress(ctx, release, &vals, addr, dnsChanged); err != nil {
		// Not fatal. The endpoint is up and serving; what it lacks is a
		// certificate a client can verify by this address, or its published
		// name, which is worth a loud line and a retry on the next redeploy
		// rather than tearing down a working create.
		k.logger.Warn("endpoint certificate or DNS record does not cover its client address",
			"endpoint", release, "address", addr, "error", err)
		addr = addressAfterFailedSecondPass(addr, lb, dnsChanged)
	}
	// helm --wait covers the pod, not the Service in front of it: it has no
	// endpoint check, so a release is "ready" before the pod is a backend the
	// ClusterIP forwards to. The controller reaches this endpoint's
	// pageserver/safekeeper over exactly that Service, so the deploy is not done
	// until that Service is verified — otherwise the task reports success and the
	// controller then refuses its own follow-up call (#724).
	if err := k.verifyReady(ctx, release); err != nil {
		return Endpoint{}, fmt.Errorf("verify endpoint %s: %w", release, err)
	}
	tailnet := k.tailnetAddress(release)
	k.logger.Info("endpoint ready", "endpoint", release, "address", addr, "tailnet_address", tailnet)
	// The version reported is the tag that was rendered, read back off the values
	// that were installed — so it cannot drift from what the release runs.
	return Endpoint{ID: release, Address: addr, TailnetAddress: tailnet, Version: vals.Image.Tag}, nil
}

// verifyReadyBudget / verifyReadyInterval / verifyReadyAttempt bound the
// confirmation that the endpoint is reachable. Nothing here fails on one bad
// attempt: a Service gains its backend a beat after the pod goes Ready
// (EndpointSlice, then kube-proxy), so the check keeps trying for the whole
// budget and reports what it last saw once that runs out. Each attempt gets its
// own timeout so a single hung dial cannot eat the budget.
const (
	verifyReadyBudget   = 30 * time.Second
	verifyReadyInterval = time.Second
	verifyReadyAttempt  = 5 * time.Second
)

// safekeeperStatusPath is the safekeeper's liveness route. It sits on the
// safekeeper's unauthenticated allow-list (ALLOWLIST_ROUTES in
// compute/safekeeper/src/http/routes.rs), so the probe carries no token.
const safekeeperStatusPath = "/v1/status"

// verifyReady confirms the deployed endpoint ANSWERS on the safekeeper's HTTP
// API, over the same Service DNS the controller's own follow-up calls use. Only
// then does the create task succeed: an endpoint the controller cannot call is
// not created, whatever the pod is doing.
//
// It is a round trip rather than a read of the Service's `Endpoints`, because
// routable and ready are different facts and only the first is visible there. An
// address is listed ready because the POD's readiness probe passed — and that
// probe is on pg-compute — while the port is listed because the Service declares
// it; neither observes the safekeeper. Reading Endpoints proves kube-proxy will
// route to sk-http and leaves "something answers there" resting on the pod's
// internal start order, which is the very thing the fork then depends on. A GET
// subsumes the routing check anyway: a Service with no backend REJECTs.
func (k *KubernetesDeployer) verifyReady(ctx context.Context, release string) error {
	// Out of cluster the endpoint's Service DNS does not resolve from here — and
	// the controller could not make the follow-up call either, so there is nothing
	// to gate (tests, dev).
	if !inCluster() {
		return nil
	}
	return awaitSafekeeper(ctx, k.EndpointSafekeeperURL(release), verifyReadyBudget, verifyReadyInterval)
}

// inCluster reports whether the controller is running inside Kubernetes, and so
// shares a network with the endpoint Services it deploys.
func inCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != ""
}

// awaitSafekeeper polls baseURL's status route until it answers 2xx, returning
// the last failure if the budget runs out first. Both failure modes are worth
// retrying and neither is distinguished: a refused connection is a Service with
// no backend yet, a 5xx a safekeeper that holds the port but is not serving.
func awaitSafekeeper(ctx context.Context, baseURL string, budget, interval time.Duration) error {
	client := &http.Client{Timeout: verifyReadyAttempt}
	defer client.CloseIdleConnections()
	u := strings.TrimRight(baseURL, "/") + safekeeperStatusPath
	deadline := time.Now().Add(budget)
	for {
		err := probeSafekeeper(ctx, client, u)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("its safekeeper did not answer GET %s within %s: %w", u, budget, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// probeSafekeeper is one GET, drained and closed so the connection can be reused
// across attempts.
func probeSafekeeper(ctx context.Context, client *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// tailnetAddress derives the endpoint's Tailscale (tailnet) host:port when the
// endpoint Service is exposed on the tailnet and a TailnetSuffix is configured;
// otherwise it returns "". The Tailscale operator names the proxy device after
// the explicit "tailscale.com/hostname" annotation if present, else
// "<namespace>-<serviceName>" (the release name is the Service name), so the
// MagicDNS name is "<device>.<TailnetSuffix>". This is a pure function of the
// name + config — it does not confirm the tailnet device is registered yet.
func (k *KubernetesDeployer) tailnetAddress(release string) string {
	if k.cfg.TailnetSuffix == "" {
		return ""
	}
	if k.cfg.ServiceAnnotations["tailscale.com/expose"] != "true" {
		return ""
	}
	device := k.cfg.ServiceAnnotations["tailscale.com/hostname"]
	if device == "" {
		device = k.cfg.Namespace + "-" + release
	}
	return fmt.Sprintf("%s.%s:%d", device, k.cfg.TailnetSuffix, k.cfg.ComputePort)
}

// errNotInCluster is returned when there is no in-cluster API to talk to.
var errNotInCluster = errors.New("not running in-cluster")

// errK8sNotFound is a 404 from the Kubernetes API: the object does not exist.
var errK8sNotFound = errors.New("not found")

// k8sAPIGet does an authenticated GET against the in-cluster Kubernetes API and
// returns the response body, or (nil, false) when not running in-cluster or on
// any error (callers fall back to a static address).
//
// Use k8sAPIGetErr where the caller has to tell a missing object or a denied
// request from an empty answer; this shape collapses them all to false.
func (k *KubernetesDeployer) k8sAPIGet(ctx context.Context, path string) ([]byte, bool) {
	body, err := k.k8sAPIGetErr(ctx, path)
	return body, err == nil
}

// k8sAPIGetErr is k8sAPIGet with the reason kept. A 404 and a 403 are ordinary
// results here — a Service that was never installed, or a ServiceAccount missing
// the RBAC to read it — and both are indistinguishable from "no address yet"
// once they are flattened to a bool, which is how a shard sits in `building`
// forever with nothing in the log to say why.
func (k *KubernetesDeployer) k8sAPIGetErr(ctx context.Context, path string) ([]byte, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errNotInCluster
	}
	saDir := serviceAccountDir
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("service account CA is not valid PEM")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s:%s%s", host, port, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %s: %w", path, resp.Status, errK8sNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// k8sAPIPatch does an authenticated PATCH against the in-cluster Kubernetes API
// with the given content type (e.g. strategic-merge-patch+json). It mirrors
// k8sAPIGet's SA-token + CA setup but, unlike the best-effort GET, returns an
// error so callers can distinguish "not in-cluster" and API rejections (a resize
// must not silently no-op). On success returns the response body and 2xx status.
func (k *KubernetesDeployer) k8sAPIPatch(ctx context.Context, path, contentType string, body []byte) ([]byte, int, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, 0, fmt.Errorf("not running in-cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	saDir := serviceAccountDir
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, 0, fmt.Errorf("read service-account token: %w", err)
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, 0, fmt.Errorf("read service-account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, 0, fmt.Errorf("parse service-account CA")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, fmt.Sprintf("https://%s:%s%s", host, port, path), strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBody, resp.StatusCode, fmt.Errorf("k8s PATCH %s -> %s: %s", path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	return respBody, resp.StatusCode, nil
}

// ShardAddress reports where an ingress shard's load balancer answers, and
// whether that is a hostname or an address — which is what decides whether each
// endpoint on the shard publishes a CNAME or an A record.
//
// One shot, unlike serviceLBAddress: the caller is a reconcile loop with its own
// cadence, and a shard that is not ready yet is an ordinary state to report
// rather than something to block on. `ok` is false while the cloud has not
// assigned anything.
// ShardPorts reads the listener ports a shard's Service publishes and the family
// its release was installed as, satisfying ingress.AddressLookup.
//
// From the same Service as the address, so the controller can check what is
// actually deployed against the row it registered. The two are installed
// separately — the row from the controller's config, the release from its
// chart's values — and nothing else would notice them diverging.
//
// The family comes from the label the shard chart stamps on every object it
// renders; an empty one means the Service predates the label, which is treated
// as "cannot tell" rather than a mismatch.
func (k *KubernetesDeployer) ShardPorts(ctx context.Context, release string) ([]int, string, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", k.cfg.Namespace, release)
	body, err := k.k8sAPIGetErr(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("read shard service %s: %w", release, err)
	}
	var out struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", fmt.Errorf("parse shard service %s: %w", release, err)
	}
	ports := make([]int, 0, len(out.Spec.Ports))
	for _, p := range out.Spec.Ports {
		ports = append(ports, p.Port)
	}
	return ports, out.Metadata.Labels["amp.yugabyte.com/ingress-family"], nil
}

// ShardAddress reads where a shard's load balancer answers, satisfying
// ingress.AddressLookup.
//
// The three outcomes are kept apart deliberately. A real error means the lookup
// itself failed — a Service that was never installed, a ServiceAccount without
// the RBAC to read it, a transport failure. `addressed` false with no error
// means the Service is there and the cloud simply has not given it an address
// yet, which is the ordinary case while a shard builds. Collapsing those two
// into one boolean is how a shard sits in `building` forever with nothing in the
// log to say which it was.
//
// Returned as plain values rather than a shared type so this package keeps
// satisfying ingress.AddressLookup structurally, without importing it.
func (k *KubernetesDeployer) ShardAddress(ctx context.Context, release string) (host string, isHostname, addressed bool, err error) {
	ip, hostname, err := k.loadBalancerIngress(ctx, release)
	switch {
	case err != nil:
		return "", false, false, fmt.Errorf("shard %s: %w", release, err)
	case hostname != "":
		return hostname, true, true, nil
	case ip != "":
		return ip, false, true, nil
	}
	return "", false, false, nil
}

// loadBalancerIngress reads, once, what the cloud has assigned a LoadBalancer
// Service: both empty until it has. A missing Service is an error wrapping
// errK8sNotFound. The caller picks between the two, because the shard path and
// the nlb_* path prefer them in opposite orders.
func (k *KubernetesDeployer) loadBalancerIngress(ctx context.Context, service string) (ip, hostname string, err error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", k.cfg.Namespace, service)
	body, err := k.k8sAPIGetErr(ctx, path)
	if err != nil {
		return "", "", fmt.Errorf("read service %s: %w", service, err)
	}
	var out struct {
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP       string `json:"ip"`
					Hostname string `json:"hostname"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("parse service %s: %w", service, err)
	}
	if len(out.Status.LoadBalancer.Ingress) == 0 {
		return "", "", nil
	}
	ing := out.Status.LoadBalancer.Ingress[0]
	return ing.IP, ing.Hostname, nil
}

// serviceLBAddress returns "<externalIP>:<computePort>" once the endpoint's
// LoadBalancer Service has an ingress address, polling up to ~2min (cloud LB
// provisioning is asynchronous). Returns "" on timeout/error.
func (k *KubernetesDeployer) serviceLBAddress(ctx context.Context, release string) string {
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", k.cfg.Namespace, release)
	deadline := time.Now().Add(lbAddressPollTimeout)
	for {
		if body, ok := k.k8sAPIGet(ctx, path); ok {
			var out struct {
				Status struct {
					LoadBalancer struct {
						Ingress []struct {
							IP       string `json:"ip"`
							Hostname string `json:"hostname"`
						} `json:"ingress"`
					} `json:"loadBalancer"`
				} `json:"status"`
			}
			if json.Unmarshal(body, &out) == nil && len(out.Status.LoadBalancer.Ingress) > 0 {
				ing := out.Status.LoadBalancer.Ingress[0]
				host := ing.IP
				if host == "" {
					host = ing.Hostname
				}
				if host != "" {
					return fmt.Sprintf("%s:%d", host, k.cfg.ComputePort)
				}
			}
		}
		if time.Now().After(deadline) {
			k.logger.Warn("LoadBalancer ingress IP not assigned in time; using Service DNS", "endpoint", release)
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(lbAddressPollInterval):
		}
	}
}

// nodePortClientAddress returns "<nodeIP>:<nodePort>" for the endpoint's
// NodePort Service: the nodePort allocated for the compute port, on any cluster
// node's address (an ExternalIP when the nodes have one, else an InternalIP /
// VPC address). Returns "" when not in-cluster / on error (caller falls back to
// the Service DNS). NB: reaching a NodePort may require a firewall rule allowing
// the node-port range to the nodes.
func (k *KubernetesDeployer) nodePortClientAddress(ctx context.Context, release string) string {
	port := k.serviceNodePort(ctx, release)
	if port == 0 {
		return ""
	}
	ip := k.aNodeIP(ctx)
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// serviceNodePort returns the nodePort allocated for the endpoint's compute port
// (matched by service port number), or 0 on error / before it is assigned.
func (k *KubernetesDeployer) serviceNodePort(ctx context.Context, release string) int {
	body, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/services/%s", k.cfg.Namespace, release))
	if !ok {
		return 0
	}
	return parseServiceNodePort(body, k.cfg.ComputePort)
}

// parseServiceNodePort extracts the nodePort assigned to the service port whose
// port number is computePort, from a Kubernetes Service JSON document.
func parseServiceNodePort(body []byte, computePort int) int {
	var out struct {
		Spec struct {
			Ports []struct {
				Port     int `json:"port"`
				NodePort int `json:"nodePort"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &out) != nil {
		return 0
	}
	for _, p := range out.Spec.Ports {
		if p.Port == computePort {
			return p.NodePort
		}
	}
	return 0
}

// detectCloud reports which cloud this cluster runs on ("aws", "gcp", "azure"),
// or "" when it cannot tell. Read from a node's spec.providerID, which the
// cloud-controller-manager sets, so it cannot drift from the cluster it
// describes. Reading it needs `nodes get, list` (rbac.clusterScoped); an explicit
// KubernetesConfig.Cloud (--deploy-cloud) skips the lookup and wins, both for a
// cluster whose provider IDs say something unexpected and for one where the
// controller does not hold that permission.
func (k *KubernetesDeployer) detectCloud(ctx context.Context) string {
	if k.cfg.Cloud != "" {
		return k.cfg.Cloud
	}
	k.cloudMu.Lock()
	defer k.cloudMu.Unlock()
	if k.cloud != "" {
		return k.cloud
	}
	body, ok := k.k8sAPIGet(ctx, "/api/v1/nodes")
	if !ok {
		k.logger.Warn("could not list nodes to detect the cloud; an internal load balancer will be refused by the chart. " +
			"The controller needs nodes get/list — set rbac.clusterScoped=true, or pass --deploy-cloud")
		return ""
	}
	cloud := parseCloudProvider(body)
	if cloud == "" {
		k.logger.Warn("could not determine the cloud from node provider IDs; an internal load balancer will be refused by the chart. " +
			"Pass --deploy-cloud to say which cloud this is")
		return ""
	}
	k.logger.Info("detected cloud for load-balancer annotations", "cloud", cloud)
	k.cloud = cloud
	return cloud
}

// parseCloudProvider maps the provider-ID scheme on any node of a Kubernetes
// NodeList to a cloud name. The schemes are set by each cloud-controller-manager:
// "aws:///us-east-2a/i-0123", "gce://project/us-west1-b/gke-...",
// "azure:///subscriptions/...". Returns "" for anything else (kind, bare metal,
// a provider we have no annotations for).
func parseCloudProvider(body []byte) string {
	var out struct {
		Items []struct {
			Spec struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &out) != nil {
		return ""
	}
	for _, n := range out.Items {
		switch {
		case strings.HasPrefix(n.Spec.ProviderID, "aws://"):
			return "aws"
		// GKE nodes report "gce://"; the cloud is still GCP.
		case strings.HasPrefix(n.Spec.ProviderID, "gce://"):
			return "gcp"
		case strings.HasPrefix(n.Spec.ProviderID, "azure://"):
			return "azure"
		}
	}
	return ""
}

// aNodeIP returns the address of any Ready cluster node, preferring an
// ExternalIP (reachable from outside the cluster) over an InternalIP (VPC).
// Returns "" when not in-cluster / on error.
func (k *KubernetesDeployer) aNodeIP(ctx context.Context) string {
	body, ok := k.k8sAPIGet(ctx, "/api/v1/nodes")
	if !ok {
		return ""
	}
	return parseANodeIP(body)
}

// parseANodeIP picks a node address from a Kubernetes NodeList JSON document:
// the first ExternalIP of a Ready node, else the first InternalIP of a Ready node.
func parseANodeIP(body []byte) string {
	var out struct {
		Items []struct {
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &out) != nil {
		return ""
	}
	firstInternal := ""
	for _, n := range out.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case "ExternalIP":
				if a.Address != "" {
					return a.Address
				}
			case "InternalIP":
				if a.Address != "" && firstInternal == "" {
					firstInternal = a.Address
				}
			}
		}
	}
	return firstInternal
}

// DeleteEndpoint uninstalls the endpoint's Helm release (kill-before-create /
// retire semantics: waits for the resources to be gone). id is the release name
// returned by CreateEndpoint. Idempotent: an absent release is tolerated.
func (k *KubernetesDeployer) DeleteEndpoint(ctx context.Context, id string) error {
	return k.helm.Uninstall(ctx, id, k.cfg.Namespace, true, k.cfg.Timeout)
}

// containerResourcesPatch is the strategic-merge fragment that sets one
// container's resources (matched by Name, the strategic-merge merge key).
type containerResourcesPatch struct {
	Name      string `json:"name"`
	Resources struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
	} `json:"resources"`
}

// ResizeEndpoint changes an endpoint's compute reservation by resizing the
// running pod in place (k8s 1.33+ `resize` subresource) and reports whether the
// pod took the change without a restart. CPU/memory are set as both request and
// limit, preserving Guaranteed QoS (a prerequisite for in-place resize).
//
// It deliberately does NOT patch the endpoint Deployment's pod template. Editing
// `spec.template` resources changes the pod-template-hash, which makes the
// Deployment controller start an ordinary rollout and *recreate* the pod — the
// opposite of an in-place resize (in-place is a Pod-only operation; workload
// controllers are not resize-aware through k8s 1.35). Durability instead rides on
// the DB-side persistence of the new size (the resize task's first write): a
// controller-driven redeploy re-renders the chart from that config. Known gap:
// an *involuntary* recreate (node loss / eviction) before the next redeploy
// brings the pod back at the old size until then; tracked as a follow-up.
func (k *KubernetesDeployer) ResizeEndpoint(ctx context.Context, spec ResizeSpec) (ResizeResult, error) {
	cpu, memory := spec.CPU, spec.Memory
	if cpu == "" {
		cpu = defaultEndpointCPU
	}
	if memory == "" {
		memory = defaultEndpointMemory
	}
	var c containerResourcesPatch
	c.Name = endpointContainerName
	c.Resources.Requests = map[string]string{"cpu": cpu, "memory": memory}
	c.Resources.Limits = map[string]string{"cpu": cpu, "memory": memory}
	const smp = "application/strategic-merge-patch+json"

	// In-place resize of the running pod via the resize subresource.
	pod, err := k.podName(ctx, spec.Name)
	if err != nil {
		return ResizeResult{}, fmt.Errorf("find pod for endpoint %s: %w", spec.Name, err)
	}
	podPatch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"containers": []containerResourcesPatch{c}},
	})
	if err != nil {
		return ResizeResult{}, err
	}
	resizePath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/resize", k.cfg.Namespace, pod)
	body, _, err := k.k8sAPIPatch(ctx, resizePath, smp, podPatch)
	if err != nil {
		return ResizeResult{}, fmt.Errorf("resize pod %s in place: %w", pod, err)
	}
	result := classifyResize(body, cpu, memory, endpointContainerName)
	// The synchronous PATCH response rarely reflects the kubelet's outcome yet, so
	// re-poll while the resize is still settling (in progress / not yet applied),
	// up to the deadline, so the reported outcome is the kubelet's real verdict
	// (Completed / Infeasible / Deferred) rather than a premature Completed. Paced
	// so we don't hammer the apiserver.
	deadline := time.Now().Add(resizeSettleTimeout)
	for result.Outcome == ResizePending && result.Message == resizeInProgressMsg && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return result, nil
		case <-time.After(resizeSettlePollInterval):
		}
		podBody, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", k.cfg.Namespace, pod))
		if !ok {
			break
		}
		result = classifyResize(podBody, cpu, memory, endpointContainerName)
	}
	k.logger.Info("endpoint resized", "endpoint", spec.Name, "pod", pod, "cpu", cpu, "memory", memory, "in_place", result.Outcome)
	return result, nil
}

// serviceAccountDir is where the in-cluster API helpers read the pod's token and
// CA. A variable only so a test can stand in a fake API server.
var serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// UpdateSourceRanges sets loadBalancerSourceRanges on the endpoint's
// load-balancer Service (the chart's <release>-lb), satisfying
// deploy.SourceRangesUpdater. A JSON merge patch replaces the list whole, and
// null removes the field, which is what the chart renders for an empty list, so
// the next helm upgrade of the release finds nothing to change.
func (k *KubernetesDeployer) UpdateSourceRanges(ctx context.Context, name string, cidrs []string) error {
	patch, err := sourceRangesPatch(cidrs)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/services/%s-lb", k.cfg.Namespace, name)
	if _, _, err := k.k8sAPIPatch(ctx, path, "application/merge-patch+json", patch); err != nil {
		return fmt.Errorf("patch load balancer source ranges of %s: %w", name, err)
	}
	k.logger.Info("endpoint load balancer source ranges updated", "endpoint", name, "allowed_cidrs", cidrs)
	return nil
}

func sourceRangesPatch(cidrs []string) ([]byte, error) {
	var ranges any = cidrs
	if len(cidrs) == 0 {
		ranges = nil
	}
	return json.Marshal(map[string]any{"spec": map[string]any{"loadBalancerSourceRanges": ranges}})
}

// podName returns the name of the endpoint's (single) pod, found by its instance
// label.
func (k *KubernetesDeployer) podName(ctx context.Context, release string) (string, error) {
	q := url.Values{"labelSelector": {"app.kubernetes.io/instance=" + release}, "limit": {"1"}}
	body, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", k.cfg.Namespace, q.Encode()))
	if !ok {
		return "", fmt.Errorf("list pods (not in-cluster or API error)")
	}
	var out struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Items) == 0 || out.Items[0].Metadata.Name == "" {
		return "", fmt.Errorf("no pod found for instance %s", release)
	}
	return out.Items[0].Metadata.Name, nil
}

// classifyResize maps a Pod's resize conditions to a ResizeResult. In k8s 1.33
// the kubelet sets a `PodResizePending` condition (reason `Infeasible` when it
// can't be satisfied, `Deferred` when it will retry) and a transient
// `PodResizeInProgress` condition while applying.
//
// The absence of a resize condition is NOT sufficient to call it Completed: on
// the synchronous /resize PATCH response (and briefly after), the kubelet has
// not yet observed the change, so no condition is set *yet* and the allocated
// resources still show the old size. Reporting Completed there would falsely
// succeed a resize the kubelet may later mark Infeasible. So Completed requires
// the pod's actual allocated resources (status.containerStatuses[].resources) to
// match the requested wantCPU/wantMem; until they do (and while no
// pending/infeasible condition is set) it stays "in progress" so the caller
// re-polls.
func classifyResize(podBody []byte, wantCPU, wantMem, container string) ResizeResult {
	var p struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name      string `json:"name"`
				Resources struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(podBody, &p); err != nil {
		return ResizeResult{Outcome: ResizePending, Message: "could not read resize status"}
	}
	for _, cond := range p.Status.Conditions {
		if cond.Status != "True" {
			continue
		}
		switch cond.Type {
		case "PodResizePending":
			if cond.Reason == "Infeasible" {
				msg := cond.Message
				if msg == "" {
					msg = "node cannot satisfy the requested resources"
				}
				return ResizeResult{Outcome: ResizeInfeasible, Message: msg}
			}
			return ResizeResult{Outcome: ResizePending, Message: "deferred; will apply when resources free up"}
		case "PodResizeInProgress":
			return ResizeResult{Outcome: ResizePending, Message: resizeInProgressMsg}
		}
	}
	// No pending/infeasible/in-progress condition. Only Completed if the pod's
	// actual allocated resources already match the request; otherwise the kubelet
	// has not applied it yet — keep the caller polling rather than reporting a
	// premature success.
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != container {
			continue
		}
		if strings.TrimSpace(cs.Resources.Requests["cpu"]) == strings.TrimSpace(wantCPU) &&
			strings.TrimSpace(cs.Resources.Requests["memory"]) == strings.TrimSpace(wantMem) {
			return ResizeResult{Outcome: ResizeCompleted, Message: "resized in place"}
		}
		return ResizeResult{Outcome: ResizePending, Message: resizeInProgressMsg}
	}
	// The endpoint container's status is not present yet — still settling.
	return ResizeResult{Outcome: ResizePending, Message: resizeInProgressMsg}
}

// clientAddress is the address recorded for CLIENTS of an endpoint: whatever
// external address the Service type yielded — a LoadBalancer ingress, a NodePort
// — and otherwise the endpoint's own Service DNS. external is "" when the type
// has none to offer (ClusterIP) or when the lookup failed.
//
// It is never the pod's address. A pod IP dies at the next reschedule, and
// nothing re-resolves this value until the endpoint's next controller-driven
// redeploy — so an ordinary extension add or eviction would silently invalidate
// the connection string the console handed out (CLOUDGA-36993). The Service DNS
// is the address the controller has always used for its own calls to the
// endpoint, and verifyReady gates the create on it resolving.
func clientAddress(serviceType, serviceDNS, external string) string {
	switch serviceType {
	case serviceTypeLoadBalancer, serviceTypeNodePort:
		if external != "" {
			return external
		}
	}
	return serviceDNS
}

// address is the in-cluster DNS name + port of an endpoint's compute Service.
// The Service name equals the release's fullnameOverride (= the release name).
func (k *KubernetesDeployer) address(release string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", release, k.cfg.Namespace, k.cfg.ComputePort)
}

// EndpointSafekeeperURL: the endpoint's safekeeper HTTP API is published on its
// Service (ports.safekeeperHttp), reachable at the same stable Service DNS the
// controller dials (CONTROLLER_REACHABLE_HOST). id is the release name.
func (k *KubernetesDeployer) EndpointSafekeeperURL(id string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", id, k.cfg.Namespace, safekeeperHTTPPort)
}

// EndpointSupervisorURL: the settings API is published on the same Service as
// the daemon APIs, under the same gate — the chart opens it only when a
// controller URL is set, which a controller-driven install always sets.
func (k *KubernetesDeployer) EndpointSupervisorURL(id string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", id, k.cfg.Namespace, supervisorHTTPPort)
}

// renderClientAddress re-renders the endpoint so its certificate covers the
// address clients were actually given, and so an nlb_* endpoint's DNS record
// points at its load balancer.
//
// A no-op in every case but the ones it exists for: compute TLS off, an address
// the chart already derives (the Service DNS, or a published name), or a repeat
// of one already in the values all skip helm unless the record changed. So an
// ordinary ClusterIP or ingress_*-with-DNS create pays nothing.
//
// No --wait: the pod is already up and this changes nothing about it. Waiting
// would only hold the create open while cert-manager works, and the endpoint is
// serving throughout on the certificate it already has.
func (k *KubernetesDeployer) renderClientAddress(ctx context.Context, release string, vals *chartValues, addr string, dnsChanged bool) error {
	// existing-secret mounts a Secret this chart did not create, whose SANs are
	// the issuer's business and not reachable from here.
	extendTLS := vals.ComputeTLS != nil && vals.ComputeTLS.Mode == computeTLSChartModeIssue &&
		!computeTLSCovers(vals.ComputeTLS, addr, k.address(release), vals.Service.publishedName())
	if !extendTLS && !dnsChanged {
		return nil
	}
	if extendTLS {
		addComputeTLSAddress(vals.ComputeTLS, addr)
		k.logger.Info("extending the endpoint certificate to its client address",
			"endpoint", release, "address", addr,
			"dns_names", vals.ComputeTLS.DnsNames, "ip_addresses", vals.ComputeTLS.IpAddresses)
	}
	if dnsChanged {
		k.logger.Info("publishing the endpoint's DNS record",
			"endpoint", release, "name", vals.Service.DNS.Name, "target", vals.Service.DNS.Target)
	}
	return k.helm.UpgradeInstall(ctx, helm.Release{
		Name:      release,
		Chart:     k.cfg.ChartPath,
		Namespace: k.cfg.Namespace,
		Values:    *vals,
		Timeout:   k.cfg.Timeout,
	})
}

// ServesComputeTLS reports whether endpoints are deployed with a server
// certificate, in either issuance mode. Without one, Postgres offers no TLS and
// the documentdb gateway self-signs.
func (k *KubernetesDeployer) ServesComputeTLS() bool {
	return k.cfg.ComputeTLSMode != "" && k.cfg.ComputeTLSMode != "disable"
}

// ServerCert is what a client needs to verify an endpoint.
type ServerCert struct {
	// CA is the PEM a client must trust, or "" when the certificate chains to a
	// public root or the pod serves none.
	CA string
	// GatewayServes is whether the pod's documentdb gateway serves the compute's
	// certificate. False when it self-signs, and when there is no gateway.
	GatewayServes bool
}

// EndpointServerCert reports how a client verifies this endpoint's server
// certificate.
//
// The flag says what the NEXT render will do; the pod runs whatever it was last
// rendered with, and nothing re-renders on a controller upgrade, a rollback, or
// a switch between issuance modes. So the answer comes from the endpoint's
// Deployment alone: no `compute-tls` volume means nothing serves a certificate,
// the volume's `secretName` is the Secret the pod actually mounts, and a
// gateway from before it served one still self-signs.
//
// Whether the certificate names the address is deliberately not checked. A
// certificate that does not is a misconfiguration, and a client that verifies
// and fails surfaces it; handing that client a string that skips verification
// would hide it.
//
// The CA is read from the endpoint's own certificate Secret rather than from a
// deployment-wide bundle, because that Secret is what actually signed what the
// compute presents: cert-manager writes the issuing CA into the leaf's `ca.crt`
// for a `ca` issuer, so a customer's own CA is reported as faithfully as the
// per-cluster root. It is the same file the supervisor reads for
// `compute_ca_not_after_seconds`.
//
// An empty CA is a real answer, not a failure. An ACME issuer writes no
// `ca.crt`, and a client verifying a publicly-trusted certificate needs
// nothing — handing it a bundle to install would be worse than handing it
// nothing.
func (k *KubernetesDeployer) EndpointServerCert(ctx context.Context, id string) (ServerCert, error) {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", k.cfg.Namespace, id)
	body, err := k.k8sAPIGetErr(ctx, path)
	switch {
	case errors.Is(err, errK8sNotFound):
		return ServerCert{}, nil
	case err != nil:
		return ServerCert{}, fmt.Errorf("read endpoint deployment %s: %w", id, err)
	}
	rendered, err := renderedComputeTLS(body)
	if err != nil {
		return ServerCert{}, fmt.Errorf("parse endpoint deployment %s: %w", id, err)
	}
	secret := rendered.secret
	if secret == "" {
		return ServerCert{}, nil
	}
	path = fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", k.cfg.Namespace, secret)
	body, err = k.k8sAPIGetErr(ctx, path)
	switch {
	// The pod mounts a Secret that is not there, so it is stuck mounting and
	// serves nothing.
	case errors.Is(err, errK8sNotFound):
		return ServerCert{}, nil
	case err != nil:
		return ServerCert{}, fmt.Errorf("read certificate secret %s: %w", secret, err)
	}
	var out struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ServerCert{}, fmt.Errorf("parse certificate secret %s: %w", secret, err)
	}
	sc := ServerCert{GatewayServes: rendered.gatewayServes}
	if raw := out.Data["ca.crt"]; raw != "" {
		ca, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return ServerCert{}, fmt.Errorf("decode ca.crt in %s: %w", secret, err)
		}
		sc.CA = string(ca)
	}
	return sc, nil
}

// renderedTLS is what an endpoint's Deployment was rendered with, as far as its
// server certificate goes.
type renderedTLS struct {
	// secret is the compute-tls volume's Secret, which Postgres serves; "" when
	// the pod has no such volume.
	secret string
	// gatewayServes: the pod has a documentdb gateway, and it serves the
	// mounted certificate rather than self-signing.
	gatewayServes bool
}

// renderedComputeTLS reads renderedTLS from a Deployment's JSON. The names are
// the endpoint chart's: the `compute-tls` volume (and its `secretName`) and, in
// the gateway container,
// DOCUMENTDB_TLS_CERT_FILE, which a chart from before the gateway served the
// certificate never sets.
func renderedComputeTLS(deploymentJSON []byte) (renderedTLS, error) {
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name   string `json:"name"`
						Secret struct {
							SecretName string `json:"secretName"`
						} `json:"secret"`
					} `json:"volumes"`
					Containers []struct {
						Name string `json:"name"`
						Env  []struct {
							Name string `json:"name"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(deploymentJSON, &d); err != nil {
		return renderedTLS{}, err
	}
	pod := d.Spec.Template.Spec
	var r renderedTLS
	for _, v := range pod.Volumes {
		if v.Name == "compute-tls" {
			r.secret = v.Secret.SecretName
		}
	}
	for _, c := range pod.Containers {
		if c.Name != "documentdb-gateway" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "DOCUMENTDB_TLS_CERT_FILE" {
				r.gatewayServes = true
			}
		}
	}
	return r, nil
}

// addComputeTLSAddress records one client-facing address in the compute
// certificate's SANs, as an IP SAN or a DNS name depending on what it is.
//
// X.509 keeps the two apart and so must this: an address listed in dnsNames is
// NOT matched by a client dialling that address, and a name listed in
// ipAddresses is rejected outright. Getting it wrong yields a certificate that
// renders, issues, and then fails every verify-full — the silent shape this is
// all meant to avoid.
//
// host may carry a port; it is stripped. Duplicates are dropped so a redeploy
// that re-resolves the same address does not grow the SAN list and force a
// reissue every time.
func addComputeTLSAddress(v *computeTlsValues, host string) {
	if v == nil || host == "" {
		return
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		if !slices.Contains(v.IpAddresses, host) {
			v.IpAddresses = append(v.IpAddresses, host)
		}
		return
	}
	if !slices.Contains(v.DnsNames, host) {
		v.DnsNames = append(v.DnsNames, host)
	}
}

// computeTLSCovers reports whether the rendered values already carry host, so
// the second pass can skip a redundant helm upgrade — and with it a certificate
// reissue and the reload that follows.
func computeTLSCovers(v *computeTlsValues, host, serviceDNS, publishedName string) bool {
	if v == nil {
		return true // compute TLS is off; nothing to cover.
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(serviceDNS); err == nil {
		serviceDNS = h
	}
	// The chart adds the Service DNS itself when includeServiceDns is on, and a
	// published name always.
	if v.IncludeServiceDns && host == serviceDNS {
		return true
	}
	if publishedName != "" && host == publishedName {
		return true
	}
	return slices.Contains(v.IpAddresses, host) || slices.Contains(v.DnsNames, host)
}

// computeTLSChartMode maps the flag's spelling onto the chart's. The flag is
// kebab-case like every other flag value the controller takes; the chart value is
// camelCase like every other key in a values file. Neither convention is worth
// bending, so the translation lives here rather than in either surface.
// computeTLSChartModeIssue is the chart spelling of the per-endpoint issuance
// mode — the only one whose SANs this controller can extend.
const computeTLSChartModeIssue = "issue"

func computeTLSChartMode(mode string) string {
	if mode == "existing-secret" {
		return "existingSecret"
	}
	return mode
}

// chartValues mirrors the subset of deploy/helm/yb-amp-endpoint values.yaml
// the controller drives. JSON tags match the chart keys; helm reads the
// marshaled document as a values file.
// documentdbGatewayValues is the subset of the chart's documentdbGateway block
// the controller sets. `enabled` is deliberately absent: the chart's own helper
// already follows endpoint.extensions, so the controller does not need to
// restate a decision the rendered extension list already carries.
type documentdbGatewayValues struct {
	Image imageValues `json:"image"`
}

type chartValues struct {
	FullnameOverride string              `json:"fullnameOverride"`
	Image            imageValues         `json:"image"`
	ImagePullSecrets []map[string]string `json:"imagePullSecrets,omitempty"`
	Yugabyte         yugabyteValues      `json:"yugabyte"`
	Endpoint         endpointValues      `json:"endpoint"`
	Controller       controllerValues    `json:"controller"`
	Service          serviceValues       `json:"service"`
	// TLS is nil (omitted) for a plaintext deployment; set when the endpoints
	// present a certificate to YugabyteDB.
	TLS *tlsValues `json:"tls,omitempty"`
	// ComputeTLS is nil (omitted) unless the computes present a certificate to
	// their own clients. Independent of TLS above.
	ComputeTLS *computeTlsValues `json:"computeTls,omitempty"`
	// UserSplit is nil (omitted) to leave the chart's default, one user.
	UserSplit *userSplitValues `json:"userSplit,omitempty"`
	// Persistence is nil (omitted) for the chart's emptyDir default; set to
	// provision the endpoint's data volume as a PVC of a storage class.
	Persistence *persistenceValues `json:"persistence,omitempty"`
	// RunDir sizes the emptyDir backing /data when no storage class is set. Nil
	// (omitted) leaves the chart default; applyDiskSizing fills it in from the
	// endpoint's CPU.
	RunDir *runDirValues `json:"runDir,omitempty"`
	// NodeSelector pins the endpoint pod to nodes with these labels (e.g. a
	// local-NVMe-SSD ephemeral pool); omitted leaves the chart's default (no
	// selector — schedules anywhere).
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// LFC is nil (omitted) to leave prewarm and offload at the chart's default.
	LFC *lfcValues `json:"lfc,omitempty"`
	// CommonLabels are the endpoint's identity labels and tags, labelled onto
	// every object of the release — never onto a selector.
	CommonLabels map[string]string `json:"commonLabels,omitempty"`
	// PodAnnotations carry the identity annotations, on the pod only.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// StartupProbe sizes how long the compute may take to listen before the
	// liveness probe starts judging it. Derived from the deploy timeout rather
	// than left to the chart, so raising --deploy-timeout for a slow cluster
	// actually raises the budget the kubelet enforces (#960).
	// DocumentDBGateway carries the sidecar's image. Omitted unless the branch
	// mounts documentdb, so a normal endpoint's values are unchanged. Present for
	// the same reason Image is: an empty tag in the chart resolves to its
	// appVersion, which is never the tag CI built.
	DocumentDBGateway *documentdbGatewayValues `json:"documentdbGateway,omitempty"`
	StartupProbe      startupProbeValues       `json:"startupProbe"`
	// ProgressDeadlineSeconds must cover the startup budget too. The Deployment
	// keeps Kubernetes' 600s default otherwise, and flips
	// Progressing=False/ProgressDeadlineExceeded there — so a pod still
	// legitimately inside a longer startup budget gets reported as a failed
	// rollout by anything waiting on condition=Available.
	ProgressDeadlineSeconds int `json:"progressDeadlineSeconds"`
}

// startupProbeValues mirrors the chart's startupProbe block. Only the budget is
// set: `enabled` is deliberately left to the chart, so turning the probe off
// stays a chart-level decision rather than something the controller pins on
// every install.
//
// The period is what the kubelet spends waiting after the compute is actually
// up, so it is kept short and the threshold carries the length.
type startupProbeValues struct {
	PeriodSeconds    int `json:"periodSeconds"`
	FailureThreshold int `json:"failureThreshold"`
}

// startupProbeBudget converts a deploy timeout into the chart's
// periodSeconds/failureThreshold pair.
//
// The kubelet must not give up before the deployer does. A probe budget shorter
// than the deploy timeout is the bug this exists to prevent: it kills the
// container mid-bootstrap, and raising --deploy-timeout achieves nothing because
// the shorter bound still wins. QA runs 35m against the chart's standalone 10m
// default, which is exactly that gap.
//
// The budget is `period * (threshold - 1)`, NOT `period * threshold`: the
// kubelet's prober probes immediately on entering its loop and only then waits
// on the period ticker, so N consecutive failures span N-1 periods. Sizing it
// the other way puts the kill one period BEFORE the deploy timeout — the exact
// failure this function exists to prevent, a period short of it rather than
// minutes.
//
// Rounded up, then +1 for that first immediate probe, and floored so a very
// short timeout cannot leave a compute less room than a plain `helm install`.
func startupProbeBudget(timeout time.Duration) startupProbeValues {
	const period = startupProbePeriodSeconds * time.Second
	threshold := int((timeout+period-1)/period) + 1
	if threshold < startupProbeMinFailureThreshold {
		threshold = startupProbeMinFailureThreshold
	}
	return startupProbeValues{PeriodSeconds: startupProbePeriodSeconds, FailureThreshold: threshold}
}

// progressDeadlineFor sizes the Deployment's own give-up clock so it outlasts
// the kubelet's startup budget. The two clocks are not merely skewed: the
// Deployment's Progressing timer last resets when the pod appears, so pod
// scheduling, image pull and (with a storageClass) volume attach all run inside
// it yet outside the kubelet's startup budget — which only starts ticking at
// container start. The headroom has to cover that pre-container overhead, so it
// is a fixed margin rather than one probe period: scaling it with the (now
// short) period would leave a full-budget bootstrap tripping ProgressDeadline
// over a slow image pull.
func progressDeadlineFor(timeout time.Duration) int {
	budget := startupProbeBudgetSeconds(startupProbeBudget(timeout))
	return int(budget.Seconds()) + progressDeadlineHeadroomSeconds
}

// startupProbeBudgetSeconds is what the kubelet actually enforces for these
// values. Shared with the tests so they cannot re-derive the formula and agree
// with a wrong implementation.
func startupProbeBudgetSeconds(v startupProbeValues) time.Duration {
	return time.Duration(v.PeriodSeconds*(v.FailureThreshold-1)) * time.Second
}

const (
	// startupProbePeriodSeconds matches the chart default; see startupProbeBudget.
	// Kept short so the kubelet notices a compute that has come up promptly: the
	// database listens ~3-4s after the container starts, and the startupProbe
	// period is how often the kubelet checks for that. It gates when the pod
	// leaves "starting"; readiness then re-probes on its own period, so the total
	// time-to-Ready is bounded by both, not this alone. The threshold carries the
	// total budget length instead of the period.
	startupProbePeriodSeconds = 2
	// startupProbeMinFailureThreshold is the chart's own default, chosen so the
	// floor budget is a round 10 minutes: 2 * (301 - 1) = 600s.
	startupProbeMinFailureThreshold = 301
	// progressDeadlineHeadroomSeconds is how far the Deployment's give-up clock
	// sits above the kubelet's startup budget. It covers the pre-container
	// overhead — pod scheduling, image pull, volume attach — that the Progressing
	// timer counts but the startup budget does not; a fixed margin, deliberately
	// not the probe period, so shortening the period does not shrink it.
	progressDeadlineHeadroomSeconds = 10
)

// lfcValues mirrors the chart's lfc block. The two are only ever set together:
// autoprewarm reloads what the offload saved, so either alone does nothing.
type lfcValues struct {
	Autoprewarm            bool   `json:"autoprewarm"`
	OffloadIntervalSeconds string `json:"offloadIntervalSeconds"`
}

type persistenceValues struct {
	// StorageClass provisions the endpoint's data volume as a PVC of that class;
	// empty leaves the chart's emptyDir default in place.
	StorageClass string `json:"storageClass,omitempty"`
	// Size is the PVC's requested storage when StorageClass is set.
	Size string `json:"size,omitempty"`
}

type serviceValues struct {
	// Type is the endpoint Service type ("ClusterIP" default, "NodePort", "LoadBalancer").
	Type string `json:"type,omitempty"`
	// Annotations on the Service (e.g. internal-LB annotations for LoadBalancer).
	Annotations map[string]string `json:"annotations,omitempty"`
	// Connectivity selects the chart's per-endpoint client reachability:
	// cluster_ip (default), nlb_internal or nlb_external. The NLB modes render a
	// second, Postgres-only Service in front of a load balancer.
	Connectivity string `json:"connectivity,omitempty"`
	// AllowedCidrs becomes that Service's loadBalancerSourceRanges.
	AllowedCidrs []string `json:"allowedCidrs,omitempty"`
	// Cloud tells the chart whose load-balancer annotations to render ("aws",
	// "gcp", "azure"). Every provider defaults a LoadBalancer Service to
	// internet-facing, so the chart refuses to render an INTERNAL one without
	// this rather than publish it by accident.
	Cloud string `json:"cloud,omitempty"`
	// Ingress is the shard slot for the ingress_* modes: the chart renders the
	// endpoint's route, its per-endpoint allowlist middleware and its DNS record
	// from it. Omitted entirely for every other mode.
	Ingress *ingressValues `json:"ingress,omitempty"`
	// DNS is an nlb_* endpoint's published name and, once its load balancer has
	// an address, the record's target. Omitted for every other mode and where
	// there is no name to publish.
	DNS *dnsValues `json:"dns,omitempty"`
}

// publishedName is the name the chart puts in the certificate itself, or "".
func (v serviceValues) publishedName() string {
	switch {
	case v.DNS != nil:
		return v.DNS.Name
	case v.Ingress != nil:
		return v.Ingress.DNSName
	}
	return ""
}

// dnsValues is what the chart needs to publish an nlb_* endpoint's name.
type dnsValues struct {
	Name       string `json:"name"`
	Target     string `json:"target,omitempty"`
	RecordType string `json:"recordType,omitempty"`
}

// setTarget points the record at a load balancer: a CNAME to a hostname (AWS),
// an A to an address (GCP, Azure).
func (d *dnsValues) setTarget(host string, isHostname bool) {
	d.Target = host
	d.RecordType = "A"
	if isHostname {
		d.RecordType = "CNAME"
	}
}

// publishLBName returns the client address for an nlb_* endpoint whose load
// balancer answers at lb ("host:port"): its published name where it has one,
// with the record pointed at lb's host, else lb itself.
func publishLBName(d *dnsValues, lb string) string {
	if d == nil {
		return lb
	}
	host, port, err := net.SplitHostPort(lb)
	if err != nil {
		return lb
	}
	d.setTarget(host, net.ParseIP(host) == nil)
	return net.JoinHostPort(d.Name, port)
}

// preRenderDNSTarget fills in a redeploy's record target from what the load
// balancer already has, before the release is rendered, so the render does not
// withdraw a live record. IP first, as serviceLBAddress prefers it: the second
// pass re-derives the target from that, and two preferences would flip the
// record between A and CNAME on every redeploy of a Service that reports both.
//
// A missing Service (a create) or no in-cluster API leaves the target empty.
// Any other read error is returned: rendering without the target would withdraw
// a live name over a transient error.
func preRenderDNSTarget(d *dnsValues, ip, hostname string, err error) error {
	switch {
	case errors.Is(err, errK8sNotFound), errors.Is(err, errNotInCluster):
		return nil
	case err != nil:
		return err
	case ip != "":
		d.setTarget(ip, false)
	case hostname != "":
		d.setTarget(hostname, true)
	}
	return nil
}

// addressAfterFailedSecondPass is the client address to report when the second
// pass failed. If that pass was to publish the record, the name has nothing
// behind it, so the load balancer's own address is the one that works.
func addressAfterFailedSecondPass(addr, lb string, dnsChanged bool) string {
	if dnsChanged && lb != "" {
		return lb
	}
	return addr
}

// dnsValuesFor renders an nlb_* endpoint's published name, or nil where there
// is none. The target is filled in by CreateEndpoint once the load balancer has
// an address.
func dnsValuesFor(spec EndpointSpec) *dnsValues {
	if !IsDedicatedLB(spec.Connectivity) || !PublishableDNSName(spec.DNSName) {
		return nil
	}
	return &dnsValues{Name: spec.DNSName}
}

// ingressValues is what the chart needs to publish one endpoint on a shard.
type ingressValues struct {
	// Shard is the Traefik release fronting this endpoint, and EntryPoint the
	// name of the listener its port is bound to on that release.
	Shard      string `json:"shard"`
	EntryPoint string `json:"entryPoint"`
	// DocumentDBEntryPoint is the shard's shared gateway listener; the chart
	// binds a route to it only for a gateway endpoint with a published name.
	DocumentDBEntryPoint string `json:"documentdbEntryPoint"`
	Port                 int    `json:"port"`
	// DNSName is the endpoint's own published name, and ShardHost/RecordType
	// what its record points at — a CNAME to the shard load balancer's hostname,
	// or an A to its address.
	DNSName    string `json:"dnsName"`
	ShardHost  string `json:"shardHost"`
	RecordType string `json:"recordType"`
}

type controllerValues struct {
	// URL the endpoint uses to reach the controller; empty = controller-free.
	URL string `json:"url,omitempty"`
}

type imageValues struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type yugabyteValues struct {
	HostPorts         string `json:"hostPorts"`
	User              string `json:"user"`
	Dbname            string `json:"dbname"`
	Password          string `json:"password,omitempty"`
	ExistingSecret    string `json:"existingSecret,omitempty"`
	ExistingSecretKey string `json:"existingSecretKey,omitempty"`
	Sslmode           string `json:"sslmode,omitempty"`
	Sslrootcert       string `json:"sslrootcert,omitempty"`
	Sslcert           string `json:"sslcert,omitempty"`
	Sslkey            string `json:"sslkey,omitempty"`
}

// tlsValues mirrors the chart's `tls` block: the per-release Certificate the
// chart renders and the CA bundle it mounts beside it.
type tlsValues struct {
	Enabled           bool             `json:"enabled"`
	IssuerRef         issuerRefValues  `json:"issuerRef"`
	CABundleConfigMap string           `json:"caBundleConfigMap"`
	Certificate       certificateValue `json:"certificate"`
}

type issuerRefValues struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
}

type certificateValue struct {
	Duration    string `json:"duration"`
	RenewBefore string `json:"renewBefore"`
}

// userSplitValues mirrors the chart's userSplit block; the ids keep the chart's
// defaults, which match the image.
type userSplitValues struct {
	Enabled bool `json:"enabled"`
}

// computeTlsValues mirrors the chart's computeTls block.
//
// The chart derives what it can — the Service DNS name and the ingress slot's
// published name — because a second source for a name it already holds is a
// second thing to keep in step. DnsNames and IpAddresses are the addresses it
// CANNOT derive, and both exist because a client can be handed an address the
// chart never sees at render time:
//
//   - an nlb_* endpoint's load-balancer address, which the cloud assigns after
//     the Service exists (see coverComputeTLSAddress, the second pass);
//   - an ingress_* endpoint in a deployment with no DNS zone, where the slot's
//     DNSName is unqualified and clients dial the shard's own address instead.
//
// Without them such an endpoint serves a certificate covering only the
// in-cluster name, so an external client gets sslmode=require and no better —
// which is the whole gap compute TLS exists to close.
type computeTlsValues struct {
	Enabled           bool             `json:"enabled"`
	Mode              string           `json:"mode"`
	IssuerRef         issuerRefValues  `json:"issuerRef"`
	ExistingSecret    string           `json:"existingSecret,omitempty"`
	IncludeServiceDns bool             `json:"includeServiceDns"`
	DnsNames          []string         `json:"dnsNames,omitempty"`
	IpAddresses       []string         `json:"ipAddresses,omitempty"`
	Certificate       certificateValue `json:"certificate"`
}

type endpointValues struct {
	PgVersion   string `json:"pgVersion"`
	ComputePort int    `json:"computePort"`
	BranchId    string `json:"branchId"`
	// EndpointId lets the supervisor find this endpoint's row in
	// amp_registry.endpoints and read its override columns. Omitted => no
	// override lookup, derived defaults only.
	EndpointId string `json:"endpointId,omitempty"`
	// Password is the compute default user's password in pg_authid's form,
	// rendered into the release's Secret. Omitted (a pre-credentials endpoint, or
	// any REDEPLOY, which has no verifier to render) leaves the supervisor's own
	// default in place.
	Password string `json:"password,omitempty"`
	// Resources is the pod's CPU + memory reservation. The chart renders each
	// value as both a request and a limit, so the pod gets Guaranteed QoS.
	Resources resourceValues `json:"resources"`
	// Extensions are the externalized extensions to mount as ImageVolumes; the
	// chart renders one volume per entry and derives the extension GUCs from
	// them. Omitted (empty) leaves the chart's no-extensions default.
	Extensions []extensionValues `json:"extensions,omitempty"`
}

// extensionValues mirrors an entry of the chart's endpoint.extensions list: the
// mount name/subdir, the OCI image reference, whether it needs a
// shared_preload_libraries entry, and — for a third-party image — where its
// tree keeps its pieces relative to the mount root. Every layout field is
// omitempty so an amp-built extension renders exactly the values it always did.
type extensionValues struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Preload        bool              `json:"preload"`
	Library        string            `json:"library,omitempty"`
	SharePath      string            `json:"sharePath,omitempty"`
	LibPath        string            `json:"libPath,omitempty"`
	LibraryPath    []string          `json:"libraryPath,omitempty"`
	EnvPaths       map[string]string `json:"envPaths,omitempty"`
	ABIExtraExempt []string          `json:"abiExtraExempt,omitempty"`
	Settings       map[string]string `json:"settings,omitempty"`
}

// resourceValues is the endpoint container's reservation. cpu and memory are
// Kubernetes resource quantities that the chart applies as BOTH requests and
// limits (Guaranteed QoS).
type resourceValues struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	// EphemeralStorage is the emptyDir /data reservation, set only for that
	// backing (a PVC is not ephemeral storage). It is what makes the scheduler
	// account the volume against the node's disk, so co-tenant endpoints cannot
	// oversubscribe it. Empty => the chart requests no ephemeral storage.
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

// values builds the per-endpoint Helm values. With spec.ControllerURL set the
// endpoint runs the controller-coordinated path; without it, it self-manages.
// Either way the pageserver reads its own branch row and how to create
// the branch from the controller-seeded branch_state row.
func (k *KubernetesDeployer) values(spec EndpointSpec, release string) chartValues {
	pgVersion := spec.PgVersion
	if pgVersion <= 0 {
		pgVersion = defaultPgVersion
	}
	// Reserve 2 cores / 4Gi unless the spec asks for something else. request ==
	// limit (rendered by the chart) is what gives the pod Guaranteed QoS.
	cpu := spec.CPU
	if cpu == "" {
		cpu = defaultEndpointCPU
	}
	memory := spec.Memory
	if memory == "" {
		memory = defaultEndpointMemory
	}
	// The one place a version becomes an image tag on this deployer. The fallback
	// is resolved here rather than left to the chart: image.tag has no omitempty
	// and the chart reads an empty tag as its own appVersion, so delegating it
	// would deploy the chart default instead of the configured tag.
	tag := spec.Version
	if tag == "" {
		tag = k.cfg.EndpointImageTag
	}
	// Only when this endpoint actually mounts documentdb — the chart's own
	// helper decides whether to render the sidecar from the same list, so
	// sending the image for an endpoint without it would be inert but
	// misleading. An empty gateway tag follows the endpoint's: CI builds the two
	// from one commit.
	var gw *documentdbGatewayValues
	if slices.ContainsFunc(spec.Extensions, func(e Extension) bool { return e.Name == "documentdb" }) {
		gwTag := k.cfg.DocumentDBGatewayImageTag
		if gwTag == "" {
			gwTag = tag
		}
		gw = &documentdbGatewayValues{Image: imageValues{
			Repository: k.cfg.DocumentDBGatewayImageRepo,
			Tag:        gwTag,
		}}
	}
	v := chartValues{
		FullnameOverride: release,
		Image: imageValues{
			Repository: k.cfg.EndpointImageRepo,
			Tag:        tag,
		},
		DocumentDBGateway: gw,
		Yugabyte: yugabyteValues{
			HostPorts: spec.YB.HostPorts,
			User:      spec.YB.User,
			Dbname:    spec.YB.DBName,
		},
		Endpoint: endpointValues{
			PgVersion:   strconv.Itoa(pgVersion),
			ComputePort: k.cfg.ComputePort,
			BranchId:    branchIDValue(spec.BranchID),
			EndpointId:  spec.EndpointID,
			Password:    spec.PasswordVerifier,
			Resources:   resourceValues{CPU: cpu, Memory: memory},
			Extensions:  extensionValuesFor(spec.Extensions),
		},
		Controller: controllerValues{URL: spec.ControllerURL},
		Service: serviceValues{
			Type:         k.cfg.ServiceType,
			Annotations:  k.cfg.ServiceAnnotations,
			Connectivity: spec.Connectivity,
			AllowedCidrs: spec.AllowedCIDRs,
			Ingress:      ingressValuesFor(spec),
			DNS:          dnsValuesFor(spec),
		},
		StartupProbe: startupProbeBudget(k.cfg.Timeout),
		// Headroom over the startup budget: the rollout must not be declared
		// failed while the kubelet is still legitimately waiting.
		ProgressDeadlineSeconds: progressDeadlineFor(k.cfg.Timeout),
	}
	// Prefer a pre-existing Secret (keeps the plaintext password off the
	// controller); fall back to the inline password for dev/test.
	if k.cfg.BackingYBExistingSecret != "" {
		v.Yugabyte.ExistingSecret = k.cfg.BackingYBExistingSecret
		v.Yugabyte.ExistingSecretKey = k.cfg.BackingYBExistingSecretKey
	} else {
		v.Yugabyte.Password = spec.YB.Password
	}
	// TLS: the chart renders a Certificate for this release and mounts its
	// Secret and the CA bundle at fixed paths, which the connection block names.
	// A password, if one is configured, still travels: whether the login is the
	// certificate or the password is the server's HBA rule to decide.
	if k.cfg.BackingYBTLSMode != "" && k.cfg.BackingYBTLSMode != "disable" {
		v.Yugabyte.Sslmode = k.cfg.BackingYBTLSMode
		v.Yugabyte.Sslrootcert = endpointTLSCAPath
		v.Yugabyte.Sslcert = endpointTLSCertPath
		v.Yugabyte.Sslkey = endpointTLSKeyPath
		v.TLS = &tlsValues{
			Enabled: true,
			IssuerRef: issuerRefValues{
				Kind:  k.cfg.BackingYBTLSIssuerKind,
				Name:  k.cfg.BackingYBTLSIssuerName,
				Group: k.cfg.BackingYBTLSIssuerGroup,
			},
			CABundleConfigMap: k.cfg.BackingYBTLSCABundleConfigMap,
			Certificate: certificateValue{
				Duration:    k.cfg.BackingYBTLSCertDuration,
				RenewBefore: k.cfg.BackingYBTLSCertRenewBefore,
			},
		}
	}

	if k.cfg.EndpointUserSplit {
		v.UserSplit = &userSplitValues{Enabled: true}
	}

	// Server TLS on the compute's listener. Set independently of the block
	// above: which certificate the pod PRESENTS to clients has nothing to do
	// with which one it presents to YugabyteDB, and in the Let's Encrypt case
	// they come from different issuers.
	if k.ServesComputeTLS() {
		v.ComputeTLS = &computeTlsValues{
			Enabled: true,
			Mode:    computeTLSChartMode(k.cfg.ComputeTLSMode),
			IssuerRef: issuerRefValues{
				Kind:  k.cfg.ComputeTLSIssuerKind,
				Name:  k.cfg.ComputeTLSIssuerName,
				Group: k.cfg.ComputeTLSIssuerGroup,
			},
			ExistingSecret:    k.cfg.ComputeTLSSecret,
			IncludeServiceDns: k.cfg.ComputeTLSIncludeServiceDNS,
			Certificate: certificateValue{
				Duration:    k.cfg.ComputeTLSCertDuration,
				RenewBefore: k.cfg.ComputeTLSCertRenewBefore,
			},
		}
		// An ingress_* endpoint in a deployment with NO DNS zone: the slot's
		// DNSName is unqualified (`ep-3f9a2c41`), which no issuer will sign and
		// no client can reach, so the address handed out is the shard's own.
		// That address is known before the deploy — the slot was allocated
		// first — so unlike the nlb_* case it needs no second pass.
		if spec.Ingress.DNSName == "" && spec.Ingress.ShardHost != "" {
			addComputeTLSAddress(v.ComputeTLS, spec.Ingress.ShardHost)
		}
	}
	if k.cfg.PullSecret != "" {
		v.ImagePullSecrets = []map[string]string{{"name": k.cfg.PullSecret}}
	}
	// A storage class turns the endpoint's data volume into a PVC of that class
	// (EBS/PD); without one the chart keeps its ephemeral emptyDir default.
	if k.cfg.StorageClass != "" {
		v.Persistence = &persistenceValues{StorageClass: k.cfg.StorageClass, Size: k.cfg.DiskSize}
	}
	// Pin endpoint pods to a labelled node pool (e.g. local-NVMe-SSD ephemeral),
	// so their emptyDir /data lands on NVMe. Omitted = schedules anywhere.
	if len(k.cfg.NodeSelector) > 0 {
		v.NodeSelector = k.cfg.NodeSelector
	}
	if k.cfg.LFCOffloadIntervalSeconds > 0 {
		v.LFC = &lfcValues{
			Autoprewarm:            true,
			OffloadIntervalSeconds: strconv.Itoa(k.cfg.LFCOffloadIntervalSeconds),
		}
	}
	// Identity last, though the API refuses its domain as a tag anyway.
	if len(spec.Labels)+len(spec.Identity) > 0 {
		v.CommonLabels = make(map[string]string, len(spec.Labels)+len(spec.Identity))
		maps.Copy(v.CommonLabels, spec.Labels)
		maps.Copy(v.CommonLabels, spec.Identity)
	}
	if len(spec.IdentityAnnotations) > 0 {
		v.PodAnnotations = maps.Clone(spec.IdentityAnnotations)
	}
	return v
}

// ingressValuesFor renders the shard slot for the chart, or nil for a mode that
// has none. The entryPoint name comes from EntryPointName, which a test holds
// equal to the shard chart's own spelling — see that function.
func ingressValuesFor(spec EndpointSpec) *ingressValues {
	if !IsIngress(spec.Connectivity) {
		return nil
	}
	// An unpublishable name is not passed to the chart at all, so no DNSEndpoint
	// is rendered for it — on a create or a redeploy alike. The name stays on the
	// allocation row either way.
	dnsName := spec.Ingress.DNSName
	if !PublishableDNSName(dnsName) {
		dnsName = ""
	}
	return &ingressValues{
		Shard:                spec.Ingress.Shard,
		EntryPoint:           EntryPointName(spec.Ingress.Port),
		DocumentDBEntryPoint: DocumentDBEntryPoint,
		Port:                 spec.Ingress.Port,
		DNSName:              dnsName,
		ShardHost:            spec.Ingress.ShardHost,
		RecordType:           spec.Ingress.RecordType,
	}
}

// extensionValuesFor maps the spec's extensions to the chart's endpoint.extensions
// list. Nil in => nil out, so the marshaled values omit the key entirely and the
// chart's no-extensions default applies.
func extensionValuesFor(exts []Extension) []extensionValues {
	if len(exts) == 0 {
		return nil
	}
	out := make([]extensionValues, len(exts))
	for i, e := range exts {
		out[i] = extensionValues{
			Name:           e.Name,
			Image:          e.Image,
			Preload:        e.Preload,
			Library:        e.Library,
			SharePath:      e.SharePath,
			LibPath:        e.LibPath,
			LibraryPath:    e.LibraryPath,
			EnvPaths:       e.EnvPaths,
			ABIExtraExempt: e.ABIExtraExempt,
			Settings:       e.Settings,
		}
	}
	return out
}
