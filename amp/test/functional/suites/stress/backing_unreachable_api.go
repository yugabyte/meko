// What the API answers while the store under it is unreachable
// (ac-apis.md §Layering, meko#886). backing_nemesis's probe watches one read
// route and only counts non-503 5xx; this test drives reads AND writes on their
// own transport, holds every answer to the documented contract — a 503 carries
// Retry-After, a write never answers 500, a 2xx is honest — and then proves the
// window left no half-written state: a create is 202 + a Task, so every task the
// API planned settled and the cluster matches its outcome, and every create it
// refused left no row at all.
package stress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_unreachable_api",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress", "gapcoverage"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Delete the backing YugabyteDB's serving pods, then drive reads and cluster creates against the controller on a transport that retries nothing: every answer must be a 2xx, a 4xx, or a 503 carrying Retry-After — never a 500 and never a hang (meko#886). Once the backing is back, every create task the API planned must have settled, a succeeded one leaving a cluster that lists and takes a branch and a failed one leaving nothing in the default listing, every create it refused must have left no cluster row, and the API must answer normally again.",
		Params:      []string{"stress.backing_unreachable_api", "chaos"},
		Timeout:     75 * time.Minute,
		Run:         runBackingUnreachableAPI,
	})
}

type unreachableAPIParams struct {
	// FaultDuration is how long calls are driven with the backing pods deleted.
	FaultDuration time.Duration
	SettleTimeout time.Duration
	// CallInterval paces one read plus one create per tick.
	CallInterval time.Duration
	// MaxCallLatency is when a single call stops counting as slow and starts
	// counting as hung. Deliberately generous: how fast the controller notices a
	// departed backing is a number this test reports, not one it invents a bar
	// for. Only a call that never comes back inside it fails.
	MaxCallLatency time.Duration
}

func unreachableAPIParamsFromConfig(c *core.Config) unreachableAPIParams {
	return unreachableAPIParams{
		FaultDuration:  c.GetDuration("stress.backing_unreachable_api.fault_duration", 90*time.Second),
		SettleTimeout:  c.GetDuration("stress.backing_unreachable_api.settle_timeout", 10*time.Minute),
		CallInterval:   c.GetDuration("stress.backing_unreachable_api.call_interval", 2*time.Second),
		MaxCallLatency: c.GetDuration("stress.backing_unreachable_api.max_call_latency", 120*time.Second),
	}
}

// acceptedSettleBudget bounds waiting the fault window's create tasks out, so a
// task that never settles costs this rather than the whole test's 75m.
const acceptedSettleBudget = 15 * time.Minute

// unreachableCleanupBudget bounds deleting the clusters the fault window
// created, on its own deadline.
const unreachableCleanupBudget = 20 * time.Minute

func runBackingUnreachableAPI(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: there is nothing to take away without it")
		return nil
	}
	params := unreachableAPIParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()
	caller := newRawCaller(cl, poolID, params.MaxCallLatency)
	defer caller.Close(t.Context, cl, logf)

	if err := t.Step("baseline_api", func(s *core.StepHandle) error {
		r := caller.list(t.Context)
		if r.status != http.StatusOK {
			return fmt.Errorf("the API answers %s before any fault was injected: %s", r, r.body)
		}
		c := caller.createCluster(t.Context, "unreach-base-"+t.ResourceSuffix())
		if c.status != http.StatusAccepted {
			return fmt.Errorf("a cluster create answers %s on a healthy control plane: %s", c, c.body)
		}
		s.Set("baseline", "ok")
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("api_while_backing_gone", func(s *core.StepHandle) error {
		kube := backingEnv.BackingKube()
		pods, err := backingPods(t, kube)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return fmt.Errorf("no backing pod matched any known selector in namespace %q", kube.Namespace())
		}
		s.Set("targets", strings.Join(pods, ","))
		for _, pod := range pods {
			if err := kube.DeleteNow(t.Context, pod); err != nil {
				logf("delete backing pod %s: %v", pod, err)
			}
		}
		caller.drive(t.Context, t.ResourceSuffix(), params.FaultDuration, params.CallInterval)
		tally := caller.tally()
		s.Set("answers", tally)
		logf("answers while the backing was gone: %s", tally)
		// How long the slowest call took to be told the store is gone. Reported
		// rather than gated: it is the number an operator wants when the API feels
		// unresponsive during a backing incident, and a threshold for it belongs in
		// a baseline, not in an assertion invented here.
		t.Metric("slowest_call_seconds", caller.slowestCall().Seconds(), "s")
		if err := caller.verdict(); err != nil {
			return err
		}
		if !caller.sawStoreDown() {
			logf("no 503 was sampled: the pods came back inside the window, or the connection pool never noticed them go")
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("backing_recovered", func(s *core.StepHandle) error {
		waited, err := awaitBackingReady(t, backingEnv.BackingKube(), params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		led, err := awaitBackingTabletsLed(t, backingEnv.BackingKube(), params.SettleTimeout, logf)
		switch {
		case errors.Is(err, errNoTabletView):
			s.Set("tablets_led_after", "skipped: "+err.Error())
			logf("tablet-leader gate skipped: %v", err)
		case err != nil:
			return err
		default:
			s.Set("tablets_led_after", led.Round(time.Second).String())
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("no_half_written_state", func(s *core.StepHandle) error {
		if err := awaitAPIReady(t.Context, caller, params.SettleTimeout); err != nil {
			return err
		}
		accepted, refused, err := caller.reconcile(t.Context, cl, poolID, env)
		if err != nil {
			return err
		}
		s.Set("accepted_present", accepted)
		s.Set("refused_absent", refused)
		return nil
	})
}

// rawAnswer is one call's outcome on a transport that retries nothing.
type rawAnswer struct {
	status     int
	retryAfter string
	location   string
	transport  error
	took       time.Duration
	body       string
}

func (a rawAnswer) String() string {
	if a.transport != nil {
		return fmt.Sprintf("transport error after %s: %v", a.took.Round(time.Millisecond), a.transport)
	}
	return fmt.Sprintf("HTTP %d after %s", a.status, a.took.Round(time.Millisecond))
}

// rawCaller drives the API over the controller client's transport but its own
// requests: Client.doJSON retries a 503 for UnavailableBudget, which is right
// for a caller and wrong for a test that has to see the 503 itself.
type rawCaller struct {
	hc         *http.Client
	apiURL     string
	poolID     string
	maxLatency time.Duration

	mu       sync.Mutex
	counts   map[string]int
	slowest  time.Duration
	failures []string
	// accepted/refused name the clusters a create was answered 2xx / 4xx for.
	accepted []acceptedCreate
	refused  []string
}

func newRawCaller(cl *controller.Client, poolID string, maxLatency time.Duration) *rawCaller {
	return &rawCaller{
		hc:         cl.HTTPClient(),
		apiURL:     cl.BaseURL() + "/api/v1",
		poolID:     poolID,
		maxLatency: maxLatency,
		counts:     map[string]int{},
	}
}

func (c *rawCaller) do(ctx context.Context, method, url string, body []byte) rawAnswer {
	rctx, cancel := context.WithTimeout(ctx, c.maxLatency)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(rctx, method, url, rdr)
	if err != nil {
		return rawAnswer{transport: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := c.hc.Do(req)
	took := time.Since(start)
	if err != nil {
		return rawAnswer{transport: err, took: took}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return rawAnswer{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		location:   resp.Header.Get("Location"),
		took:       took,
		body:       strings.TrimSpace(string(data)),
	}
}

func (c *rawCaller) list(ctx context.Context) rawAnswer {
	return c.do(ctx, http.MethodGet, c.apiURL+"/storage-pools/"+c.poolID+"/clusters?limit=1", nil)
}

// A create is 202 + a Task: whether the store kept the cluster is that task's
// outcome, never the status line.
type acceptedCreate struct {
	name   string
	taskID string
}

func taskIDOf(a rawAnswer) string {
	var t controller.Task
	_ = json.Unmarshal([]byte(a.body), &t)
	return controller.TaskRef(t, a.location)
}

func (c *rawCaller) createCluster(ctx context.Context, name string) rawAnswer {
	body, _ := json.Marshal(map[string]any{"name": name})
	a := c.do(ctx, http.MethodPost, c.apiURL+"/storage-pools/"+c.poolID+"/clusters", body)
	c.mu.Lock()
	switch {
	case a.status >= 200 && a.status < 300:
		c.accepted = append(c.accepted, acceptedCreate{name: name, taskID: taskIDOf(a)})
	case a.status >= 400 && a.status < 500:
		c.refused = append(c.refused, name)
	}
	c.mu.Unlock()
	return a
}

// drive runs one read and one create per tick and records every answer.
func (c *rawCaller) drive(ctx context.Context, suffix string, d, interval time.Duration) {
	deadline := time.Now().Add(d)
	for i := 0; time.Now().Before(deadline); i++ {
		c.record("read", c.list(ctx))
		c.record("create", c.createCluster(ctx, fmt.Sprintf("unreach-%s-%d", suffix, i)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// record holds one answer to the contract: a 5xx other than 503 is a bug, a 503
// without Retry-After is a bug, and a call that outlasts maxLatency is a hang.
func (c *rawCaller) record(kind string, a rawAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.took > c.slowest {
		c.slowest = a.took
	}
	switch {
	case a.transport != nil:
		// A dead port-forward is the harness losing its route to the controller,
		// not the controller failing to answer.
		if core.IsEnvFailure(a.transport) {
			c.counts[kind+":env_error"]++
			return
		}
		c.counts[kind+":transport_error"]++
		if strings.Contains(a.transport.Error(), "context deadline exceeded") {
			c.failures = append(c.failures, fmt.Sprintf("a %s call never came back inside %s with the backing gone; an unreachable store has to be reported, and a client that waits this long has hung", kind, a.took.Round(time.Second)))
		}
		return
	case a.status == http.StatusServiceUnavailable:
		c.counts[kind+":503"]++
		if _, err := strconv.Atoi(strings.TrimSpace(a.retryAfter)); err != nil {
			c.failures = append(c.failures, fmt.Sprintf("a %s call answered 503 with Retry-After %q; the spec makes the header required so a client knows how long to wait", kind, a.retryAfter))
		}
		return
	case a.status >= 500:
		c.counts[fmt.Sprintf("%s:%d", kind, a.status)]++
		c.failures = append(c.failures, fmt.Sprintf("a %s call answered %d while the backing was unreachable, which tells a client the controller is at fault and its request must not be retried: %s", kind, a.status, truncate(a.body, 300)))
		return
	}
	c.counts[fmt.Sprintf("%s:%d", kind, a.status)]++
}

func (c *rawCaller) tally() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.counts)+1)
	for k, n := range c.counts {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	parts = append(parts, "slowest="+c.slowest.Round(time.Millisecond).String())
	return strings.Join(parts, " ")
}

// slowestCall is the longest a single call took during the fault window.
func (c *rawCaller) slowestCall() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slowest
}

func (c *rawCaller) verdict() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.failures) == 0 {
		return nil
	}
	return fmt.Errorf("%d answer(s) broke the unreachable-store contract (meko#886): %s", len(c.failures), strings.Join(dedupe(c.failures), "; "))
}

func (c *rawCaller) sawStoreDown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, n := range c.counts {
		if strings.HasSuffix(k, ":503") && n > 0 {
			return true
		}
		if strings.HasSuffix(k, ":transport_error") && n > 0 {
			return true
		}
	}
	return false
}

// Close deletes every cluster this caller created. Its own budget, rooted off
// the run's context: the whole point is that it still runs when the test failed
// because that context is done.
func (c *rawCaller) Close(ctx context.Context, cl *controller.Client, logf func(string, ...any)) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unreachableCleanupBudget)
	defer cancel()
	names, err := clusterIDsByName(cctx, cl, c.poolID)
	if err != nil {
		logf("cleanup: list clusters (best-effort): %v", err)
		return
	}
	c.mu.Lock()
	created := append([]acceptedCreate{}, c.accepted...)
	c.mu.Unlock()
	for _, acc := range created {
		if id := names[acc.name]; id != "" {
			if _, err := cl.DeleteClusterAndWait(cctx, c.poolID, id); err != nil {
				logf("cleanup: delete cluster %s (best-effort): %v", id, err)
			}
		}
	}
}

type acceptedOutcome struct {
	name      string
	taskID    string
	status    string
	inDefault bool
}

// A failed create keeps its row for an operator until the GC sweeper takes it,
// so the default listing is what it must be absent from (ac-tasks.md).
func judgeAccepted(o acceptedOutcome) error {
	switch o.status {
	case controller.TaskSucceeded:
		if !o.inDefault {
			return fmt.Errorf("cluster %q is not in the listing although its create task %s succeeded; the store kept nothing for a write it reported done", o.name, o.taskID)
		}
	case controller.TaskFailed:
		if o.inDefault {
			return fmt.Errorf("cluster %q is in the default listing although its create task %s failed: pickers and counts must not see a half-provisioned cluster", o.name, o.taskID)
		}
	default:
		return fmt.Errorf("the create task %s of cluster %q is still %q after the backing recovered: every planned task must reach a terminal state", o.taskID, o.name, o.status)
	}
	return nil
}

func (c *rawCaller) settleAccepted(ctx context.Context, cl *controller.Client, accepted []acceptedCreate) ([]acceptedOutcome, error) {
	wctx, cancel := context.WithTimeout(ctx, acceptedSettleBudget)
	defer cancel()
	out := make([]acceptedOutcome, 0, len(accepted))
	for _, acc := range accepted {
		if acc.taskID == "" {
			return nil, fmt.Errorf("the create of cluster %q was answered 2xx but named no task, so its outcome cannot be read: a 202 must carry the Task it planned", acc.name)
		}
		// WaitTask reports a failed task as an error WITH the task, which is the
		// outcome this step reads rather than a failure of its own.
		done, err := cl.WaitTask(wctx, acc.taskID)
		if err != nil && done.Status == "" {
			return nil, fmt.Errorf("await the create task %s of cluster %q: %w", acc.taskID, acc.name, err)
		}
		out = append(out, acceptedOutcome{name: acc.name, taskID: acc.taskID, status: done.Status})
	}
	return out, nil
}

// reconcile holds the API's answers to what the store actually kept: every
// create the API PLANNED settled, and the cluster it left behind matches that
// task's outcome; every refused one left nothing.
func (c *rawCaller) reconcile(ctx context.Context, cl *controller.Client, poolID string, env ampTenantEnv) (int, int, error) {
	c.mu.Lock()
	accepted := append([]acceptedCreate{}, c.accepted...)
	refused := append([]string{}, c.refused...)
	c.mu.Unlock()

	outcomes, err := c.settleAccepted(ctx, cl, accepted)
	if err != nil {
		return 0, 0, err
	}
	// Listed only once every task settled: a running create would read as lost.
	names, err := clusterIDsByName(ctx, cl, poolID)
	if err != nil {
		return 0, 0, err
	}
	visible, err := clusterIDsIn(ctx, cl, poolID, false)
	if err != nil {
		return 0, 0, err
	}
	for _, name := range refused {
		if id := names[name]; id != "" {
			return 0, 0, fmt.Errorf("cluster %q exists as %s although its create was refused with a 4xx: a refused write must leave nothing behind", name, id)
		}
	}
	present := 0
	var succeeded []string
	for _, o := range outcomes {
		o.inDefault = visible[o.name] != ""
		if err := judgeAccepted(o); err != nil {
			return 0, 0, err
		}
		if o.status == controller.TaskSucceeded {
			succeeded = append(succeeded, o.name)
			present++
		}
	}
	// One accepted cluster is exercised end to end: a cluster row that exists
	// while its branch or recipe does not lists clean and fails here, and nothing
	// in the listing can tell the two apart. A failure is not proof of that, since
	// a leaderless backing tablet fails it too (meko#1366).
	if len(succeeded) > 0 {
		if err := clusterIsUsable(ctx, cl, env, poolID, visible[succeeded[len(succeeded)-1]]); err != nil {
			return 0, 0, err
		}
	}
	return present, len(refused), nil
}

// clusterIsUsable deploys a writer on the cluster's default branch and queries
// through it.
func clusterIsUsable(ctx context.Context, cl *controller.Client, env ampTenantEnv, poolID, clusterID string) error {
	cluster, err := cl.GetCluster(ctx, poolID, clusterID)
	if err != nil {
		return fmt.Errorf("read back cluster %s created during the fault window: %w", clusterID, err)
	}
	if cluster.DefaultBranch.BranchID == "" {
		return fmt.Errorf("cluster %s created during the fault window has no default branch", clusterID)
	}
	if _, err := cl.CreateEndpointAndWait(ctx, poolID, clusterID, cluster.DefaultBranch.BranchID,
		controller.CreateEndpointRequest{Role: "read_write", Resources: &controller.EndpointResources{CPU: tenantEndpointCPU, Memory: tenantEndpointMemory}}); err != nil {
		return fmt.Errorf("a cluster created while the backing was unreachable could not take a compute once it was back: %w", err)
	}
	eps, err := cl.ListAllEndpoints(ctx, poolID, clusterID, cluster.DefaultBranch.BranchID)
	if err != nil || len(eps) == 0 {
		return fmt.Errorf("cluster %s lists no endpoint after a successful create (%v)", clusterID, err)
	}
	conn, err := env.ConnectEndpoint(ctx, eps[len(eps)-1])
	if err != nil {
		return fmt.Errorf("connect the compute on cluster %s: %w", clusterID, err)
	}
	defer conn.Close()
	if _, err := conn.QueryInt(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the compute on cluster %s does not answer: %w", clusterID, err)
	}
	return nil
}

// awaitAPIReady waits for the API to answer a read 200 again.
func awaitAPIReady(ctx context.Context, c *rawCaller, settle time.Duration) error {
	deadline := time.Now().Add(settle)
	var last rawAnswer
	for time.Now().Before(deadline) {
		last = c.list(ctx)
		if last.status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("the API never answered a read 200 in the %s after the backing came back: last %s %s", settle, last, truncate(last.body, 200))
}

func clusterIDsByName(ctx context.Context, cl *controller.Client, poolID string) (map[string]string, error) {
	return clusterIDsIn(ctx, cl, poolID, true)
}

func clusterIDsIn(ctx context.Context, cl *controller.Client, poolID string, includeFailed bool) (map[string]string, error) {
	out := map[string]string{}
	const page = 100
	for offset := 0; ; offset += page {
		var list controller.ClusterList
		var err error
		if includeFailed {
			list, err = cl.ListClustersIncludingFailed(ctx, poolID, page, offset)
		} else {
			list, err = cl.ListClusters(ctx, poolID, page, offset)
		}
		if err != nil {
			return nil, fmt.Errorf("list clusters: %w", err)
		}
		for _, c := range list.Clusters {
			out[c.Name] = c.ID
		}
		if list.Count < page || offset+len(list.Clusters) >= list.Total {
			return out, nil
		}
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
