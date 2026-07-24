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

	"github.com/yugabyte/amp/controller/internal/helm"
)

// defaultPgVersion backs an endpoint whose spec omits a version (the chart's
// own default is the same). yb-amp is Postgres v18 only.
const defaultPgVersion = 18

// defaultEndpointCPU / defaultEndpointMemory back an endpoint whose spec omits a
// resource reservation. They mirror the chart's own values.yaml default so a
// controller-driven deploy and a standalone chart install reserve the same
// compute (2 cores / 4Gi) — applied as request == limit for Guaranteed QoS.
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
	PullSecret        string // imagePullSecrets name in the namespace (empty = none)
	ComputePort       int    // compute Postgres port the endpoint Service publishes
	ServiceType       string // endpoint Service type: "ClusterIP" (default), "NodePort", or "LoadBalancer"
	// ServiceAnnotations are set on the endpoint Service (e.g. GKE internal-LB annotations).
	ServiceAnnotations map[string]string
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
	// The deploy namespace is the controller's own namespace (the chart sets
	// AMP_DEPLOY_NAMESPACE to .Release.Namespace), so it always exists — no
	// --create-namespace, which would otherwise demand cluster-scoped
	// namespaces:create the controller need not hold on a shared cluster.
	if err := k.helm.UpgradeInstall(ctx, helm.Release{
		Name:      release,
		Chart:     k.cfg.ChartPath,
		Namespace: k.cfg.Namespace,
		Values:    k.values(spec, release),
		Wait:      true,
		Timeout:   k.cfg.Timeout,
	}); err != nil {
		return Endpoint{}, fmt.Errorf("helm deploy %s: %w", release, err)
	}
	// Resolve the client address by Service type: a LoadBalancer's external LB IP,
	// a NodePort's "<nodeIP>:<nodePort>", or (ClusterIP) the pod IP — a demo
	// convenience. All fall back to the stable Service DNS. NB: this is only the
	// *client* address — the controller reaches the endpoint's pageserver/
	// safekeeper by the stable Service DNS (CONTROLLER_REACHABLE_HOST set in the chart),
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
	tailnet := k.tailnetAddress(release)
	k.logger.Info("endpoint ready", "endpoint", release, "address", addr, "tailnet_address", tailnet)
	return Endpoint{ID: release, Address: addr, TailnetAddress: tailnet}, nil
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
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
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

// podName returns the name of the endpoint's (single) pod, found by the same
// instance label podClientAddress uses.
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

// chartValues mirrors the subset of deploy/helm/yb-amp-endpoint values.yaml
// the controller drives. JSON tags match the chart keys; helm reads the
// marshaled document as a values file.
type chartValues struct {
	FullnameOverride string              `json:"fullnameOverride"`
	Image            imageValues         `json:"image"`
	ImagePullSecrets []map[string]string `json:"imagePullSecrets,omitempty"`
	Yugabyte         yugabyteValues      `json:"yugabyte"`
	Endpoint         endpointValues      `json:"endpoint"`
	Controller       controllerValues    `json:"controller"`
	Service          serviceValues       `json:"service"`
	// Persistence is nil (omitted) for the chart's emptyDir default; set to
	// provision the endpoint's data volume as a PVC of a storage class.
	Persistence *persistenceValues `json:"persistence,omitempty"`
	// NodeSelector pins the endpoint pod to nodes with these labels (e.g. a
	// local-NVMe-SSD ephemeral pool); omitted leaves the chart's default (no
	// selector — schedules anywhere).
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
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
	// Resources is the pod's CPU + memory reservation. The chart renders each
	// value as both a request and a limit, so the pod gets Guaranteed QoS.
	Resources resourceValues `json:"resources"`
}

// resourceValues is the endpoint container's compute reservation. cpu and memory
// are Kubernetes resource quantities that the chart applies as BOTH requests and
// limits (Guaranteed QoS).
type resourceValues struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// values builds the per-endpoint Helm values. With spec.ControllerURL set
// (and spec.Bootstrap=true), the endpoint registers its pageserver+safekeeper
// with the controller, attaches the branch (generation via the attach-hook),
// runs initdb for the supplied branch, and registers it.
// Without it, the endpoint attaches an already-materialized branch.
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
			Resources:        resourceValues{CPU: cpu, Memory: memory},
		},
		Controller: controllerValues{URL: spec.ControllerURL},
		Service:    serviceValues{Type: k.cfg.ServiceType, Annotations: k.cfg.ServiceAnnotations},
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
	// Pin endpoint pods to a labelled node pool (e.g. local-NVMe-SSD ephemeral),
	// so their emptyDir /data lands on NVMe. Omitted = schedules anywhere.
	if len(k.cfg.NodeSelector) > 0 {
		v.NodeSelector = k.cfg.NodeSelector
	}
	return v
}
