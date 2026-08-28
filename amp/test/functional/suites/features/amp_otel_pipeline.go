// amp_otel_pipeline asserts the observability pipeline the control-plane chart
// deploys is actually healthy: the postgres-exporter sidecar on an endpoint pod
// serves a parseable Prometheus exposition, and the OpenTelemetry collectors the
// chart declares are present and ready in the run's own namespace. The collector
// half is gated on the opentelemetry.io CRDs, the same condition the chart's
// otel documents render on, so a cluster without the OpenTelemetry Operator
// skips naming that CRD while a cluster that HAS it fails on a missing or
// unhealthy collector.
package features

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	// otelCollectorCRD carries the OpenTelemetryCollector kind the chart's otel
	// documents need, and otelCollectorAPIVersion is the served version the chart
	// gates rendering on (.Capabilities.APIVersions.Has "opentelemetry.io/v1beta1").
	otelCollectorCRD        = "opentelemetrycollectors.opentelemetry.io"
	otelCollectorAPIVersion = "v1beta1"
	// otelCollectorPodInfix is how the OpenTelemetry Operator names the workload it
	// reconciles for a collector CR: <cr name>-collector.
	otelCollectorPodInfix = "-collector-"
	// otelExporterContainer is the endpoint pod's postgres-exporter sidecar.
	otelExporterContainer = "postgres-exporter"
	// otelExporterUpSeries is postgres_exporter's own connect gauge: 1 only while it
	// can reach the compute's Postgres.
	otelExporterUpSeries = "pg_up"

	// DefaultOtelReadyTimeout bounds both waits (collectors ready, exporter
	// serving).
	DefaultOtelReadyTimeout = 5 * time.Minute
	// DefaultOtelMaxCollectorRestarts is the restart budget per collector pod.
	DefaultOtelMaxCollectorRestarts = 0
)

// DefaultOtelCollectors are the collector CR name suffixes the chart renders
// (values otel.collectors.controller / otel.collectors.compute, both on).
var DefaultOtelCollectors = []string{"controller", "compute"}

// otelExporterSeries are series the shipped amp-monitoring Postgres dashboard
// reads, so a scrape missing one means the exporter answers without collecting.
var otelExporterSeries = []string{otelExporterUpSeries, "pg_stat_database_numbackends"}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_otel_pipeline",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Prove the metrics pipeline the control-plane chart deploys comes up healthy: an endpoint pod's postgres-exporter sidecar serves a parseable exposition carrying the series the shipped dashboards read, and on a cluster whose opentelemetry.io CRDs let the chart render them, every declared OpenTelemetry collector is present and ready in the run's namespace.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpOtelPipeline,
	})
}

func runAmpOtelPipeline(t *core.T) error {
	o := otelOptions{
		collectors:      t.Options.GetStringSlice("functional.otel.collectors", DefaultOtelCollectors),
		expectCollector: t.Options.GetBool("functional.otel.expect_collectors", true),
		readyTimeout:    t.Options.GetDuration("functional.otel.ready_timeout", DefaultOtelReadyTimeout),
		maxRestarts:     t.Options.GetInt("functional.otel.max_collector_restarts", DefaultOtelMaxCollectorRestarts),
		kubeContext:     byocKubeContext(t.Options),
	}
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &otelRun{a: a, opt: o}
		// The exporter phase runs first on purpose: it needs no operator, so its
		// coverage must not be lost to the collector phase's environment skip.
		return []phase{
			{"endpoint_exporter", r.endpointExporter},
			{"collector_health", r.collectorHealth},
		}
	})
}

type otelOptions struct {
	collectors      []string
	expectCollector bool
	readyTimeout    time.Duration
	maxRestarts     int
	kubeContext     string
}

type otelRun struct {
	a   *ampFeatureRun
	opt otelOptions
}

func (r *otelRun) record(name string, props map[string]any) {
	_ = r.a.f.t.Step(name, func(h *core.StepHandle) error {
		for k, v := range props {
			h.Set(k, v)
		}
		return nil
	})
}

// otelExec runs one kubectl invocation; the scope flags are the caller's.
type otelExec func(ctx context.Context, extra ...string) (exec.Result, error)

func (r *otelRun) kubectl(kube *k8s.Kubectl) otelExec {
	p := &byocKube{run: kube.Runner(), bin: byocKubectlBin, kubeContext: r.opt.kubeContext}
	return p.exec
}

// --- postgres-exporter sidecar ---

func (r *otelRun) endpointExporter(ctx context.Context) error {
	kube := r.a.f.ampKube()
	if kube == nil {
		return core.AsEnvFailure(fmt.Errorf("no kubectl for the control-plane namespace (direct-URL control plane), so an endpoint pod's postgres-exporter sidecar cannot be probed"))
	}
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID)
	if err != nil {
		return err
	}
	pod, err := (&kubeComputeRoller{kube: kube}).podName(ctx, ep.ID)
	if err != nil {
		return err
	}
	ex := r.kubectl(kube)
	ns := kube.Namespace()

	rows, err := otelListPods(ctx, ex, ns, "app.kubernetes.io/instance="+endpointDeployName(ep.ID))
	if err != nil {
		return err
	}
	if err := otelExporterContainerVerdict(rows, pod, r.opt.maxRestarts); err != nil {
		return err
	}
	port, annotations, err := otelScrapeAnnotations(ctx, ex, ns, pod)
	if err != nil {
		return err
	}

	scrape := pageserver.New(otelPodScrape(kube, pod, endpointPodContainer), port)
	var samples pageserver.Samples
	waitErr := r.a.f.t.WaitFor(fmt.Sprintf("postgres-exporter in pod %s serving on :%d", pod, port),
		r.opt.readyTimeout, 5*time.Second, func() (bool, error) {
			s, err := scrape.Metrics(ctx)
			if err != nil {
				r.a.f.logf("otel_pipeline: exporter scrape not answering yet: %v", err)
				return false, nil
			}
			samples = s
			if verr := otelExporterVerdict(samples, otelExporterSeries); verr != nil {
				r.a.f.logf("otel_pipeline: exporter answered but %v", verr)
				return false, nil
			}
			return true, nil
		})
	up, _ := samples.Value(otelExporterUpSeries, nil)
	r.record("postgres_exporter", map[string]any{
		"pod":                pod,
		"port":               port,
		"annotations":        annotations,
		"series":             len(samples),
		otelExporterUpSeries: up,
	})
	if waitErr != nil {
		return fmt.Errorf("probe postgres-exporter in pod %s (scrape http://localhost:%d/metrics from the %s container, expected a parseable exposition carrying %s, last state: %w): the sidecar the amp metrics pipeline and the shipped Postgres dashboard both read is not serving",
			pod, port, endpointPodContainer, strings.Join(otelExporterSeries, ", "), otelExporterVerdictOrErr(samples, waitErr))
	}
	return nil
}

// otelExporterVerdictOrErr prefers the exposition's own verdict over the wait's
// generic timeout, so the failure names what the scrape actually returned.
func otelExporterVerdictOrErr(samples pageserver.Samples, waitErr error) error {
	if verr := otelExporterVerdict(samples, otelExporterSeries); verr != nil {
		return verr
	}
	return waitErr
}

// otelExporterVerdict decides whether one scrape proves the sidecar serves: a
// parseable exposition, pg_up at 1 (it reached Postgres), and every series the
// dashboards read present.
func otelExporterVerdict(samples pageserver.Samples, want []string) error {
	if len(samples) == 0 {
		return fmt.Errorf("the scrape returned no parseable prometheus samples")
	}
	up, ok := samples.Value(otelExporterUpSeries, nil)
	if !ok {
		return fmt.Errorf("the scrape carries %d series but not %s, so this is not a postgres_exporter exposition", len(samples), otelExporterUpSeries)
	}
	if up != 1 {
		return fmt.Errorf("%s = %v, want 1: the exporter answers but cannot reach the compute's Postgres", otelExporterUpSeries, up)
	}
	for _, name := range want {
		if _, ok := samples.Value(name, nil); !ok {
			return fmt.Errorf("the scrape carries %d series but not %s, which the shipped amp Postgres dashboard reads", len(samples), name)
		}
	}
	return nil
}

// otelExporterContainerVerdict checks the sidecar exists, is ready, and has not
// been crash-looping.
func otelExporterContainerVerdict(rows []otelPod, pod string, maxRestarts int) error {
	row, ok := otelFindPod(rows, pod)
	if !ok {
		return fmt.Errorf("pod %s is not in the endpoint's pod list [%s], so its containers cannot be read", pod, otelPodNames(rows))
	}
	c, ok := row.container(otelExporterContainer)
	if !ok {
		return fmt.Errorf("endpoint pod %s runs containers [%s] and none is named %s: the sidecar the amp metrics pipeline reads is not deployed", pod, row.containerNames(), otelExporterContainer)
	}
	if !c.ready {
		return fmt.Errorf("container %s in endpoint pod %s is not ready (%d restarts): the exporter sidecar is deployed but not serving", otelExporterContainer, pod, c.restarts)
	}
	if c.restarts > maxRestarts {
		return fmt.Errorf("container %s in endpoint pod %s restarted %d times, budget %d: the exporter sidecar is crash-looping", otelExporterContainer, pod, c.restarts, maxRestarts)
	}
	return nil
}

const otelAnnotationJSONPath = `{.metadata.annotations.prometheus\.io/scrape}|` +
	`{.metadata.annotations.prometheus\.io/port}|{.metadata.annotations.prometheus\.io/path}`

// otelScrapeAnnotations reads the pod's prometheus.io annotations, the contract
// the compute collector's pod discovery keeps, and returns the port they publish.
func otelScrapeAnnotations(ctx context.Context, ex otelExec, ns, pod string) (int, string, error) {
	res, err := ex(ctx, "-n", ns, "get", "pod", pod, "-o", "jsonpath="+otelAnnotationJSONPath)
	if err != nil || res.ExitCode != 0 {
		return 0, "", fmt.Errorf("read the prometheus.io annotations of pod %s: exit %d: %v %s", pod, res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	port, err := otelParseScrapeAnnotations(res.Stdout)
	if err != nil {
		return 0, strings.TrimSpace(res.Stdout), fmt.Errorf("endpoint pod %s: %w: the compute collector discovers the exporter through these annotations, so a collector would never scrape this pod", pod, err)
	}
	return port, strings.TrimSpace(res.Stdout), nil
}

// otelParseScrapeAnnotations validates scrape|port|path and returns the port.
func otelParseScrapeAnnotations(out string) (int, error) {
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 {
		return 0, fmt.Errorf("prometheus.io/{scrape,port,path} read back as %q, want three values", strings.TrimSpace(out))
	}
	scrape, rawPort, path := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
	if scrape != "true" {
		return 0, fmt.Errorf("prometheus.io/scrape = %q, want \"true\"", scrape)
	}
	if path != "/metrics" {
		return 0, fmt.Errorf("prometheus.io/path = %q, want \"/metrics\"", path)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("prometheus.io/port = %q, want a port number", rawPort)
	}
	return port, nil
}

// otelPodScrape scrapes an in-pod HTTP port from the endpoint container, which is
// the one carrying bash (the exporter's own image is not guaranteed a shell).
func otelPodScrape(kube *k8s.Kubectl, pod, container string) pageserver.ExecFunc {
	return func(ctx context.Context, script string) (string, error) {
		res, err := kube.ForPod(pod, container).Run(ctx, script)
		if err != nil {
			return "", fmt.Errorf("exec in pod %s container %s: %w", pod, container, err)
		}
		switch res.ExitCode {
		case 0:
			return res.Stdout, nil
		case pageserver.ConnectRefusedExit:
			return "", fmt.Errorf("nothing accepts a connection on the metrics port inside pod %s", pod)
		default:
			return "", fmt.Errorf("scrape in pod %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
}

// --- collectors ---

func (r *otelRun) collectorHealth(ctx context.Context) error {
	kube := r.a.f.ampKube()
	if kube == nil {
		return core.AsEnvFailure(fmt.Errorf("no kubectl for the control-plane namespace (direct-URL control plane), so the collectors cannot be read"))
	}
	ex := r.kubectl(kube)
	ns := kube.Namespace()

	versions, err := otelCRDVersions(ctx, ex)
	if err != nil {
		return err
	}
	r.record("otel_capability", map[string]any{
		"crd":             otelCollectorCRD,
		"served_versions": strings.Join(versions, " "),
		"can_render":      otelCRDSkipReason(versions) == "",
	})
	if reason := otelCRDSkipReason(versions); reason != "" {
		r.a.f.t.Skip(reason)
		return nil
	}

	crs, err := otelCollectorCRs(ctx, ex, ns)
	if err != nil {
		return err
	}
	var pods []otelPod
	waitErr := r.a.f.t.WaitFor(fmt.Sprintf("otel collectors %v ready in namespace %s", r.opt.collectors, ns),
		r.opt.readyTimeout, 10*time.Second, func() (bool, error) {
			p, err := otelListPods(ctx, ex, ns, "")
			if err != nil {
				return false, err
			}
			pods = p
			if verr := otelCollectorVerdict(crs, pods, r.opt.collectors, r.opt.maxRestarts); verr != nil {
				r.a.f.logf("otel_pipeline: collectors not healthy yet: %v", verr)
				return false, nil
			}
			return true, nil
		})
	verdict := otelCollectorVerdict(crs, pods, r.opt.collectors, r.opt.maxRestarts)
	r.record("otel_collectors", map[string]any{
		"namespace": ns,
		"expected":  strings.Join(r.opt.collectors, " "),
		"crs":       strings.Join(crs, " "),
		"pods":      otelCollectorPodLine(crs, pods),
		"asserted":  r.opt.expectCollector,
	})
	if waitErr == nil {
		return nil
	}
	if verdict == nil {
		verdict = waitErr
	}
	if !r.opt.expectCollector {
		r.a.f.logf("otel_pipeline: %v (recorded only: functional.otel.expect_collectors is off)", verdict)
		return nil
	}
	return fmt.Errorf("probe otel collectors in namespace %s (expected a collector CR per %s with every %s pod Running and ready, saw CRs [%s] and pods [%s]): %w — the cluster serves %s %s, so the chart rendered these collectors and they must come up",
		ns, strings.Join(r.opt.collectors, ", "), otelCollectorPodInfix, strings.Join(crs, " "), otelCollectorPodLine(crs, pods), verdict, otelCollectorCRD, otelCollectorAPIVersion)
}

// otelCRDVersions returns the versions the OpenTelemetryCollector CRD serves, or
// nothing at all when the CRD is absent.
func otelCRDVersions(ctx context.Context, ex otelExec) ([]string, error) {
	res, err := ex(ctx, "get", "crd", otelCollectorCRD, "--ignore-not-found",
		"-o", "jsonpath={.spec.versions[*].name}")
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("read the %s CRD: exit %d: %v %s", otelCollectorCRD, res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	return strings.Fields(res.Stdout), nil
}

// otelCRDSkipReason names the environment condition that stops the chart from
// rendering any collector document, and is empty when the cluster can run them.
func otelCRDSkipReason(versions []string) string {
	if len(versions) == 0 {
		return fmt.Sprintf("this cluster has no %s CRD, so the control-plane chart renders no collector documents at all (the OpenTelemetry Operator is not installed) and there is no collector to assert health of", otelCollectorCRD)
	}
	for _, v := range versions {
		if v == otelCollectorAPIVersion {
			return ""
		}
	}
	return fmt.Sprintf("this cluster's %s CRD serves [%s] and not %s, which is the API version the control-plane chart gates its collector documents on, so no collector is rendered here",
		otelCollectorCRD, strings.Join(versions, " "), otelCollectorAPIVersion)
}

// otelCollectorCRs lists the collector CRs in the namespace.
func otelCollectorCRs(ctx context.Context, ex otelExec, ns string) ([]string, error) {
	res, err := ex(ctx, "-n", ns, "get", otelCollectorCRD, "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("list %s in namespace %s: exit %d: %v %s", otelCollectorCRD, ns, res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// otelCollectorVerdict pairs each expected collector with its CR and the
// operator-reconciled pods behind it, and reports the first thing missing.
func otelCollectorVerdict(crs []string, pods []otelPod, want []string, maxRestarts int) error {
	for _, suffix := range want {
		cr, ok := otelCRForSuffix(crs, suffix)
		if !ok {
			return fmt.Errorf("no collector CR named *-%s exists, CRs present [%s]: the chart declares this collector, so the operator was never handed one or it was removed", suffix, strings.Join(crs, " "))
		}
		matched := otelPodsWithPrefix(pods, cr+otelCollectorPodInfix)
		if len(matched) == 0 {
			return fmt.Errorf("collector CR %s has no pod named %s*: the operator reconciled no workload for it", cr, cr+otelCollectorPodInfix)
		}
		for _, p := range matched {
			if p.phase != "Running" || !p.allReady() {
				return fmt.Errorf("collector pod %s (CR %s) is phase %s with containers [%s], want Running and every container ready", p.name, cr, p.phase, p.containerLine())
			}
			if n := p.restarts(); n > maxRestarts {
				return fmt.Errorf("collector pod %s (CR %s) restarted %d times, budget %d: the collector is crash-looping", p.name, cr, n, maxRestarts)
			}
		}
	}
	return nil
}

func otelCRForSuffix(crs []string, suffix string) (string, bool) {
	for _, cr := range crs {
		if strings.HasSuffix(cr, "-"+suffix) {
			return cr, true
		}
	}
	return "", false
}

func otelPodsWithPrefix(pods []otelPod, prefix string) []otelPod {
	var out []otelPod
	for _, p := range pods {
		if strings.HasPrefix(p.name, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// otelCollectorPodLine renders the collector pods and their state for a step
// property or a failure message.
func otelCollectorPodLine(crs []string, pods []otelPod) string {
	var parts []string
	for _, cr := range crs {
		for _, p := range otelPodsWithPrefix(pods, cr+otelCollectorPodInfix) {
			parts = append(parts, fmt.Sprintf("%s=%s(%s)", p.name, p.phase, p.containerLine()))
		}
	}
	return strings.Join(parts, " ")
}

// --- pod reading ---

// otelPod is one pod's phase and per-container readiness/restarts.
type otelPod struct {
	name       string
	phase      string
	containers []otelContainer
}

type otelContainer struct {
	name     string
	ready    bool
	restarts int
}

func (p otelPod) container(name string) (otelContainer, bool) {
	for _, c := range p.containers {
		if c.name == name {
			return c, true
		}
	}
	return otelContainer{}, false
}

func (p otelPod) allReady() bool {
	if len(p.containers) == 0 {
		return false
	}
	for _, c := range p.containers {
		if !c.ready {
			return false
		}
	}
	return true
}

func (p otelPod) restarts() int {
	n := 0
	for _, c := range p.containers {
		n += c.restarts
	}
	return n
}

func (p otelPod) containerNames() string {
	names := make([]string, 0, len(p.containers))
	for _, c := range p.containers {
		names = append(names, c.name)
	}
	return strings.Join(names, " ")
}

func (p otelPod) containerLine() string {
	parts := make([]string, 0, len(p.containers))
	for _, c := range p.containers {
		parts = append(parts, fmt.Sprintf("%s ready=%v restarts=%d", c.name, c.ready, c.restarts))
	}
	return strings.Join(parts, ", ")
}

func otelFindPod(pods []otelPod, name string) (otelPod, bool) {
	for _, p := range pods {
		if p.name == name {
			return p, true
		}
	}
	return otelPod{}, false
}

func otelPodNames(pods []otelPod) string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.name)
	}
	return strings.Join(names, " ")
}

const otelPodJSONPath = `{range .items[*]}{.metadata.name}|{.status.phase}|` +
	`{range .status.containerStatuses[*]}{.name},{.ready},{.restartCount};{end}{"\n"}{end}`

// otelListPods reads name, phase, and per-container readiness/restarts for the
// namespace's pods (all of them when selector is empty).
func otelListPods(ctx context.Context, ex otelExec, ns, selector string) ([]otelPod, error) {
	extra := []string{"-n", ns, "get", "pods"}
	if selector != "" {
		extra = append(extra, "-l", selector)
	}
	extra = append(extra, "-o", "jsonpath="+otelPodJSONPath)
	res, err := ex(ctx, extra...)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("list pods in namespace %s: exit %d: %v %s", ns, res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	return otelParsePods(res.Stdout), nil
}

func otelParsePods(out string) []otelPod {
	var pods []otelPod
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		p := otelPod{name: fields[0], phase: fields[1]}
		for _, raw := range strings.Split(fields[2], ";") {
			cf := strings.Split(strings.TrimSpace(raw), ",")
			if len(cf) != 3 || cf[0] == "" {
				continue
			}
			restarts, _ := strconv.Atoi(cf[2])
			p.containers = append(p.containers, otelContainer{
				name:     cf[0],
				ready:    cf[1] == "true",
				restarts: restarts,
			})
		}
		pods = append(pods, p)
	}
	return pods
}
