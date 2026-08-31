package perf

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The heavy perf workloads measure AMP, not YugabyteDB: after the co-located
// YB-in-k8s backing is up, this stands an amp control plane and one compute
// endpoint on top of it, and the benchmark drives that endpoint's compute
// Postgres. The full amp path (compute → pageserver → YB) is therefore in every
// request, which it never was while the drivers dialled the tservers directly.

const (
	// The endpoint's compute Postgres role/database, as the endpoint image
	// bootstraps them (deploy/README.md; the same pair sharedk8s connects with).
	endpointComputeUser = "cloud_admin"
	// endpointComputePassword is what the compute's supervisor sets for
	// cloud_admin over its loopback trust connection so REMOTE clients can
	// authenticate (endpoint_supervisor remote_auth.rs; the value is
	// env_or("COMPUTE_PASSWORD", "cloud_admin") and the chart does not override
	// it). sharedk8s never needed it because a port-forward arrives on loopback,
	// where pg_hba trusts; this client pod is a remote peer.
	endpointComputePassword = "cloud_admin"
	endpointComputeDB       = "postgres"
	// endpointComputePort is the chart's endpoint.computePort — the port the
	// endpoint Service publishes for compute Postgres. Used only when the
	// controller reports an endpoint address without one.
	endpointComputePort = 55433

	// perfPoolName / perfClusterPrefix name the objects carved out of the
	// run-scoped control plane. It serves exactly one cluster, so the names need
	// no cross-run uniqueness beyond the resource suffix.
	perfPoolName       = "ampqa-perf"
	perfClusterPrefix  = "ampqa-perf-"
	perfCPNamespaceFmt = "ampqa-perf-cp-%s"

	// endpointSettle is a short pause after the compute workload reports ready.
	// The chart's readiness probe is a TCP check on the compute port, which can
	// answer a beat before Postgres completes its own startup — and BenchBase's
	// create phase connects ONCE, with no retry.
	endpointSettle = 10 * time.Second
)

// ampEndpoint is the provisioned control plane plus its single compute endpoint:
// the namespace it lives in, a kubectl bound to it (for diagnostics and
// endpoint-pod metrics), and the in-cluster connection the client pod drives.
type ampEndpoint struct {
	Namespace  string
	Kube       *k8s.Kubectl
	EndpointID string
	Conn       pg.ConnInfo
}

// podSelector matches the endpoint's compute pods — the app.kubernetes.io/instance
// label the endpoint's workload stamps on them.
func (e ampEndpoint) podSelector() string {
	return "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(e.EndpointID)
}

// ampCPNamespace is the control-plane namespace for a run (or a comparison
// variant): its own, never the backing's, so its teardown is independent.
func ampCPNamespace(suffix string) string { return fmt.Sprintf(perfCPNamespaceFmt, suffix) }

// ampBacking maps a deployed YB-in-k8s backing into the ampstack Backing config.
//
// HostPorts MUST be the per-pod stable tserver DNS names, never the tserver
// Service: the endpoint's thin client dials the tserver RPC port (9100) on the
// same host it is given for YSQL, and the Service publishes only 5433 — kube-proxy
// black-holes ClusterIP:9100 and the endpoint crash-loops on a heartbeat timeout
// (meko#648). AdminHostPorts stays the coordinator-reachable tailnet CSV, which
// is the only path this process can dial for the one-shot yb_storage CREATE.
func ampBacking(info ybk8s.Info) ampstack.Backing {
	host, port := ybk8s.FirstHostPort(strings.Join(info.TailnetAddrs, ","))
	conn := pg.ConnInfo{Host: host, Port: port, User: info.User, Database: info.User, SSLMode: "disable"}
	b := ybbacking.ExternalBacking(ybbacking.FromYBK8s(conn, info.TailnetAddrs, info.TserverHostPorts))
	// The backing is in THIS cluster, so the one-shot yb_storage CREATE runs in a
	// tserver rather than being dialled from here — see Backing.KubeNamespace.
	b.KubeNamespace = info.Namespace
	return b
}

// endpointConn builds the connection the in-cluster client pod drives: the
// endpoint Service's STABLE DNS name, cross-namespace from the client pod's own.
// The controller-reported address is only consulted for its port — under the
// default ClusterIP service type that address is the compute POD IP, which a
// reschedule invalidates.
func endpointConn(namespace, endpointID, reportedAddr string) pg.ConnInfo {
	return pg.ConnInfo{
		Host:     fmt.Sprintf("%s.%s.svc.cluster.local", ampstack.EndpointWorkloadName(endpointID), namespace),
		Port:     addrPort(reportedAddr, endpointComputePort),
		User:     endpointComputeUser,
		Password: endpointComputePassword,
		Database: endpointComputeDB,
		SSLMode:  "disable",
	}
}

// addrPort extracts the port of a "host:port" address, falling back to def.
func addrPort(addr string, def int) int {
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return def
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return def
	}
	return port
}

// ampStackConfig resolves the control-plane config for one run/variant from an
// ALREADY-DERIVED amp config (so a comparison variant's own controller/endpoint
// image overrides apply), pinning the namespace, the kube-context and the
// external backing.
func ampStackConfig(amp ampstack.Config, ybCfg ybk8s.Config, info ybk8s.Info, suffix string) ampstack.Config {
	amp.Namespace = ampCPNamespace(suffix)
	if amp.Context == "" {
		amp.Context = ybCfg.KubeContext
	}
	amp.Backing = ampBacking(info)
	return amp
}

// provisionAmpEndpoint stands the control plane up on the already-running
// backing, carves one cluster + read_write endpoint out of it, and returns the
// endpoint's compute connection. Teardown (control plane namespace, cascading
// the endpoint) is registered on td before the deploy, so a partial bring-up is
// still reclaimed — and because td is LIFO and the backing was pushed first, the
// control plane always goes down before the cluster it sits on.
func provisionAmpEndpoint(ctx context.Context, t *core.T, amp ampstack.Config, ybCfg ybk8s.Config, info ybk8s.Info, suffix string, sink apps.ArtifactSink, td *teardownStack, logf func(string, ...any)) (ampEndpoint, error) {
	cfg := ampStackConfig(amp, ybCfg, info, suffix)
	app := ampstack.New(cfg).WithLogf(logf)
	if ybCfg.Kubeconfig != "" {
		app = app.WithKube(k8s.New(k8s.Config{Context: cfg.Context, Namespace: cfg.Namespace}).
			WithRunner(k8s.KubeconfigRunner(ybCfg.Kubeconfig)))
	}
	td.push("teardown amp control plane (namespace + endpoint)", func(ctx context.Context) error {
		_ = app.Close(ctx)
		return app.Teardown(ctx)
	})

	fail := func(format string, args ...any) (ampEndpoint, error) {
		diag.CollectK8sDiagnostics(ctx, app.Kube(), cfg.Namespace, sink, logf)
		return ampEndpoint{}, core.EnvFail("amp control plane %s: "+format, append([]any{cfg.Namespace}, args...)...)
	}

	if _, err := app.Deploy(ctx); err != nil {
		return fail("deploy: %w", err)
	}
	cl, url, err := app.ControllerClient(ctx)
	if err != nil {
		return fail("reach controller: %w", err)
	}
	logf("perf: amp control plane ready in %s (controller %s, backing %s)", cfg.Namespace, url, cfg.Backing.HostPorts)

	ep, err := createPerfEndpoint(ctx, cl, suffix)
	if err != nil {
		return fail("%w", err)
	}
	if err := ampstack.WaitEndpointWorkloadReady(ctx, app.Kube(), ep.ID, cfg.ControllerReadyTimeout); err != nil {
		return fail("endpoint %s compute never became ready: %w", ep.ID, err)
	}
	if err := sleepCtx(ctx, endpointSettle); err != nil {
		return ampEndpoint{}, err
	}

	out := ampEndpoint{Namespace: cfg.Namespace, Kube: app.Kube(), EndpointID: ep.ID, Conn: endpointConn(cfg.Namespace, ep.ID, ep.Address)}
	logf("perf: workload targets amp endpoint %s at %s", ep.ID, out.Conn.Addr())
	return out, nil
}

// createPerfEndpoint carves the one user/pool/cluster/endpoint this run needs
// out of a freshly deployed control plane. Nothing else uses it, so there is no
// name contention to tolerate — unlike sharedk8s, which serves every parallel
// test off one control plane.
func createPerfEndpoint(ctx context.Context, cl *controller.Client, suffix string) (controller.Endpoint, error) {
	user, err := cl.CreateUser(ctx, controller.CreateUserRequest{})
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("create user: %w", err)
	}
	pool, err := cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: perfPoolName, OwnerUserID: user.ID})
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("create storage pool %s: %w", perfPoolName, err)
	}
	return createPerfEndpointInPool(ctx, cl, pool.ID, suffix)
}

// createPerfEndpointInPool carves the cluster + endpoint into a pool the caller
// already owns — the live-arm path, whose pool exists since provisioning.
func createPerfEndpointInPool(ctx context.Context, cl *controller.Client, poolID, suffix string) (controller.Endpoint, error) {
	name := perfClusterPrefix + suffix
	if _, err := cl.CreateClusterAndWait(ctx, poolID, controller.CreateClusterRequest{Name: name}); err != nil {
		return controller.Endpoint{}, fmt.Errorf("create cluster %s: %w", name, err)
	}
	proj, err := findPerfCluster(ctx, cl, poolID, name)
	if err != nil {
		return controller.Endpoint{}, err
	}
	branchID := proj.DefaultBranch.BranchID
	if _, err := cl.CreateEndpointAndWait(ctx, poolID, proj.ID, branchID, controller.CreateEndpointRequest{Role: "read_write"}); err != nil {
		return controller.Endpoint{}, fmt.Errorf("create endpoint: %w", err)
	}
	eps, err := cl.ListAllEndpoints(ctx, poolID, proj.ID, branchID)
	if err != nil {
		return controller.Endpoint{}, fmt.Errorf("list endpoints: %w", err)
	}
	return pickReadWrite(eps)
}

// pickReadWrite returns the read_write endpoint of a branch (falling back to the
// first). A benchmark writes, so a read-only compute would fail the load phase
// rather than measure it.
func pickReadWrite(eps []controller.Endpoint) (controller.Endpoint, error) {
	for _, ep := range eps {
		if ep.Role == "read_write" {
			return ep, nil
		}
	}
	if len(eps) == 0 {
		return controller.Endpoint{}, fmt.Errorf("no endpoints after create")
	}
	return eps[0], nil
}

// findPerfCluster resolves the just-created cluster by name.
func findPerfCluster(ctx context.Context, cl *controller.Client, poolID, name string) (controller.Cluster, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListClusters(ctx, poolID, page, offset)
		if err != nil {
			return controller.Cluster{}, fmt.Errorf("list clusters: %w", err)
		}
		for _, proj := range list.Clusters {
			if proj.Name == name {
				return proj, nil
			}
		}
		if list.Count < page || offset+list.Count >= list.Total {
			return controller.Cluster{}, fmt.Errorf("created cluster %q not found", name)
		}
	}
}
