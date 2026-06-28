package deploy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/controller/internal/helm"
)

// defaultPgVersion backs an endpoint whose spec omits a version (the chart's
// own default is the same). yb-light is Postgres v17 only.
const defaultPgVersion = 17

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

// KubernetesConfig is the static configuration of the Helm-backed deployer: the
// cluster/chart/image coordinates that are the same for every endpoint. The
// per-endpoint identity (branch id/pg version, the backing YB connection,
// the storage-controller URL, bootstrap) arrives in the EndpointSpec.
type KubernetesConfig struct {
	Namespace      string // namespace endpoints are deployed into
	ChartPath      string // path to the yb-light-endpoint Helm chart
	HelmBin        string // helm binary; bare name resolved via PATH, a path used verbatim
	KubeconfigPath string // KUBECONFIG for helm; empty = default discovery (in-cluster or ~/.kube)

	EndpointImageRepo string // endpoint container image repository
	EndpointImageTag  string // endpoint container image tag
	PullSecret        string // imagePullSecrets name in the namespace (empty = none)
	ComputePort       int    // compute Postgres port the endpoint Service publishes
	ServiceType       string // endpoint Service type: "ClusterIP" (default), "NodePort", or "LoadBalancer"
	// ServiceAnnotations are set on the endpoint Service (e.g. GKE internal-LB annotations).
	ServiceAnnotations map[string]string

	// StorageClass, when non-empty, provisions the endpoint's data volume as a
	// PersistentVolumeClaim of that class (a network block volume — EBS/PD) of
	// size DiskSize, instead of an ephemeral emptyDir. Empty = emptyDir.
	StorageClass string
	DiskSize     string

	// Optional: deliver the backing YugabyteDB password from a pre-existing
	// Secret in the namespace instead of inline (keeps the plaintext off the
	// controller). When ExistingSecret is empty, the spec's inline YB.Password
	// is used.
	BackingYBExistingSecret    string
	BackingYBExistingSecretKey string

	Timeout time.Duration // how long to wait for a deploy/retire to converge (helm --wait)
}

// KubernetesDeployer is the Helm-backed Deployer: one Helm release (one pod) per
// endpoint (amp-controller.md §5.4, "one pod per endpoint"). It is the
// Kubernetes sibling of [DockerDeployer], implementing the same [Deployer]
// interface.
type KubernetesDeployer struct {
	helm   *helm.Client
	cfg    KubernetesConfig
	logger *slog.Logger
}

// NewKubernetesDeployer builds the deployer from its static config.
func NewKubernetesDeployer(cfg KubernetesConfig, logger *slog.Logger) *KubernetesDeployer {
	return &KubernetesDeployer{
		helm:   helm.New(cfg.HelmBin, cfg.KubeconfigPath, logger),
		cfg:    cfg,
		logger: logger,
	}
}

var _ Deployer = (*KubernetesDeployer)(nil)

// CreateEndpoint installs/updates the endpoint's Helm release and waits for it
// to be ready, then returns the stable in-cluster client address. The release
// name (= spec.Name, a DNS-1123 label) is also the object/Service name via
// fullnameOverride, so the address is a pure function of the name and config.
func (k *KubernetesDeployer) CreateEndpoint(ctx context.Context, spec EndpointSpec) (Endpoint, error) {
	release := spec.Name
	if err := k.helm.UpgradeInstall(ctx, helm.Release{
		Name:            release,
		Chart:           k.cfg.ChartPath,
		Namespace:       k.cfg.Namespace,
		Values:          k.values(spec, release),
		CreateNamespace: true,
		Wait:            true,
		Timeout:         k.cfg.Timeout,
	}); err != nil {
		return Endpoint{}, fmt.Errorf("helm deploy %s: %w", release, err)
	}
	// Resolve the client address by Service type: a LoadBalancer's external LB IP,
	// a NodePort's "<nodeIP>:<nodePort>", or (ClusterIP) the pod IP — a demo
	// convenience. All fall back to the stable Service DNS. NB: this is only the
	// *client* address — the controller reaches the endpoint's pageserver/
	// safekeeper by the stable Service DNS (SC_REACHABLE_HOST set in the chart),
	// so a pod reschedule re-registers the same address (no 409, #121).
	addr := k.address(release)
	switch k.cfg.ServiceType {
	case serviceTypeLoadBalancer:
		if lb := k.serviceLBAddress(ctx, release); lb != "" {
			addr = lb
		} else if podAddr := k.podClientAddress(ctx, release); podAddr != "" {
			addr = podAddr
		}
	case serviceTypeNodePort:
		if np := k.nodePortClientAddress(ctx, release); np != "" {
			addr = np
		}
	default:
		if podAddr := k.podClientAddress(ctx, release); podAddr != "" {
			addr = podAddr
		}
	}
	k.logger.Info("endpoint ready", "endpoint", release, "address", addr)
	return Endpoint{ID: release, Address: addr}, nil
}

// k8sAPIGet does an authenticated GET against the in-cluster Kubernetes API and
// returns the response body, or (nil, false) when not running in-cluster or on
// any error (callers fall back to a static address).
func (k *KubernetesDeployer) k8sAPIGet(ctx context.Context, path string) ([]byte, bool) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, false // not in-cluster
	}
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, false
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s:%s%s", host, port, path), nil)
	if err != nil {
		return nil, false
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
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return body, true
}

// podClientAddress returns "<podIP>:<computePort>" for the endpoint's pod, or ""
// when not in-cluster / on error. Best-effort: the pod IP is not stable across
// pod restarts (the Service DNS is).
func (k *KubernetesDeployer) podClientAddress(ctx context.Context, release string) string {
	q := url.Values{"labelSelector": {"app.kubernetes.io/instance=" + release}, "limit": {"1"}}
	body, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", k.cfg.Namespace, q.Encode()))
	if !ok {
		return ""
	}
	var out struct {
		Items []struct {
			Status struct {
				PodIP string `json:"podIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Items) == 0 || out.Items[0].Status.PodIP == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", out.Items[0].Status.PodIP, k.cfg.ComputePort)
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
		case <-time.After(3 * time.Second):
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

// address is the in-cluster DNS name + port of an endpoint's compute Service.
// The Service name equals the release's fullnameOverride (= the release name).
func (k *KubernetesDeployer) address(release string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", release, k.cfg.Namespace, k.cfg.ComputePort)
}

// EndpointSafekeeperURL: the endpoint's safekeeper HTTP API is published on its
// Service (ports.safekeeperHttp), reachable at the same stable Service DNS the
// storage controller dials (SC_REACHABLE_HOST). id is the release name.
func (k *KubernetesDeployer) EndpointSafekeeperURL(id string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", id, k.cfg.Namespace, safekeeperHTTPPort)
}

// chartValues mirrors the subset of deploy/helm/yb-light-endpoint values.yaml
// the controller drives. JSON tags match the chart keys; helm reads the
// marshaled document as a values file.
type chartValues struct {
	FullnameOverride  string                  `json:"fullnameOverride"`
	Image             imageValues             `json:"image"`
	ImagePullSecrets  []map[string]string     `json:"imagePullSecrets,omitempty"`
	Yugabyte          yugabyteValues          `json:"yugabyte"`
	Endpoint          endpointValues          `json:"endpoint"`
	StorageController storageControllerValues `json:"storageController"`
	Service           serviceValues           `json:"service"`
	// Persistence is nil (omitted) for the chart's emptyDir default; set to
	// provision the endpoint's data volume as a PVC of a storage class.
	Persistence *persistenceValues `json:"persistence,omitempty"`
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
}

type storageControllerValues struct {
	// URL the endpoint uses to reach the storage controller; empty = SC-free.
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
}

type endpointValues struct {
	PgVersion   string `json:"pgVersion"`
	ComputePort int    `json:"computePort"`
	BranchId    string `json:"branchId"`
	Bootstrap   bool   `json:"bootstrap"`
	// Branch ancestry: empty for a root branch. When set, the endpoint forks
	// the ancestor at AncestorLsn instead of running initdb.
	AncestorBranchId string `json:"ancestorBranchId,omitempty"`
	AncestorLsn      string `json:"ancestorLsn,omitempty"`
}

// values builds the per-endpoint Helm values. With spec.StorageControllerURL set
// (and spec.Bootstrap=true), the endpoint registers its pageserver+safekeeper
// with the controller, attaches the branch (generation via the attach-hook),
// runs initdb for the supplied branch, and registers it.
// Without it, the endpoint attaches an already-materialized branch.
func (k *KubernetesDeployer) values(spec EndpointSpec, release string) chartValues {
	pgVersion := spec.PgVersion
	if pgVersion <= 0 {
		pgVersion = defaultPgVersion
	}
	v := chartValues{
		FullnameOverride: release,
		Image: imageValues{
			Repository: k.cfg.EndpointImageRepo,
			Tag:        k.cfg.EndpointImageTag,
		},
		Yugabyte: yugabyteValues{
			HostPorts: spec.YB.HostPorts,
			User:      spec.YB.User,
			Dbname:    spec.YB.DBName,
		},
		Endpoint: endpointValues{
			PgVersion:        strconv.Itoa(pgVersion),
			ComputePort:      k.cfg.ComputePort,
			BranchId:         spec.BranchID,
			Bootstrap:        spec.Bootstrap,
			AncestorBranchId: spec.AncestorBranchID,
			AncestorLsn:      spec.AncestorLSN,
		},
		StorageController: storageControllerValues{URL: spec.StorageControllerURL},
		Service:           serviceValues{Type: k.cfg.ServiceType, Annotations: k.cfg.ServiceAnnotations},
	}
	// Prefer a pre-existing Secret (keeps the plaintext password off the
	// controller); fall back to the inline password for dev/test.
	if k.cfg.BackingYBExistingSecret != "" {
		v.Yugabyte.ExistingSecret = k.cfg.BackingYBExistingSecret
		v.Yugabyte.ExistingSecretKey = k.cfg.BackingYBExistingSecretKey
	} else {
		v.Yugabyte.Password = spec.YB.Password
	}
	if k.cfg.PullSecret != "" {
		v.ImagePullSecrets = []map[string]string{{"name": k.cfg.PullSecret}}
	}
	// A storage class turns the endpoint's data volume into a PVC of that class
	// (EBS/PD); without one the chart keeps its ephemeral emptyDir default.
	if k.cfg.StorageClass != "" {
		v.Persistence = &persistenceValues{StorageClass: k.cfg.StorageClass, Size: k.cfg.DiskSize}
	}
	return v
}
