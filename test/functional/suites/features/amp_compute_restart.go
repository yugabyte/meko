package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// restartTable is the table the compute-restart test seeds and re-reads
	// across the compute pod restart.
	restartTable = "amp_restart"
	// restartRows is the row count seeded before the restart and asserted intact
	// after it.
	restartRows = 200
	// restartReadyTimeout bounds waiting for the endpoint's Deployment to become
	// Available again after the pod is rolled.
	restartReadyTimeout = 5 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_compute_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Storage/compute separation: restart an endpoint's compute pod and assert the checksummed dataset survived.",
		Params:      []string{"functional", "env.ampstack", "apps.ybk8s"},
		Run:         runAmpComputeRestart,
	})
}

// runAmpComputeRestart is the headline storage/compute-separation proof. It
// writes a known, checksummed dataset through a compute endpoint, RESTARTS the
// endpoint's compute (deletes its pod — the endpoint's Deployment reschedules a
// fresh one), then reconnects and asserts every row (and the checksum) survived.
// The data lives in the shared external YugabyteDB storage, not in the ephemeral
// compute, so it must persist across a compute restart.
func runAmpComputeRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"compute_restart", a.computeRestart}}
	})
}

// computeRestart is the live feature phase: it wires the real amp-namespace
// kubectl (for finding + rolling the endpoint pod and resolving its fresh IP)
// and a pod-IP connector into the seam-pure restart logic.
func (a *ampFeatureRun) computeRestart(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	roller := &kubeComputeRoller{kube: a.f.ampKube(), timeout: restartReadyTimeout}
	return computeRestartSteps(ctx, cl, a.proj, a.f.endpointSQLAt, roller, a.f.t.Metric)
}

// podConnector opens a SQL connection to an endpoint at a specific (freshly
// resolved) pod IP — the compute pod IP changes across a restart, so the stale
// address the controller first reported cannot be reused.
type podConnector func(ctx context.Context, ip string, ep controller.Endpoint) (sqlConn, error)

// computeRoller is the pod-lifecycle surface the restart test drives: find the
// endpoint's compute pod, roll it, wait for the Deployment to recover, and
// resolve the rescheduled pod's IP. kubeComputeRoller is the live impl; a fake
// satisfies it in unit tests.
type computeRoller interface {
	podName(ctx context.Context, endpointID string) (string, error)
	deletePod(ctx context.Context, name string) error
	waitReady(ctx context.Context, endpointID string) error
	runningPodIP(ctx context.Context, endpointID string) (string, error)
}

// computeRestartSteps drives the storage/compute-separation assertions over the
// ampCtl, pod-connector, and computeRoller seams, so write → restart-compute →
// (data + checksum survive) is unit-testable with fakes.
func computeRestartSteps(ctx context.Context, cl ampCtl, proj ampProject, connect podConnector, roller computeRoller, metric func(string, float64, string)) error {
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}

	ip0, err := roller.runningPodIP(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("resolve endpoint compute pod IP: %w", err)
	}
	c0, err := connect(ctx, ip0, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	if _, err := c0.Exec(ctx, "CREATE TABLE "+restartTable+" (id int primary key, payload text)"); err != nil {
		c0.Close()
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := c0.Exec(ctx, "INSERT INTO "+restartTable+" (id, payload) SELECT g, md5(g::text) FROM generate_series(1, $1) g", restartRows); err != nil {
		c0.Close()
		return fmt.Errorf("seed rows: %w", err)
	}
	before, err := c0.QueryInt(ctx, "SELECT count(*) FROM "+restartTable)
	if err != nil {
		c0.Close()
		return fmt.Errorf("count seeded rows: %w", err)
	}
	sumBefore, err := c0.QueryString(ctx, "SELECT coalesce(md5(string_agg(payload, '' ORDER BY id)), '') FROM "+restartTable)
	c0.Close()
	if err != nil {
		return fmt.Errorf("checksum before restart: %w", err)
	}
	if before != restartRows {
		return fmt.Errorf("seeded %d rows, expected %d", before, restartRows)
	}

	// Restart the compute: roll the endpoint pod; its Deployment reschedules one.
	pod, err := roller.podName(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("find endpoint compute pod: %w", err)
	}
	if err := roller.deletePod(ctx, pod); err != nil {
		return fmt.Errorf("delete endpoint compute pod %s: %w", pod, err)
	}
	if err := roller.waitReady(ctx, ep.ID); err != nil {
		return fmt.Errorf("endpoint compute did not come back after restart: %w", err)
	}
	ip1, err := roller.runningPodIP(ctx, ep.ID)
	if err != nil {
		return fmt.Errorf("resolve rescheduled compute pod IP: %w", err)
	}

	// Reconnect (fresh pod IP) and assert the data + checksum survived.
	c1, err := connect(ctx, ip1, ep)
	if err != nil {
		return fmt.Errorf("reconnect after restart: %w", err)
	}
	defer c1.Close()
	after, err := c1.QueryInt(ctx, "SELECT count(*) FROM "+restartTable)
	if err != nil {
		return fmt.Errorf("count rows after restart: %w", err)
	}
	sumAfter, err := c1.QueryString(ctx, "SELECT coalesce(md5(string_agg(payload, '' ORDER BY id)), '') FROM "+restartTable)
	if err != nil {
		return fmt.Errorf("checksum after restart: %w", err)
	}
	if metric != nil {
		metric("rows_persisted", float64(after), "count")
	}
	if after != before {
		return fmt.Errorf("storage/compute separation violated: %d rows before compute restart, %d after", before, after)
	}
	if sumAfter != sumBefore {
		return fmt.Errorf("storage/compute separation violated: dataset checksum changed across compute restart")
	}
	return nil
}

// endpointSQLAt connects to an endpoint at a specific pod IP (the compute pod IP
// changes across a restart), reusing the endpoint's port + configured
// credentials.
func (f *flow) endpointSQLAt(ctx context.Context, ip string, ep controller.Endpoint) (sqlConn, error) {
	if f.ampEnv != nil {
		// Attach mode: reach the endpoint through the provider's Service
		// port-forward, which routes to whichever compute pod is currently Ready —
		// the pod IP the roller resolved is not operator-routable, and the Service
		// re-targets the rescheduled pod across a restart, so ignore ip here.
		return f.ampEnv.ConnectEndpoint(ctx, ep)
	}
	ci, err := f.endpointConnInfo(ep)
	if err != nil {
		return nil, err
	}
	ci.Host = ip
	return connectEndpoint(ctx, ci)
}

// ampKube returns a kubectl scoped to the amp control-plane namespace where the
// endpoint compute pods (and their Deployments) live: the standing control plane's
// namespace in attach mode, else the self-deployed amp namespace on the ampstack
// context. It mirrors how ampstack itself builds its kube (ambient kubeconfig, the
// configured context).
func (f *flow) ampKube() *k8s.Kubectl {
	if f.ampEnv != nil {
		return f.ampEnv.AmpKube()
	}
	acfg := ampstack.FromConfig(f.t.Options)
	return k8s.New(k8s.Config{Context: acfg.Context, Namespace: f.namespace()})
}

// endpointDeployName is the controller's deploy/release name for an endpoint
// (controller EndpointDeployName): the endpoint UUID prefixed with "yb-ep-". The
// endpoint's Deployment, Service, and pods all carry this name, and the pods
// carry it as the app.kubernetes.io/instance label.
func endpointDeployName(endpointID string) string { return "yb-ep-" + endpointID }

// kubeComputeRoller is the live computeRoller: it finds, rolls, and re-resolves an
// endpoint's compute pod via kubectl in the amp namespace.
type kubeComputeRoller struct {
	kube    *k8s.Kubectl
	timeout time.Duration
}

// instanceSelector is the label selector matching an endpoint's compute pods.
func (r *kubeComputeRoller) instanceSelector(endpointID string) string {
	return "app.kubernetes.io/instance=" + endpointDeployName(endpointID)
}

// runningPod resolves the endpoint's Running pod (name + IP), skipping any pod
// that is Terminating during a reschedule. It reads name/phase/IP as three
// index-aligned lists (the simple jsonpath already proven elsewhere) rather than
// a fragile filter expression.
func (r *kubeComputeRoller) runningPod(ctx context.Context, endpointID string) (name, ip string, err error) {
	sel := r.instanceSelector(endpointID)
	names, err := r.kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
	if err != nil {
		return "", "", err
	}
	phases, err := r.kube.GetSelector(ctx, "pod", sel, "{.items[*].status.phase}")
	if err != nil {
		return "", "", err
	}
	ips, err := r.kube.GetSelector(ctx, "pod", sel, "{.items[*].status.podIP}")
	if err != nil {
		return "", "", err
	}
	nf, pf, ipf := strings.Fields(names), strings.Fields(phases), strings.Fields(ips)
	at := func(s []string, i int) string {
		if i < len(s) {
			return s[i]
		}
		return ""
	}
	for i := range nf {
		if at(pf, i) == "Running" {
			return nf[i], at(ipf, i), nil
		}
	}
	if len(nf) > 0 {
		return nf[0], at(ipf, 0), nil
	}
	return "", "", fmt.Errorf("no pod found for endpoint %s (selector %q)", endpointID, sel)
}

func (r *kubeComputeRoller) podName(ctx context.Context, endpointID string) (string, error) {
	name, _, err := r.runningPod(ctx, endpointID)
	return name, err
}

func (r *kubeComputeRoller) runningPodIP(ctx context.Context, endpointID string) (string, error) {
	_, ip, err := r.runningPod(ctx, endpointID)
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", fmt.Errorf("endpoint %s pod has no IP yet", endpointID)
	}
	return ip, nil
}

func (r *kubeComputeRoller) deletePod(ctx context.Context, name string) error {
	return r.kube.Delete(ctx, "pod", name)
}

// waitReady waits for the endpoint's Deployment to report Available again after
// the pod was rolled. It is a convergence hint; the true readiness gate is the
// pg connection (which polls SELECT 1) the caller makes right after.
func (r *kubeComputeRoller) waitReady(ctx context.Context, endpointID string) error {
	return r.kube.WaitFor(ctx, "deployment/"+endpointDeployName(endpointID), "condition=Available", r.timeout)
}
