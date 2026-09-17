// Per-endpoint connectivity (meko#967): an endpoint created with an nlb_* mode
// gets a second, Postgres-only Service behind the cloud's L4 load balancer, and
// its recorded address is the load balancer's rather than the in-cluster one.
// nlb_external
// is exercised only behind an allow-list that admits nobody: it puts Postgres on
// the internet behind a password.
package features

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// lbReadyTimeout bounds the wait for the cloud to hand the Service an
	// ingress address. The controller itself gives up after two minutes and
	// records the Service-type address instead, which the assertion below catches.
	lbReadyTimeout = 8 * time.Minute
	// lbGoneTimeout bounds the wait for the Service to disappear after the
	// endpoint delete: the load balancer is a billed cloud resource.
	lbGoneTimeout = 3 * time.Minute
	// lbConnectTimeout bounds the best-effort connect through the load balancer
	// from the coordinator, which may not sit in the cluster's network.
	lbConnectTimeout = 45 * time.Second
	// lbNoCloudRender is the endpoint chart's refusal to render an internal load
	// balancer for a cloud it cannot name.
	lbNoCloudRender = "needs service.cloud"
)

var errConnectivityUnsupported = errors.New("this controller drops an explicit connectivity block from the endpoint it creates, so it predates configurable connectivity")

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_connectivity",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Endpoint connectivity: a malformed connectivity block (unknown mode, a bare IP in allowed_cidrs, cidrs on cluster_ip, connectivity on a cluster create without with_writer) is refused with 400; a cluster_ip endpoint gets no load-balancer Service; an nlb_internal endpoint gets a Postgres-only LoadBalancer Service carrying the cloud's internal annotation, its recorded address is the load balancer's, and the Service is gone once the endpoint is deleted. On a cluster whose cloud the controller cannot name the create must fail rather than publish an internet-facing load balancer. A cluster create that does carry with_writer must hand that writer the load balancer it asked for. An nlb_external endpoint with allowed_cidrs gets an internet-facing Service whose loadBalancerSourceRanges is exactly that list, no internal annotation, only the Postgres port, and the Service is gone once the endpoint is deleted.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// Two load balancers, each waited out to a cloud-assigned ingress.
		Timeout: 45 * time.Minute,
		Run:     runAmpEndpointConnectivity,
	})
}

func runAmpEndpointConnectivity(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"connectivity_validation", a.connectivityValidation},
			{"nlb_internal_endpoint", a.nlbInternalEndpoint},
			{"nlb_external_allowed_cidrs", a.nlbExternalAllowedCIDRs},
			{"with_writer_connectivity", a.withWriterConnectivity},
		}
	})
}

// connectivityCtl is the slice of the controller client the validation steps
// drive: the raw creates, whose 400 arrives before any task exists.
type connectivityCtl interface {
	CreateCluster(context.Context, string, controller.CreateClusterRequest) (controller.Task, string, error)
	CreateEndpoint(context.Context, string, string, string, controller.CreateEndpointRequest) (controller.Task, string, error)
}

func (a *ampFeatureRun) connectivityValidation(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	// Explicit, because a supporting controller reports null for a default create.
	conn := controller.EndpointConnectivity{Mode: controller.ConnectivityClusterIP}
	ep, err := createEndpointReq(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, controller.CreateEndpointRequest{
		Role:         roleReadWrite,
		Connectivity: &conn,
	})
	if err != nil {
		return fmt.Errorf("create the cluster_ip endpoint: %w", err)
	}
	if ep.Connectivity == nil {
		a.f.t.Skip(errConnectivityUnsupported.Error())
		return nil
	}
	if ep.Connectivity.Mode != controller.ConnectivityClusterIP {
		return fmt.Errorf("an endpoint created with connectivity.mode=%s reports mode %q", controller.ConnectivityClusterIP, ep.Connectivity.Mode)
	}
	if kube := a.f.ampKube(); kube != nil {
		if _, err := lbServiceIngress(ctx, kube, ep.ID); !isNotFound(err) {
			return fmt.Errorf("a cluster_ip endpoint has a load-balancer Service %s (lookup error: %v), want none", lbServiceName(ep.ID), err)
		}
	}
	// Released here, not at teardown: a branch takes one read_write endpoint
	// (endpoints_one_writer_per_branch) and the nlb_internal create wants it.
	if _, err := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("delete the cluster_ip endpoint: %w", err)
	}
	return connectivityValidationSteps(ctx, cl, a.proj, "conn-"+a.f.suffix(), a.f.cfg.PgVersion)
}

// connectivityValidationSteps sends the four malformed requests and demands a
// 400 for each. Nothing here waits on a task: an accepted request is the failure.
func connectivityValidationSteps(ctx context.Context, cl connectivityCtl, proj ampCluster, clusterName string, pgVersion int) error {
	epCases := []struct {
		what string
		conn controller.EndpointConnectivity
	}{
		{"an unknown mode", controller.EndpointConnectivity{Mode: "nlb_sideways"}},
		{"a bare IP in allowed_cidrs", controller.EndpointConnectivity{Mode: controller.ConnectivityNLBInternal, AllowedCIDRs: []string{"10.0.0.1"}}},
		{"allowed_cidrs on cluster_ip", controller.EndpointConnectivity{Mode: controller.ConnectivityClusterIP, AllowedCIDRs: []string{"10.0.0.0/8"}}},
	}
	for _, tc := range epCases {
		conn := tc.conn
		_, _, err := cl.CreateEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite, Connectivity: &conn})
		if err == nil {
			return fmt.Errorf("an endpoint create with %s was accepted, want 400", tc.what)
		}
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			return fmt.Errorf("an endpoint create with %s failed with HTTP %d, want 400: %w", tc.what, status, err)
		}
	}
	_, _, err := cl.CreateCluster(ctx, proj.poolID, controller.CreateClusterRequest{
		Name: clusterName, PgVersion: pgVersion,
		Connectivity: &controller.EndpointConnectivity{Mode: controller.ConnectivityNLBInternal},
	})
	if err == nil {
		return fmt.Errorf("a cluster create carrying connectivity without with_writer was accepted, want 400 (it asks for an exposure nothing would get)")
	}
	if status := apiErrorStatus(err); status != http.StatusBadRequest {
		return fmt.Errorf("a cluster create carrying connectivity without with_writer failed with HTTP %d, want 400: %w", status, err)
	}
	return nil
}

func (a *ampFeatureRun) nlbInternalEndpoint(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so the load-balancer Service cannot be inspected")
		return nil
	}
	cloud, err := clusterCloud(ctx, kube)
	if err != nil {
		a.f.t.Skip("cannot read a node's providerID to name the cloud, so an nlb_internal outcome cannot be judged: " + err.Error())
		return nil
	}
	a.f.logf("cluster cloud from spec.providerID: %q", cloud)

	req := controller.CreateEndpointRequest{Role: roleReadWrite, Connectivity: &controller.EndpointConnectivity{Mode: controller.ConnectivityNLBInternal}}
	_, err = cl.CreateEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, req)
	if cloud == "" {
		if err == nil {
			return fmt.Errorf("an nlb_internal create succeeded on a cluster whose cloud the controller cannot name; every provider defaults a LoadBalancer Service to internet-facing, so this may have published a private endpoint")
		}
		if !strings.Contains(err.Error(), lbNoCloudRender) {
			return fmt.Errorf("an nlb_internal create on a cluster with no nameable cloud failed, but not with the chart's %q refusal: %w", lbNoCloudRender, err)
		}
		a.f.logf("no nameable cloud: the create was refused as designed: %v", err)
		return nil
	}
	if err != nil {
		if strings.Contains(err.Error(), lbNoCloudRender) {
			return fmt.Errorf("the controller could not name the cloud on a %s cluster: rbac.nodeRead is off or the node read failed (see \"nlb_internal create fails\" in amp/runbooks/controller_runbook.md): %w", cloud, err)
		}
		return fmt.Errorf("create the nlb_internal endpoint: %w", err)
	}
	ep, err := endpointWithMode(ctx, cl, a.proj, controller.ConnectivityNLBInternal)
	if err != nil {
		return err
	}
	a.f.register("delete nlb_internal endpoint", func(ctx context.Context) error {
		_, e := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID)
		if e != nil && strings.Contains(e.Error(), "not found") {
			return nil
		}
		return e
	})

	lbHost, err := waitLBIngress(ctx, kube, ep.ID, lbReadyTimeout)
	if err != nil {
		return err
	}
	svc, err := readLBService(ctx, kube, ep.ID)
	if err != nil {
		return err
	}
	if err := lbServiceVerdict(svc, cloud, addrPortOf(ep.Address)); err != nil {
		return fmt.Errorf("load-balancer Service %s: %w", lbServiceName(ep.ID), err)
	}
	if err := lbAddressVerdict(ep.Address, lbHost); err != nil {
		return err
	}

	// Best effort: the coordinator may not sit in the cluster's network, and an
	// internal load balancer is not reachable from outside it.
	if err := a.connectThrough(ctx, ep); err != nil {
		a.f.logf("could not connect through the internal load balancer from the coordinator (expected outside the VPC): %v", err)
	} else {
		a.f.logf("connected through the internal load balancer at %s", ep.Address)
	}

	if _, err := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("delete the nlb_internal endpoint: %w", err)
	}
	return waitLBGone(ctx, kube, ep.ID, lbGoneTimeout)
}

// withWriterConnectivity is the accepted half of the cluster-create block: the
// writer the create deploys must come out on the load balancer it asked for.
func (a *ampFeatureRun) withWriterConnectivity(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so the writer's load-balancer Service cannot be inspected")
		return nil
	}
	cloud, err := clusterCloud(ctx, kube)
	if err != nil || cloud == "" {
		a.f.t.Skip("this cluster's cloud cannot be named from a node's providerID, so an nlb_internal writer cannot be asked for: " + fmt.Sprint(err))
		return nil
	}

	name := "connw-" + a.f.suffix()
	if _, err := cl.CreateClusterAndWait(ctx, a.proj.poolID, controller.CreateClusterRequest{
		Name:         name,
		PgVersion:    a.f.cfg.PgVersion,
		WithWriter:   true,
		Connectivity: &controller.EndpointConnectivity{Mode: controller.ConnectivityNLBInternal},
	}); err != nil {
		return fmt.Errorf("create a cluster with a writer on nlb_internal: %w", err)
	}
	proj, err := findClusterCtl(ctx, cl, a.proj.poolID, name)
	if err != nil {
		return err
	}
	a.f.register("delete with_writer connectivity cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, a.proj.poolID, proj.ID)
		return e
	})

	branch := ampCluster{poolID: a.proj.poolID, projID: proj.ID, branchID: proj.DefaultBranch.BranchID}
	ep, err := endpointWithMode(ctx, cl, branch, controller.ConnectivityNLBInternal)
	if err != nil {
		return fmt.Errorf("the cluster create carrying connectivity was accepted with a writer, but that writer does not record the mode it asked for: %w", err)
	}
	lbHost, err := waitLBIngress(ctx, kube, ep.ID, lbReadyTimeout)
	if err != nil {
		return err
	}
	svc, err := readLBService(ctx, kube, ep.ID)
	if err != nil {
		return err
	}
	if err := lbServiceVerdict(svc, cloud, addrPortOf(ep.Address)); err != nil {
		return fmt.Errorf("the writer's load-balancer Service %s: %w", lbServiceName(ep.ID), err)
	}
	return lbAddressVerdict(ep.Address, lbHost)
}

func (a *ampFeatureRun) connectThrough(ctx context.Context, ep controller.Endpoint) error {
	ci, err := a.f.endpointConnInfo(ep)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, lbConnectTimeout)
	defer cancel()
	c, err := pg.Connect(cctx, ci)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.QueryInt(cctx, "SELECT 1")
	return err
}

// endpointWithMode finds the branch's endpoint carrying mode, the one the create
// just produced.
func endpointWithMode(ctx context.Context, cl ampCtl, proj ampCluster, mode string) (controller.Endpoint, error) {
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("list endpoints: %w", err)
	}
	for i := len(eps) - 1; i >= 0; i-- {
		if eps[i].Connectivity != nil && eps[i].Connectivity.Mode == mode {
			return eps[i], nil
		}
	}
	return controller.Endpoint{}, fmt.Errorf("the %s create succeeded but no endpoint on branch %s reports that mode", mode, proj.branchID)
}

// lbServiceName is the endpoint chart's second, Postgres-only Service.
func lbServiceName(endpointID string) string {
	return ampstack.EndpointWorkloadName(endpointID) + "-lb"
}

// lbService is what the verdict reads off the Service: its type, the ports it
// publishes and the annotations that make it internal.
type lbService struct {
	Type        string
	Ports       []string
	Annotations string
}

func readLBService(ctx context.Context, kube *k8s.Kubectl, endpointID string) (lbService, error) {
	out, err := kube.Get(ctx, "service", lbServiceName(endpointID), `{.spec.type}{"|"}{.spec.ports[*].port}{"|"}{.metadata.annotations}`)
	if err != nil {
		return lbService{}, fmt.Errorf("read Service %s: %w", lbServiceName(endpointID), err)
	}
	return parseLBService(out), nil
}

func parseLBService(out string) lbService {
	parts := strings.SplitN(out, "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return lbService{Type: parts[0], Ports: strings.Fields(parts[1]), Annotations: parts[2]}
}

// internalLBAnnotation is what marks a LoadBalancer Service internal on each
// cloud, as the endpoint chart renders it.
var internalLBAnnotation = map[string]lbAnnotation{
	"aws":   {"service.beta.kubernetes.io/aws-load-balancer-scheme", "internal"},
	"gcp":   {"networking.gke.io/load-balancer-type", "Internal"},
	"azure": {"service.beta.kubernetes.io/azure-load-balancer-internal", "true"},
}

// lbAnnotation is a key and a value, not "key:value": kubectl prints JSON.
type lbAnnotation struct{ key, value string }

// json renders the pair as it appears in that object.
func (a lbAnnotation) json() string { return `"` + a.key + `":"` + a.value + `"` }

// lbServiceVerdict checks the Service is a LoadBalancer publishing exactly the
// Postgres port and carrying the cloud's internal annotation. The single port is
// the security invariant: the supervisor's settings API is unauthenticated and
// can restart Postgres, so it must never sit behind the load balancer.
func lbServiceVerdict(svc lbService, cloud string, pgPort string) error {
	if svc.Type != "LoadBalancer" {
		return fmt.Errorf("type is %q, want LoadBalancer", svc.Type)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != pgPort {
		return fmt.Errorf("publishes ports %v, want exactly the Postgres port %s (the pageserver, safekeeper and supervisor ports must stay off the load balancer)", svc.Ports, pgPort)
	}
	want, ok := internalLBAnnotation[cloud]
	if !ok {
		return fmt.Errorf("no internal load-balancer annotation is known for cloud %q", cloud)
	}
	if !strings.Contains(svc.Annotations, want.json()) {
		return fmt.Errorf("lacks the %s internal annotation %s=%s; the load balancer would be internet-facing. annotations: %s", cloud, want.key, want.value, svc.Annotations)
	}
	return nil
}

// lbAddressVerdict checks the endpoint's recorded address is the load
// balancer's. A pod IP there means the create outran the controller's
// two-minute ingress poll and fell back, and nothing re-polls afterwards.
func lbAddressVerdict(address, lbHost string) error {
	host := address
	if h, _, err := net.SplitHostPort(strings.TrimSpace(address)); err == nil {
		host = h
	}
	if host == lbHost {
		return nil
	}
	return fmt.Errorf("the endpoint's recorded address is %q but its load balancer is at %q: the create fell back to the Service-type address after lbAddressPollTimeout and nothing re-polls, so nothing outside the cluster can reach it (\"Endpoint stuck on the in-cluster address after an nlb_* create\" in amp/runbooks/controller_runbook.md)", address, lbHost)
}

// clusterCloud names the cloud from a node's spec.providerID the way the
// controller does, or "" when the prefix is not one it knows (kind, bare metal).
func clusterCloud(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	id, err := kube.InNamespace("").GetSelector(ctx, "nodes", "kubernetes.io/os", "{.items[0].spec.providerID}")
	if err != nil {
		return "", err
	}
	return cloudFromProviderID(id), nil
}

func cloudFromProviderID(id string) string {
	switch {
	case strings.HasPrefix(id, "aws://"):
		return "aws"
	case strings.HasPrefix(id, "gce://"):
		return "gcp"
	case strings.HasPrefix(id, "azure://"):
		return "azure"
	}
	return ""
}

func lbServiceIngress(ctx context.Context, kube *k8s.Kubectl, endpointID string) (string, error) {
	return kube.Get(ctx, "service", lbServiceName(endpointID), `{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}`)
}

func waitLBIngress(ctx context.Context, kube *k8s.Kubectl, endpointID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		host, err := lbServiceIngress(ctx, kube, endpointID)
		if err != nil && !isNotFound(err) {
			return "", fmt.Errorf("read Service %s: %w", lbServiceName(endpointID), err)
		}
		if err == nil && host != "" {
			return host, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("the nlb_internal endpoint has no Service %s after %s: the chart did not render the load balancer", lbServiceName(endpointID), timeout)
			}
			return "", fmt.Errorf("Service %s has no load-balancer ingress after %s; describe it and read its events (a missing subnet tag or an exhausted load-balancer quota stops it here)", lbServiceName(endpointID), timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func waitLBGone(ctx context.Context, kube *k8s.Kubectl, endpointID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := lbServiceIngress(ctx, kube, endpointID)
		if isNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Service %s still exists %s after its endpoint was deleted; its load balancer is a billed cloud resource (lookup: %v)", lbServiceName(endpointID), timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NotFound")
}

// addrPortOf returns the port of a "host:port" address, or the chart's compute
// port when the address carries none.
func addrPortOf(address string) string {
	if _, p, err := net.SplitHostPort(strings.TrimSpace(address)); err == nil {
		return p
	}
	return "55433"
}

// nlbExternalNoAdmit is a /32 nobody holds, so the internet-facing load balancer
// this phase creates admits no client while it exists.
const nlbExternalNoAdmit = "192.0.2.1/32"

// nlbExternalAllowedCIDRs is the internet-facing mode with its source
// allow-list: the Service must carry the CIDRs as loadBalancerSourceRanges and
// no internal annotation, publish only the Postgres port, and go away with the
// endpoint. The allow-list is the only thing between an nlb_external endpoint
// and the internet, and nothing had ever read it back off a real Service.
func (a *ampFeatureRun) nlbExternalAllowedCIDRs(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.t.Skip("the control plane is reached by a direct URL with no kube access, so the load-balancer Service cannot be inspected")
		return nil
	}
	cloud, err := clusterCloud(ctx, kube)
	if err != nil || cloud == "" {
		a.f.t.Skip("this cluster's cloud cannot be named from a node's providerID, so an nlb_external outcome cannot be judged: " + fmt.Sprint(err))
		return nil
	}
	req := controller.CreateEndpointRequest{Role: roleReadWrite, Connectivity: &controller.EndpointConnectivity{
		Mode: controller.ConnectivityNLBExternal, AllowedCIDRs: []string{nlbExternalNoAdmit},
	}}
	if _, err := cl.CreateEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, req); err != nil {
		return fmt.Errorf("create the nlb_external endpoint: %w", err)
	}
	ep, err := endpointWithMode(ctx, cl, a.proj, controller.ConnectivityNLBExternal)
	if err != nil {
		return err
	}
	a.f.register("delete nlb_external endpoint", func(ctx context.Context) error {
		_, e := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID)
		if e != nil && strings.Contains(e.Error(), "not found") {
			return nil
		}
		return e
	})
	if ep.Connectivity == nil || len(ep.Connectivity.AllowedCIDRs) != 1 || ep.Connectivity.AllowedCIDRs[0] != nlbExternalNoAdmit {
		return fmt.Errorf("the endpoint records connectivity %+v, want nlb_external with allowed_cidrs [%s]", ep.Connectivity, nlbExternalNoAdmit)
	}
	lbHost, err := waitLBIngress(ctx, kube, ep.ID, lbReadyTimeout)
	if err != nil {
		return err
	}
	svc, err := readLBService(ctx, kube, ep.ID)
	if err != nil {
		return err
	}
	ranges, err := kube.Get(ctx, "service", lbServiceName(ep.ID), `{.spec.loadBalancerSourceRanges[*]}`)
	if err != nil {
		return fmt.Errorf("read loadBalancerSourceRanges of %s: %w", lbServiceName(ep.ID), err)
	}
	if err := externalLBVerdict(svc, ranges, cloud, addrPortOf(ep.Address), nlbExternalNoAdmit); err != nil {
		return fmt.Errorf("load-balancer Service %s: %w", lbServiceName(ep.ID), err)
	}
	if err := lbAddressVerdict(ep.Address, lbHost); err != nil {
		return err
	}
	if _, err := cl.DeleteEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("delete the nlb_external endpoint: %w", err)
	}
	return waitLBGone(ctx, kube, ep.ID, lbGoneTimeout)
}

// externalLBVerdict: a LoadBalancer on exactly the Postgres port, the caller's
// allow-list applied verbatim, and none of the internal annotations (an
// internal annotation on an nlb_external Service would make it unreachable from
// where the user asked to reach it).
func externalLBVerdict(svc lbService, sourceRanges, cloud, pgPort, wantCIDR string) error {
	if svc.Type != "LoadBalancer" {
		return fmt.Errorf("type is %q, want LoadBalancer", svc.Type)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != pgPort {
		return fmt.Errorf("publishes ports %v, want exactly the Postgres port %s", svc.Ports, pgPort)
	}
	got := strings.Fields(sourceRanges)
	if len(got) != 1 || got[0] != wantCIDR {
		return fmt.Errorf("loadBalancerSourceRanges is %v, want [%s]: allowed_cidrs is the only thing between an internet-facing Postgres and the internet, and it did not reach the Service", got, wantCIDR)
	}
	if ann, ok := internalLBAnnotation[cloud]; ok && strings.Contains(svc.Annotations, ann.json()) {
		return fmt.Errorf("carries the %s internal annotation %s=%s on an nlb_external endpoint", cloud, ann.key, ann.value)
	}
	return nil
}
