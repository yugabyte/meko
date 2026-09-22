package perf

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	testNameClientWindow = "perf/client_window"
	cwPrefix             = "client_window."
	cwTable              = "cw_probe"
	cwHarnessRev         = "1"

	// A postmaster-context setting, so storing it forces a restart. Not shared_buffers: a bad value leaves a compute that cannot start again.
	cwRestartSettingName  = "track_commit_timestamp"
	cwRestartSettingValue = "on"
)

const (
	cwComputeRestart  = "compute_restart"
	cwResize          = "resize"
	cwSettingsUpdate  = "settings_update"
	cwUpgradeRoll     = "upgrade_roll"
	cwUpgradeRollBack = "upgrade_roll_back"
	cwPauseResume     = "pause_resume"
	cwBackingRoll     = "backing_roll"
)

const (
	DefaultClientWindowRateTPS          = 10
	DefaultClientWindowGrace            = 5 * time.Second
	DefaultClientWindowSampleInterval   = 1 * time.Second
	DefaultClientWindowStatementTimeout = 15 * time.Second
	DefaultClientWindowHold             = 30 * time.Second
	DefaultClientWindowRecoverTimeout   = 20 * time.Minute
	DefaultClientWindowEndpointCPU      = "2"
	DefaultClientWindowEndpointMem      = "4Gi"
	DefaultClientWindowResizeCPU        = "1"
	DefaultClientWindowResizeMem        = "2Gi"
	DefaultClientWindowJavaClient       = true
	DefaultClientWindowBackingRollSleep = 30 * time.Second
	DefaultClientWindowBackingSettle    = 2 * time.Minute
)

var DefaultClientWindowOperations = []string{
	cwComputeRestart, cwResize, cwSettingsUpdate, cwUpgradeRoll, cwPauseResume, cwBackingRoll,
}

type cwParams struct {
	Operations       []string
	RateTPS          int
	Grace            time.Duration
	SampleInterval   time.Duration
	StatementTimeout time.Duration
	Hold             time.Duration
	RecoverTimeout   time.Duration
	EndpointCPU      string
	EndpointMem      string
	ResizeCPU        string
	ResizeMem        string
	JavaClient       bool
	BackingRollSleep time.Duration
	BackingSettle    time.Duration
	PauseResume      bool
}

func cwParamsFromConfig(c *core.Config) cwParams {
	return cwParams{
		Operations:       c.GetStringSlice("perf.client_window.operations", DefaultClientWindowOperations),
		RateTPS:          max(c.GetInt("perf.client_window.rate_tps", DefaultClientWindowRateTPS), 1),
		Grace:            c.GetDuration("perf.client_window.grace", DefaultClientWindowGrace),
		SampleInterval:   c.GetDuration("perf.client_window.sample_interval", DefaultClientWindowSampleInterval),
		StatementTimeout: c.GetDuration("perf.client_window.statement_timeout", DefaultClientWindowStatementTimeout),
		Hold:             c.GetDuration("perf.client_window.hold", DefaultClientWindowHold),
		RecoverTimeout:   c.GetDuration("perf.client_window.recover_timeout", DefaultClientWindowRecoverTimeout),
		EndpointCPU:      c.GetString("perf.client_window.endpoint_cpu", DefaultClientWindowEndpointCPU),
		EndpointMem:      c.GetString("perf.client_window.endpoint_memory", DefaultClientWindowEndpointMem),
		ResizeCPU:        c.GetString("perf.client_window.resize_cpu", DefaultClientWindowResizeCPU),
		ResizeMem:        c.GetString("perf.client_window.resize_memory", DefaultClientWindowResizeMem),
		JavaClient:       c.GetBool("perf.client_window.java_client", DefaultClientWindowJavaClient),
		BackingRollSleep: c.GetDuration("perf.client_window.backing_roll_sleep", DefaultClientWindowBackingRollSleep),
		BackingSettle:    c.GetDuration("perf.client_window.backing_settle", DefaultClientWindowBackingSettle),
		PauseResume:      c.GetBool("perf.lifecycle.pause_resume", DefaultLifecyclePauseResume),
	}
}

func (p cwParams) wants(op string) bool {
	for _, name := range p.Operations {
		if name == op {
			return true
		}
	}
	return false
}

func init() {
	core.Register(core.Test{
		Name:       testNameClientWindow,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamControlPlane,
		Description: "Measure the client-visible error window of every amp operation (compute restart, resize, a settings update that forces a restart, an endpoint upgrade roll and back, pause/resume and a rolling backing " +
			"restart) from the controller task's own start, with a pgx pool, a pinned pgx session carrying a prepared statement and an idle transaction, and the Java sample app all driving the endpoint at " +
			"perf.client_window.rate_tps. Each operation gates error_seconds, first_success_after_seconds and errors_total, and every error has to be a SQLSTATE in class 57 or 08, a severed socket or a statement timeout, with " +
			"nothing at all outside the window plus perf.client_window.grace.",
		Params:  []string{"perf.client_window", "perf.gate", "perf.lifecycle"},
		Timeout: 4 * time.Hour,
		Run:     runClientWindow,
	})
}

type cwResult struct {
	Op                string
	ErrorSeconds      float64
	FirstSuccessAfter float64
	Errors            int
	Transport         int
	Timeouts          int
	Relay             int
	TaskSeconds       float64
	Start             time.Time
	End               time.Time
	Prepared          preparedStatementOutcome
}

type clientWindowRun struct {
	t      *core.T
	cl     *controller.Client
	env    ampMultitenantEnv
	kube   *k8s.Kubectl
	poolID string
	params cwParams
	logf   func(string, ...any)

	ten         tenant
	homeVersion string
	drv         *pgxDriver
	java        *sampleapp.App
	javaWhy     string
	results     []cwResult
}

func runClientWindow(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("requires a kube context for the control-plane namespace: the operations are driven against the endpoint's own pods")
		return nil
	}
	params := cwParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	r := &clientWindowRun{
		t: t, cl: cl, env: env, kube: kubeEnv.AmpKube(), poolID: env.AmpPoolID(),
		params: params, logf: logf,
	}
	t.SetPerfIdentity(identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsClientWindow(params, cwClients(params.JavaClient))).core())

	td := &teardownStack{}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(ctx, logf)
	}()
	defer r.cleanup()

	if err := t.Step("create_tenant", r.createTenant); err != nil {
		return err
	}
	if err := t.Step("start_clients", func(s *core.StepHandle) error { return r.startClients(s, td) }); err != nil {
		return err
	}

	driverCtx, stopDriver := context.WithCancel(t.Context)
	defer stopDriver()
	go r.drv.run(driverCtx)

	for _, op := range r.operations() {
		if err := r.measure(op); err != nil {
			return err
		}
	}
	stopDriver()

	if err := t.Step("check_java_client", r.checkJava); err != nil {
		return err
	}
	return t.Step("gate_client_window", r.gate)
}

func (r *clientWindowRun) cleanup() {
	if r.drv != nil {
		r.drv.releasePinned()
	}
	if r.ten.Conn != nil {
		r.ten.Conn.Close()
		r.ten.Conn = nil
	}
	if r.ten.ClusterID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := r.cl.DeleteClusterAndWait(ctx, r.poolID, r.ten.ClusterID); err != nil {
		r.logf("client_window: could not delete cluster %s: %v", r.ten.ClusterID, err)
	}
}

func (r *clientWindowRun) createTenant(s *core.StepHandle) error {
	mt := multitenantParams{EndpointCPU: r.params.EndpointCPU, EndpointMemory: r.params.EndpointMem}
	ten, _, err := createTenant(r.t, r.cl, r.env, r.poolID, 0, mt)
	if err != nil {
		return fmt.Errorf("%w%s", err, schedulingHint(r.t, r.logf))
	}
	r.ten = ten
	s.Set("cluster", ten.ClusterID)
	s.Set("endpoint", ten.Endpoint.ID)
	_, err = ten.Conn.Exec(r.t.Context, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id bigserial PRIMARY KEY, client text NOT NULL, at timestamptz NOT NULL, mark text)", cwTable))
	return err
}

func (r *clientWindowRun) startClients(s *core.StepHandle, td *teardownStack) error {
	period := time.Second / time.Duration(r.params.RateTPS)
	r.drv = newPGXDriver(r.ten.Conn.Pool(), cwTable, period, r.params.StatementTimeout, r.logf)
	if err := r.drv.preparePinned(r.t.Context); err != nil {
		return err
	}
	s.Set("rate_tps", r.params.RateTPS)
	s.Set("pgx_clients", "pool + pinned session with a prepared statement")

	if !r.params.JavaClient {
		r.javaWhy = "disabled by perf.client_window.java_client"
		s.Set("java_client", r.javaWhy)
		return nil
	}
	if err := r.startJava(td); err != nil {
		r.javaWhy = err.Error()
		r.logf("client_window: WARN the Java client is not running (%v); the JDBC error classes will not be checked", err)
	}
	s.Set("java_client", orNone(r.javaWhy))
	return nil
}

func (r *clientWindowRun) startJava(td *teardownStack) error {
	ctx := r.t.Context
	cfg := perfFromConfig(r.t.Options)
	sink := attachSink{t: r.t, logf: r.logf}
	podExec, err := startPerfClientPod(ctx, r.kube, sink, td,
		"ampqa-cw-"+r.t.ResourceSuffix(), cfg.ClientImage, cfg.ClientPullSecret)
	if err != nil {
		return err
	}
	jarPath, err := deliverSampleAppJar(ctx, r.t, cfg, podExec)
	if err != nil {
		return err
	}
	conn, err := authedEndpointConn(ctx, r.kube, r.ten.Endpoint.ID, r.ten.Endpoint.Address, r.logf)
	if err != nil {
		return err
	}
	props := sampleAppProps(mixSqlTransactions, conn)
	props["metrics_trace_timeout"] = fmt.Sprintf("%d", r.params.SampleInterval.Milliseconds())
	app := sampleapp.New(podExec, sampleapp.RunSpec{
		JarPath:         jarPath,
		Workload:        sampleapp.SqlInserts,
		Hosts:           []string{conn.Host},
		YSQLPort:        conn.Port,
		NumTables:       1,
		NumThreadsRead:  1,
		NumThreadsWrite: 1,
		Marker:          "cw-" + r.t.ResourceSuffix(),
		Props:           props,
	}, sampleapp.WithLogf(r.logf), sampleapp.WithLoadMarker("ops/sec"),
		sampleapp.WithReadyTimeout(r.params.RecoverTimeout))
	if err := app.Start(ctx); err != nil {
		return err
	}
	td.push("report-kill the Java client", func(ctx context.Context) error { return app.ReportKill(ctx, sink) })
	if err := app.WaitReady(ctx); err != nil {
		return fmt.Errorf("the Java client never started generating load: %w", err)
	}
	r.java = app
	return nil
}

func deliverSampleAppJar(ctx context.Context, t *core.T, cfg perfConfig, podExec exec.RemoteExecutor) (string, error) {
	specs, err := artifact.Resolve(t.Options)
	if err != nil {
		return "", fmt.Errorf("resolve artifacts: %w", err)
	}
	spec, declared := specs["sampleapp"]
	if !declared {
		return "", fmt.Errorf("artifacts.sampleapp is not declared")
	}
	secrets, err := core.NewSecrets(t.Options)
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	jarSecret, err := secrets.Value(sampleAppJarSecret(t.Options))
	if err != nil {
		return "", fmt.Errorf("resolve the sample-app jar secret: %w", err)
	}
	dep := &artifact.Deployer{}
	if err := dep.Deploy(ctx, []exec.RemoteExecutor{podExec}, spec,
		artifact.DeployOptions{RemoteDir: cfg.RemoteDir, Secret: jarSecret}); err != nil {
		return "", fmt.Errorf("deploy the sample-app jar: %w", err)
	}
	return path.Join(cfg.RemoteDir, spec.GlobPath), nil
}

type cwOp struct {
	Name   string
	Skip   string
	Settle time.Duration
	Run    func(ctx context.Context) (time.Time, error)
}

func (r *clientWindowRun) operations() []cwOp {
	var ops []cwOp
	add := func(op cwOp) {
		if r.params.wants(op.Name) {
			ops = append(ops, op)
		}
	}
	add(cwOp{Name: cwComputeRestart, Run: r.restartCompute})
	add(cwOp{Name: cwResize, Run: r.resize})
	add(cwOp{Name: cwSettingsUpdate, Run: r.settingsUpdate})
	if r.params.wants(cwUpgradeRoll) {
		ops = append(ops,
			cwOp{Name: cwUpgradeRoll, Run: r.upgradeRollAway},
			cwOp{Name: cwUpgradeRollBack, Run: r.upgradeRollBack})
	}
	if r.params.wants(cwPauseResume) {
		op := cwOp{Name: cwPauseResume, Run: r.pauseResume}
		if !r.params.PauseResume {
			op.Skip = "the controller has no pause/resume routes yet (meko#518); perf.lifecycle.pause_resume turns this on once it does"
		}
		ops = append(ops, op)
	}
	add(cwOp{Name: cwBackingRoll, Run: r.backingRoll, Settle: r.params.BackingSettle})
	return ops
}

func (r *clientWindowRun) measure(op cwOp) error {
	return r.t.Step("op_"+op.Name, func(s *core.StepHandle) error {
		if op.Skip != "" {
			s.Set("skipped", op.Skip)
			r.logf("client_window: %s skipped: %s", op.Name, op.Skip)
			return nil
		}
		if err := sleepCtx(r.t.Context, r.params.Hold); err != nil {
			return err
		}
		mark := fmt.Sprintf("%s-%s", op.Name, r.t.ResourceSuffix())
		tx, err := beginIdleTx(r.t.Context, r.ten.Conn.Pool(), cwTable, mark)
		if err != nil {
			return err
		}

		issued := time.Now()
		start, err := op.Run(r.t.Context)
		end := time.Now()
		if start.IsZero() {
			start = issued
		}
		if err != nil {
			tx.commit(r.t.Context, r.params.StatementTimeout)
			return fmt.Errorf("%s: %w", op.Name, err)
		}

		if err := r.waitHealthy(); err != nil {
			tx.commit(r.t.Context, r.params.StatementTimeout)
			return fmt.Errorf("after %s: %w", op.Name, err)
		}
		txOut := tx.commit(r.t.Context, r.params.StatementTimeout)
		if err := verifyIdleTxRow(r.t.Context, r.ten.Conn, cwTable, txOut, mark); err != nil {
			return err
		}
		s.Set("idle_tx", idleTxSummary(txOut))
		if !txOut.Committed && txOut.Class == errClassOther {
			return fmt.Errorf("the idle transaction spanning %s failed with an unlicensed error (%s %s): %w",
				op.Name, txOut.Class, txOut.SQLState, txOut.Err)
		}

		prepared, err := r.drv.checkPrepared(r.t.Context)
		if err != nil {
			return fmt.Errorf("after %s: %w", op.Name, err)
		}
		s.Set("prepared_statement", string(prepared))

		if err := sleepCtx(r.t.Context, op.Settle+r.params.Grace+r.params.Hold); err != nil {
			return err
		}
		res, err := r.evaluate(op.Name, start, end, op.Settle)
		if err != nil {
			return err
		}
		res.Prepared = prepared
		r.results = append(r.results, res)
		s.Set("errors", res.Errors)
		s.Set("error_seconds", fmt.Sprintf("%.1f", res.ErrorSeconds))
		s.Set("first_success_after_seconds", fmt.Sprintf("%.1f", res.FirstSuccessAfter))
		return nil
	})
}

func (r *clientWindowRun) waitHealthy() error {
	return r.t.WaitFor("the endpoint answers again", r.params.RecoverTimeout, time.Second, func() (bool, error) {
		ctx, cancel := context.WithTimeout(r.t.Context, r.params.StatementTimeout)
		defer cancel()
		_, err := r.ten.Conn.QueryInt(ctx, "SELECT 1")
		return err == nil, nil
	})
}

func idleTxSummary(out idleTxOutcome) string {
	switch {
	case out.Committed:
		return "committed"
	case out.answered():
		return fmt.Sprintf("refused (%s %s)", out.Class, out.SQLState)
	default:
		return fmt.Sprintf("unresolved (%s)", out.Class)
	}
}

// Grace 0 closes the window at the very first success, which is how the red proof is produced.
func (r *clientWindowRun) evaluate(op string, start, opEnd time.Time, settle time.Duration) (cwResult, error) {
	res := cwResult{Op: op, Start: start, End: opEnd, TaskSeconds: opEnd.Sub(start).Seconds()}
	events := r.drv.snapshot()

	from := start
	if opEnd.After(from) {
		from = opEnd
	}
	recovered, ok := recoveryPoint(events, from, r.params.Grace)
	if !ok {
		return res, fmt.Errorf("%s: the pooled client never held a connection for %s after the operation, so the endpoint did not come back",
			op, r.params.Grace)
	}
	res.FirstSuccessAfter = recovered.Sub(start).Seconds()
	windowEnd := recovered.Add(settle)

	var firstErr, lastErr time.Time
	for _, ev := range events {
		if ev.ok() || ev.At.Before(start) || ev.At.After(windowEnd) {
			continue
		}
		if ev.Class == errClassRelay {
			res.Relay++
			continue
		}
		if ev.Class == errClassOther {
			return res, unlicensed(op, ev)
		}
		res.Errors++
		if firstErr.IsZero() {
			firstErr = ev.At
		}
		lastErr = ev.At
		switch ev.Class {
		case errClassTransport:
			res.Transport++
		case errClassTimeout:
			res.Timeouts++
		}
	}
	if !firstErr.IsZero() {
		res.ErrorSeconds = lastErr.Sub(firstErr).Seconds()
	}

	quietFrom := start.Add(-r.params.Hold)
	quietUntil := windowEnd.Add(r.params.Grace)
	for _, ev := range events {
		if ev.ok() || ev.At.Before(quietFrom) {
			continue
		}
		if ev.At.Before(start) {
			return res, fmt.Errorf("%s: a client errored %.1fs BEFORE the operation started, so the endpoint was not healthy going in: %s",
				op, start.Sub(ev.At).Seconds(), ev.describe())
		}
		if ev.At.After(quietUntil) {
			return res, fmt.Errorf("%s: a client errored %.1fs after the window closed (restored %s, settle %s, grace %s): %s",
				op, ev.At.Sub(windowEnd).Seconds(), recovered.Format(time.RFC3339), settle, r.params.Grace, ev.describe())
		}
		if ev.Class == errClassOther {
			return res, unlicensed(op, ev)
		}
	}
	r.logf("client_window: %s: %d error(s) over %.1fs, service restored %.1fs after the task started, %d relay gap(s), prepared statement %s",
		op, res.Errors, res.ErrorSeconds, res.FirstSuccessAfter, res.Relay, res.Prepared)
	return res, nil
}

func unlicensed(op string, ev probeEvent) error {
	return fmt.Errorf("%s: a client saw an error the operation does not license, want a SQLSTATE in class 57 or 08, a severed socket or a statement timeout: %s",
		op, ev.describe())
}

func recoveryPoint(events []probeEvent, from time.Time, grace time.Duration) (time.Time, bool) {
	for i, ev := range events {
		if ev.At.Before(from) || !ev.ok() || ev.Client != clientPooled {
			continue
		}
		clean := true
		for _, later := range events[i+1:] {
			if later.At.After(ev.At.Add(grace)) {
				break
			}
			if !later.ok() {
				clean = false
				break
			}
		}
		if clean {
			return ev.At, true
		}
	}
	return time.Time{}, false
}

func (r *clientWindowRun) async(ctx context.Context, call func() (controller.Task, string, error)) (time.Time, error) {
	issued := time.Now()
	task, loc, err := call()
	if err != nil {
		return issued, err
	}
	done, err := r.cl.WaitTask(ctx, controller.TaskRef(task, loc))
	if err != nil {
		return issued, err
	}
	start := done.CreatedAt
	if start.IsZero() || start.Before(issued.Add(-time.Minute)) || start.After(time.Now()) {
		return issued, nil
	}
	return start, nil
}

// Graceful, so Postgres gets to send 57P01 rather than dropping the socket under a statement.
func (r *clientWindowRun) restartCompute(ctx context.Context) (time.Time, error) {
	pod, err := r.runningComputePod(ctx)
	if err != nil {
		return time.Time{}, core.EnvFail("endpoint %s has no compute pod to restart: %v", r.ten.Endpoint.ID, err)
	}
	start := time.Now()
	if err := r.kube.Delete(ctx, "pod", pod); err != nil {
		return start, err
	}
	if err := ampstack.WaitEndpointWorkloadReady(ctx, r.kube, r.ten.Endpoint.ID, r.params.RecoverTimeout); err != nil {
		return start, err
	}
	return start, nil
}

// A Terminating or Pending pod is the wrong one to delete: the window would close on a workload that was never disturbed.
func (r *clientWindowRun) runningComputePod(ctx context.Context) (string, error) {
	selector := "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(r.ten.Endpoint.ID)
	names, err := r.kube.GetSelector(ctx, "pod", selector, "{.items[*].metadata.name}")
	if err != nil {
		return "", err
	}
	phases, err := r.kube.GetSelector(ctx, "pod", selector, "{.items[*].status.phase}")
	if err != nil {
		return "", err
	}
	nf, pf := strings.Fields(names), strings.Fields(phases)
	for i := range nf {
		if i < len(pf) && pf[i] == "Running" {
			return nf[i], nil
		}
	}
	return "", fmt.Errorf("no Running pod matches %q", selector)
}

func (r *clientWindowRun) resize(ctx context.Context) (time.Time, error) {
	return r.async(ctx, func() (controller.Task, string, error) {
		return r.cl.ResizeEndpoint(ctx, r.poolID, r.ten.ClusterID, r.ten.BranchID, r.ten.Endpoint.ID,
			controller.ResizeEndpointRequest{Resources: controller.EndpointResources{
				CPU: r.params.ResizeCPU, Memory: r.params.ResizeMem}})
	})
}

func (r *clientWindowRun) settingsUpdate(ctx context.Context) (time.Time, error) {
	start := time.Now()
	settings := map[string]string{cwRestartSettingName: cwRestartSettingValue}
	out, err := r.cl.UpdateEndpointSettings(ctx, r.poolID, r.ten.ClusterID, r.ten.BranchID, r.ten.Endpoint.ID,
		controller.UpdateSettingsRequest{PgSettings: &settings})
	if err != nil {
		return start, err
	}
	for _, ep := range out.Endpoints {
		if len(ep.Restarted) > 0 {
			return start, nil
		}
	}
	return start, fmt.Errorf("%s=%s restarted nothing (%+v), so this step measured no window",
		cwRestartSettingName, cwRestartSettingValue, out.Endpoints)
}

// The version has to change for the pod to move: a roll onto the version already recorded re-renders an identical release and helm leaves the pod alone.
//
// Which is why this pair cannot run against a controller that pins the endpoint
// version to its own release: both rolls would name that one version, helm would
// leave the pod where it is, and the step would report a client window of zero
// that no client ever saw.
func (r *clientWindowRun) upgradeRollAway(ctx context.Context) (time.Time, error) {
	ep, err := r.cl.GetEndpoint(ctx, r.poolID, r.ten.ClusterID, r.ten.BranchID, r.ten.Endpoint.ID)
	if err != nil {
		return time.Time{}, err
	}
	if ep.Version == "" {
		return time.Time{}, fmt.Errorf("this controller records no endpoint version, so there is no second image to roll to")
	}
	portal := strings.TrimRight(r.t.Options.GetString("report.portal.url", ""), "/")
	if portal == "" {
		return time.Time{}, core.EnvFail("no report portal to resolve the previous release from (report.portal.url is empty)")
	}
	list, err := releases.New(portal).WithLogf(r.logf).List(ctx, 50)
	if err != nil {
		return time.Time{}, core.EnvFail("read the release list from %q: %w", portal, err)
	}
	prev, err := cwPreviousRelease(list, ep.Version)
	if err != nil {
		return time.Time{}, core.EnvFail("%w", err)
	}
	if pinned, ok := r.cl.RollTargetIsPinned(ctx); ok && pinned {
		r.t.Skip("this controller pins the endpoint version to its own release, so a roll cannot move an endpoint between two versions and this step would measure no window")
		return time.Time{}, nil
	}
	r.homeVersion = ep.Version
	r.logf("client_window: rolling endpoint %s from %s to %s and back", ep.ID, ep.Version, prev.Tag)
	return r.roll(ctx, prev.Tag)
}

func (r *clientWindowRun) upgradeRollBack(ctx context.Context) (time.Time, error) {
	if r.homeVersion == "" {
		return time.Time{}, fmt.Errorf("the roll away never recorded the version to return to")
	}
	return r.roll(ctx, r.homeVersion)
}

func (r *clientWindowRun) roll(ctx context.Context, target string) (time.Time, error) {
	return r.async(ctx, func() (controller.Task, string, error) {
		return r.cl.UpgradeEndpoints(ctx, controller.UpgradeEndpointsRequest{
			Version: target, EndpointIDs: []string{r.ten.Endpoint.ID}, MaxParallel: 1})
	})
}

func (r *clientWindowRun) pauseResume(ctx context.Context) (time.Time, error) {
	start, err := r.async(ctx, func() (controller.Task, string, error) {
		return r.cl.PauseEndpoint(ctx, r.poolID, r.ten.ClusterID, r.ten.BranchID, r.ten.Endpoint.ID)
	})
	if err != nil {
		return start, err
	}
	_, err = r.async(ctx, func() (controller.Task, string, error) {
		return r.cl.ResumeEndpoint(ctx, r.poolID, r.ten.ClusterID, r.ten.BranchID, r.ten.Endpoint.ID)
	})
	return start, err
}

func (r *clientWindowRun) backingRoll(ctx context.Context) (time.Time, error) {
	yu, ok := r.t.Env.(interface {
		YBAUniverse() (universe.YBAMaintainer, string)
	})
	if !ok {
		return time.Time{}, fmt.Errorf("this env exposes no YBA universe")
	}
	maintainer, universeID := yu.YBAUniverse()
	if maintainer == nil || universeID == "" {
		return time.Time{}, fmt.Errorf("the backing is not a YBA universe, so it cannot be rolled through YBA")
	}
	start := time.Now()
	return start, maintainer.RestartUniverseAndWait(ctx, universeID, yba.UpgradeRolling, r.params.BackingRollSleep)
}

func (r *clientWindowRun) checkJava(s *core.StepHandle) error {
	if r.java == nil {
		s.Set("skipped", orNone(r.javaWhy))
		return nil
	}
	logText, err := captureAppLog(r.t.Context, r.java)
	captured := time.Now()
	if err != nil {
		s.Set("skipped", "the Java client produced no readable log: "+err.Error())
		r.logf("client_window: WARN %v", err)
		return nil
	}
	r.t.AttachBytes("java-client.log", "text/plain", []byte(logText))
	if len(r.results) == 0 {
		return nil
	}
	anchor, ok := javaLogAnchor(logText)
	if !ok {
		s.Set("skipped", "the Java client's log carries no timestamps to place its exceptions in a window")
		return nil
	}
	offset := captured.Sub(anchor)
	errs := parseJavaErrors(logText, offset)
	s.Set("exceptions", len(errs))
	for i := range r.results {
		res := &r.results[i]
		windowEnd := res.Start.Add(time.Duration(res.FirstSuccessAfter * float64(time.Second)))
		if res.End.After(windowEnd) {
			windowEnd = res.End
		}
		n := 0
		for _, e := range errs {
			if e.At.IsZero() || e.At.Before(res.Start) || e.At.After(windowEnd.Add(r.params.Grace)) {
				continue
			}
			n++
			if e.Class == errClassOther && e.SQLState != "" {
				return fmt.Errorf("%s: the JDBC client saw SQLSTATE %s, which the operation does not license: %s",
					res.Op, e.SQLState, e.Line)
			}
		}
		s.Set(res.Op+"_java_errors", n)
		r.t.Metric(cwPrefix+res.Op+".java_errors", float64(n), "count")
	}
	return nil
}

func (r *clientWindowRun) gate(s *core.StepHandle) error {
	if len(r.results) == 0 {
		return fmt.Errorf("no operation was measured, so there is nothing to gate")
	}
	var gated, info []core.Datapoint
	secFloor := r.params.StatementTimeout.Seconds()
	for _, res := range r.results {
		gated = append(gated,
			core.Datapoint{Name: cwPrefix + res.Op + ".error_seconds", Value: res.ErrorSeconds, Unit: "s", Direction: core.LowerIsBetter, MinDelta: secFloor},
			core.Datapoint{Name: cwPrefix + res.Op + ".first_success_after_seconds", Value: res.FirstSuccessAfter, Unit: "s", Direction: core.LowerIsBetter, MinDelta: secFloor},
			core.Datapoint{Name: cwPrefix + res.Op + ".errors_total", Value: float64(res.Errors), Unit: "count", Direction: core.LowerIsBetter},
		)
		info = append(info,
			core.Datapoint{Name: cwPrefix + res.Op + ".transport_errors", Value: float64(res.Transport), Unit: "count", Direction: core.LowerIsBetter},
			core.Datapoint{Name: cwPrefix + res.Op + ".timeouts", Value: float64(res.Timeouts), Unit: "count", Direction: core.LowerIsBetter},
			core.Datapoint{Name: cwPrefix + res.Op + ".relay_errors", Value: float64(res.Relay), Unit: "count", Direction: core.LowerIsBetter},
			core.Datapoint{Name: cwPrefix + res.Op + ".operation_seconds", Value: res.TaskSeconds, Unit: "s", Direction: core.LowerIsBetter},
		)
	}
	s.Set("operations", len(r.results))
	s.Set("datapoints", len(gated))
	r.t.Chart(cwChart(r.results))
	id := identityFromConfig(r.t.Options, "", frameworkCommit(), loadFieldsClientWindow(r.params, cwClients(r.java != nil)))
	r.t.SetPerfIdentity(id.core())
	return emitAndGateAuto(r.t, perfFromConfig(r.t.Options), scaleVersion(r.t), testNameClientWindow, gated, info,
		id, r.logf)
}

func cwChart(results []cwResult) core.Chart {
	var errSeries, recoverySeries []core.ChartPoint
	for i, res := range results {
		errSeries = append(errSeries, core.ChartPoint{X: float64(i + 1), Y: res.ErrorSeconds})
		recoverySeries = append(recoverySeries, core.ChartPoint{X: float64(i + 1), Y: res.FirstSuccessAfter})
	}
	names := make([]string, 0, len(results))
	for _, res := range results {
		names = append(names, res.Op)
	}
	return core.Chart{
		Title:  "Client window per operation (" + strings.Join(names, ", ") + ")",
		Kind:   core.ChartBar,
		XLabel: "operation", YLabel: "seconds",
		Series: []core.ChartSeries{
			{Name: "errors, first to last", Points: errSeries},
			{Name: "first success after the task started", Points: recoverySeries},
		},
	}
}

func loadFieldsClientWindow(p cwParams, clients int) LoadFields {
	return LoadFields{
		Script: fmt.Sprintf("client_window:%s@%s/%s/rate%d/grace%s/hold%s/stmt%s/settle%s",
			strings.Join(p.Operations, "+"), p.EndpointCPU, p.EndpointMem,
			p.RateTPS, p.Grace, p.Hold, p.StatementTimeout, p.BackingSettle),
		Clients: clients,
		Harness: cwHarnessRev,
	}
}

func cwClients(javaRunning bool) int {
	if javaRunning {
		return 3
	}
	return 2
}
