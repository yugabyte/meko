package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const liveBranchCleanupBudget = 20 * time.Minute

type liveEndpoint struct {
	role string
	ep   controller.Endpoint
	conn *pg.Client
	// unknownSessions accumulates over every pod the endpoint ran in: a deleted
	// pod's log is gone, so each one is read before it is rolled.
	unknownSessions int
	firstSeen       string
}

// liveBranchRun is one cluster whose default branch serves a writer, with the
// rows it acknowledged tracked as a count and a sum. No read replica: one never
// finishes creating while its writer holds the branch write token (meko#833).
type liveBranchRun struct {
	t            *core.T
	env          ampTenantEnv
	cl           *controller.Client
	kube         *k8s.Kubectl
	logf         func(string, ...any)
	poolID       string
	readyTimeout time.Duration
	cluster      controller.Cluster
	eps          []*liveEndpoint
	rows         int64
	sum          int64
	geomReady    bool
	cleanups     []func(context.Context)
}

func newLiveBranchRun(t *core.T, readyTimeout time.Duration) (*liveBranchRun, bool, error) {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		return nil, false, nil
	}
	ak, ok := t.Env.(ampKubeEnv)
	if !ok || ak.AmpKube() == nil {
		return nil, false, nil
	}
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return nil, true, err
	}
	return &liveBranchRun{
		t: t, env: env, cl: cl, kube: ak.AmpKube(), logf: t.Masker().Logf(log.Printf),
		poolID: env.AmpPoolID(), readyTimeout: readyTimeout,
	}, true, nil
}

func (r *liveBranchRun) steps(steps ...liveStep) error {
	for _, st := range steps {
		if err := r.t.Step(st.name, st.fn); err != nil {
			return r.withEvidence(err)
		}
	}
	return nil
}

// withEvidence attaches every endpoint pod's logs before cleanup deletes the
// cluster, and names any `unknown session` they carry in the failure. An
// endpoint whose create failed is in no list the test kept, so pods are found
// by name.
func (r *liveBranchRun) withEvidence(err error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pods, lerr := r.kube.ListPods(ctx)
	if lerr != nil {
		r.logf("live branch: list pods for evidence: %v", lerr)
		return err
	}
	for _, pod := range pods {
		if !strings.HasPrefix(pod, "yb-ep-") {
			continue
		}
		logs, _ := r.kube.LogsAll(ctx, "pod/"+pod, 0)
		prev, _ := r.kube.LogsAllPrevious(ctx, "pod/"+pod, 0)
		desc, _ := r.kube.Describe(ctx, "pod", pod)
		r.t.AttachBytes(pod+".log", "log", []byte(logs+"\n--- previous container ---\n"+prev))
		r.t.AttachBytes(pod+".describe.txt", "log", []byte(desc))
		n, first := 0, ""
		for _, line := range strings.Split(logs+"\n"+prev, "\n") {
			if strings.Contains(strings.ToLower(line), "unknown session") {
				n++
				if first == "" {
					first = strings.TrimSpace(line)
				}
			}
		}
		if n > 0 && !strings.Contains(err.Error(), pod) {
			err = fmt.Errorf("%w (pod %s logged %d `unknown session`, meko#1493; first: %s)", err, pod, n, first)
		}
	}
	return err
}

type liveStep struct {
	name string
	fn   func(*core.StepHandle) error
}

func (r *liveBranchRun) createLiveBranch(s *core.StepHandle, prefix string, rows int) error {
	ctx := r.t.Context
	name := prefix + "-" + r.t.ResourceSuffix()
	if _, err := r.cl.CreateClusterAndWait(ctx, r.poolID, controller.CreateClusterRequest{Name: name}); err != nil {
		return fmt.Errorf("create cluster %s: %w", name, err)
	}
	list, err := r.cl.ListClusters(ctx, r.poolID, 500, 0)
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	for _, c := range list.Clusters {
		if c.Name == name {
			r.cluster = c
		}
	}
	if r.cluster.ID == "" {
		return fmt.Errorf("cluster %q is absent after a successful create", name)
	}
	branch := r.cluster.DefaultBranch.BranchID
	if _, err := r.cl.CreateEndpointAndWait(ctx, r.poolID, r.cluster.ID, branch, controller.CreateEndpointRequest{
		Role:      "read_write",
		Resources: &controller.EndpointResources{CPU: tenantEndpointCPU, Memory: tenantEndpointMemory},
	}); err != nil {
		return fmt.Errorf("create the writer endpoint: %w", err)
	}
	eps, err := r.cl.ListAllEndpoints(ctx, r.poolID, r.cluster.ID, branch)
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	for _, ep := range eps {
		r.eps = append(r.eps, &liveEndpoint{role: ep.Role, ep: ep})
	}
	if r.writer() == nil {
		return fmt.Errorf("the branch lists %d endpoints and no read_write one after creating the writer", len(eps))
	}
	if err := r.connectAll(); err != nil {
		return err
	}
	if _, err := r.writer().conn.Exec(ctx, "CREATE TABLE qa_places (id bigint PRIMARY KEY, lon double precision NOT NULL, lat double precision NOT NULL, v bigint NOT NULL)"); err != nil {
		return fmt.Errorf("create qa_places on the writer: %w", err)
	}
	if err := r.insert(rows); err != nil {
		return err
	}
	s.Set("cluster", r.cluster.ID)
	s.Set("rows", r.rows)
	return nil
}

// restartCycles rolls every endpoint pod, then reads and writes on the cold
// computes. check runs on each endpoint after every cycle.
func (r *liveBranchRun) restartCycles(s *core.StepHandle, cycles, rowsPerCycle int, check func(*liveEndpoint) error) error {
	ctx := r.t.Context
	var slowest time.Duration
	for cycle := 1; cycle <= cycles; cycle++ {
		r.closeConns()
		for _, le := range r.eps {
			start := time.Now()
			pod, err := ampstack.EndpointPodName(ctx, r.kube, le.ep.ID)
			if err != nil {
				return fmt.Errorf("cycle %d: find the %s endpoint pod: %w", cycle, le.role, err)
			}
			r.scanLogs(le, pod)
			if err := r.kube.Delete(ctx, "pod", pod); err != nil {
				return fmt.Errorf("cycle %d: delete the %s endpoint pod %s: %w", cycle, le.role, pod, err)
			}
			if err := ampstack.WaitEndpointWorkloadReady(ctx, r.kube, le.ep.ID, r.readyTimeout); err != nil {
				return fmt.Errorf("cycle %d: the %s endpoint never came back: %w", cycle, le.role, err)
			}
			if d := time.Since(start); d > slowest {
				slowest = d
			}
		}
		if err := r.connectAll(); err != nil {
			return fmt.Errorf("cycle %d: %w", cycle, err)
		}
		if err := r.verifyRows(r.writer()); err != nil {
			return fmt.Errorf("cycle %d, cold writer: %w", cycle, err)
		}
		if err := r.insert(rowsPerCycle); err != nil {
			return fmt.Errorf("cycle %d: %w", cycle, err)
		}
		for _, le := range r.eps {
			if err := r.verifyRows(le); err != nil {
				return fmt.Errorf("cycle %d: %w", cycle, err)
			}
			if check != nil {
				if err := check(le); err != nil {
					return fmt.Errorf("cycle %d: %w", cycle, err)
				}
			}
		}
		r.logf("live branch: cycle %d/%d ok, %d rows", cycle, cycles, r.rows)
	}
	r.t.Metric("slowest_endpoint_restart_seconds", slowest.Seconds(), "s")
	s.Set("cycles", cycles)
	s.Set("rows", r.rows)
	return nil
}

func (r *liveBranchRun) noUnknownSession(s *core.StepHandle) error {
	var bad []string
	for _, le := range r.eps {
		if pod, err := ampstack.EndpointPodName(r.t.Context, r.kube, le.ep.ID); err == nil {
			r.scanLogs(le, pod)
		}
		s.Set(le.role+"_unknown_sessions", le.unknownSessions)
		if le.unknownSessions > 0 {
			bad = append(bad, fmt.Sprintf("%s endpoint %s: %d, first: %s", le.role, le.ep.ID, le.unknownSessions, le.firstSeen))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("the thin client hit `unknown session`, a session reaching a tserver that never issued it (meko#1493): %s", strings.Join(bad, "; "))
	}
	return nil
}

func (r *liveBranchRun) scanLogs(le *liveEndpoint, pod string) {
	out, err := r.kube.LogsAll(r.t.Context, "pod/"+pod, 0)
	if err != nil {
		r.logf("live branch: read logs of %s: %v", pod, err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), "unknown session") {
			le.unknownSessions++
			if le.firstSeen == "" {
				le.firstSeen = strings.TrimSpace(line)
			}
		}
	}
}

func (r *liveBranchRun) hostPorts(le *liveEndpoint) (string, error) {
	sel := "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(le.ep.ID)
	out, err := r.kube.GetSelector(r.t.Context, "configmap", sel, "{.items[*].data.YB_HOST_PORTS}")
	if err != nil {
		return "", fmt.Errorf("read YB_HOST_PORTS of the %s endpoint: %w", le.role, err)
	}
	return strings.TrimSpace(out), nil
}

// insert is one statement: a batch is acknowledged whole or not at all, so the
// expected count and sum never drift from what the writer committed.
func (r *liveBranchRun) insert(n int) error {
	from := r.rows
	cols, vals := "id, lon, lat, v", "g, (g % 360) - 180, (g % 170) - 85, g * 7"
	if r.geomReady {
		cols += ", geom"
		vals += ", ST_SetSRID(ST_MakePoint((g % 360) - 180, (g % 170) - 85), 4326)"
	}
	last := from + int64(n) - 1
	sql := fmt.Sprintf("INSERT INTO qa_places (%s) SELECT %s FROM generate_series($1::bigint, $2::bigint) g", cols, vals)
	if _, err := r.writer().conn.Exec(r.t.Context, sql, from, last); err != nil {
		return fmt.Errorf("insert ids %d..%d on the writer (%d acknowledged before): %w", from, last, r.rows, err)
	}
	r.rows += int64(n)
	r.sum += 7 * (from + last) * int64(n) / 2
	return nil
}

func (r *liveBranchRun) verifyRows(le *liveEndpoint) error {
	var rows, sum int64
	if err := le.conn.Query(r.t.Context, func(rs pgx.Rows) error {
		return rs.Scan(&rows, &sum)
	}, "SELECT count(*), coalesce(sum(v), 0)::bigint FROM qa_places"); err != nil {
		return fmt.Errorf("read qa_places on the %s endpoint: %w", le.role, err)
	}
	if rows != r.rows || sum != r.sum {
		return fmt.Errorf("the %s endpoint reads %d rows summing to %d, want the %d acknowledged rows summing to %d", le.role, rows, sum, r.rows, r.sum)
	}
	return nil
}

func (r *liveBranchRun) connectAll() error {
	for _, le := range r.eps {
		c, err := r.env.ConnectEndpoint(r.t.Context, le.ep)
		if err != nil {
			return fmt.Errorf("connect the %s endpoint %s: %w", le.role, le.ep.ID, err)
		}
		le.conn = c
	}
	return nil
}

func (r *liveBranchRun) closeConns() {
	for _, le := range r.eps {
		if le.conn != nil {
			le.conn.Close()
			le.conn = nil
		}
	}
}

func (r *liveBranchRun) writer() *liveEndpoint {
	for _, le := range r.eps {
		if le.role == "read_write" {
			return le
		}
	}
	return nil
}

// onCleanup runs after the cluster delete, whether or not it succeeded, in
// registration order.
func (r *liveBranchRun) onCleanup(fn func(context.Context)) { r.cleanups = append(r.cleanups, fn) }

func (r *liveBranchRun) cleanup() {
	r.closeConns()
	ctx, cancel := context.WithTimeout(context.Background(), liveBranchCleanupBudget)
	defer cancel()
	if r.cluster.ID != "" {
		if _, err := r.cl.DeleteClusterAndWait(ctx, r.poolID, r.cluster.ID); err != nil {
			r.logf("live branch: delete cluster %s: %v", r.cluster.ID, err)
		}
	}
	for _, fn := range r.cleanups {
		fn(ctx)
	}
}
