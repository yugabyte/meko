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

	"github.com/yugabyte/yb-light/amp/internal/helm"
)

// defaultPgVersion backs an endpoint whose spec omits a version (the chart's
// own default is the same). yb-light is Postgres v17 only.
const defaultPgVersion = 17

// serviceTypeLoadBalancer is the endpoint Service type that gets an external IP;
// the deployer then reports that IP as the endpoint's client address.
const serviceTypeLoadBalancer = "LoadBalancer"

// lbAddressPollTimeout bounds how long CreateEndpoint waits for a LoadBalancer
// Service to receive its ingress IP before falling back to the Service DNS. A
// short, fixed cap (independent of the longer helm --wait deploy timeout) so a
// slow cloud LB doesn't keep the synchronous create blocked indefinitely.
const lbAddressPollTimeout = 2 * time.Minute

// KubernetesConfig is the static configuration of the Helm-backed deployer: the
// cluster/chart/image coordinates that are the same for every endpoint. The
// per-endpoint identity (tenant/timeline/pg version, the backing YB connection,
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
	ServiceType       string // endpoint Service type: "ClusterIP" (default) or "LoadBalancer"
	// ServiceAnnotations are set on the endpoint Service (e.g. GKE internal-LB annotations).
	ServiceAnnotations map[string]string

	// Optional: deliver the backing YugabyteDB password from a pre-existing
	// Secret in the namespace instead of inline (keeps the plaintext off the
	// controller). When ExistingSecret is empty, the spec's inline YB.Password
	// is used.
	BackingYBExistingSecret    string
	BackingYBExistingSecretKey string

	Timeout time.Duration // how long to wait for a deploy/retire to converge (helm --wait)
}

// KubernetesDeployer is the Helm-backed Deployer: one Helm release (one pod) per
// endpoint (yblight-controller.md §5.4, "one pod per endpoint"). It is the
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
	// Resolve the client address. For a LoadBalancer Service it is the external
	// LB IP (clients connect from outside the cluster); otherwise the pod IP (a
	// demo convenience). Both fall back to the stable Service DNS. NB: this is
	// only the *client* address — the controller reaches the endpoint's
	// pageserver/safekeeper by the stable Service DNS (SC_REACHABLE_HOST set in
	// the chart), so a pod reschedule re-registers the same address (no 409, #121).
	addr := k.address(release)
	switch k.cfg.ServiceType {
	case serviceTypeLoadBalancer:
		if lb := k.serviceLBAddress(ctx, release); lb != "" {
			addr = lb
		} else if podAddr := k.podClientAddress(ctx, release); podAddr != "" {
			addr = podAddr
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
}

type serviceValues struct {
	// Type is the endpoint Service type ("ClusterIP" default, "LoadBalancer").
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
	TenantId    string `json:"tenantId"`
	TimelineId  string `json:"timelineId"`
	Bootstrap   bool   `json:"bootstrap"`
}

// values builds the per-endpoint Helm values. With spec.StorageControllerURL set
// (and spec.Bootstrap=true), the endpoint registers its pageserver+safekeeper
// with the controller, attaches the tenant (generation via the attach-hook),
// runs initdb for the supplied tenant/timeline, and registers the timeline.
// Without it, the endpoint attaches an already-materialized tenant/timeline.
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
			PgVersion:   strconv.Itoa(pgVersion),
			ComputePort: k.cfg.ComputePort,
			TenantId:    spec.TenantID,
			TimelineId:  spec.TimelineID,
			Bootstrap:   spec.Bootstrap,
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
	return v
}
