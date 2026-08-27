// Shared plumbing for the multi-tenant stress tests: standing N tenants up on
// one control plane, driving a writer against each, and proving afterwards that
// the deployment is still standing.
//
// These tests ask "does amp keep working while many tenants hammer it?" — load
// actually moved, every tenant made progress, the endpoints still serve, the
// control plane still answers and can still create a tenant, and nothing in the
// namespace is crash-looping. They deliberately do NOT audit the data: what a
// tenant should hold after faults is a correctness question that needs its own
// test to answer properly, and mixing the two here would only produce
// unreliable accusations from a load test.
//
// A tenant here is what a tenant is in the product: its own cluster, its own
// branch, its own endpoint. That is the only arrangement worth stressing —
// N tables in one database would exercise Postgres, not amp.
package stress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// ampTenantEnv is the standing control plane these tests need. Declared
// structurally, so any env exposing a control plane satisfies it without the
// stress package importing the features flow.
type ampTenantEnv interface {
	AmpController(ctx context.Context) (*controller.Client, error)
	AmpPoolID() string
	ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error)
}

// ampKubeEnv is the extra capability the chaos test needs: the namespace where
// endpoint compute pods live, so faults can be aimed at amp itself.
type ampKubeEnv interface {
	AmpKube() *k8s.Kubectl
}

const tenantRole = "read_write"

// Tenant endpoints are sized explicitly and small. An unsized endpoint takes
// the product's default reservation, which is generous enough that a handful of
// tenants exhaust a shared test cluster — the deploys then sit Pending until the
// controller's deploy timeout expires, and the run fails for capacity rather
// than for anything the test was asking about.
const (
	tenantEndpointCPU    = "500m"
	tenantEndpointMemory = "1Gi"
)

// tenant is one tenant and the state its verification needs.
type tenant struct {
	Index     int
	Name      string
	ClusterID string
	BranchID  string
	Endpoint  controller.Endpoint
	Conn      *pg.Client
	Table     string

	// Written is how many DISTINCT ids this tenant's writer was acknowledged
	// for — the lower bound the verification holds storage to. It and the three
	// counters below are written by the tenant's writer goroutine and may be
	// READ while it runs (live progress sampling), so all four are touched
	// atomically.
	Written int64
	// Attempted is how many ids it issued, acknowledged or not — the difference
	// against Written is how much of the run the client spent in trouble.
	Attempted int64
	// WriteErrors and Reconnects record how hostile the run was to this tenant's
	// client. A tenant that committed nothing because its connection died is a
	// different finding from one its neighbours starved, and without these two
	// numbers the fairness gate cannot tell them apart.
	WriteErrors int64
	Reconnects  int64
	// nextID is the writer's cursor. It persists ACROSS load phases: a test that
	// loads, injects faults and loads again must not reissue the ids of the first
	// phase, or every re-insert is a no-op the writer still counts as a new row
	// and the verification reports invented data loss.
	nextID int64
}

// Namespaces one tenant's ids; a shared range misattributes data loss.
const tenantIDSpan = 1_000_000_000

// Not safe for concurrent callers: one tenant has one writer.
func (ten *tenant) nextRowID() int64 {
	id := int64(ten.Index)*tenantIDSpan + ten.nextID
	ten.nextID++
	return id
}

// tenantFleet owns the tenants' lifecycle: create them, hand them out, and
// delete them whatever happened.
type tenantFleet struct {
	cl     *controller.Client
	poolID string
	env    ampTenantEnv
	logf   func(string, ...any)
	// EndpointCPU and EndpointMemory override the small per-tenant reservation
	// below. Empty means the default: a fleet of many tenants has to fit on a
	// shared cluster, but a test running ONE tenant through large writes needs a
	// compute sized for them.
	EndpointCPU    string
	EndpointMemory string
	Tenants        []tenant
}

func newTenantFleet(t *core.T, env ampTenantEnv, logf func(string, ...any)) (*tenantFleet, error) {
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return nil, err
	}
	return &tenantFleet{cl: cl, poolID: env.AmpPoolID(), env: env, logf: logf}, nil
}

// Create stands n MORE tenants up, each with its own cluster, endpoint and
// table. Indices continue from the tenants already in the fleet: a later call
// must not reuse a name or an id range, or the second tenant would write into
// the first one's keyspace and the verification would blame the wrong one. It
// stops at the first failure, leaving whatever it built for Close.
func (f *tenantFleet) Create(t *core.T, n int) error {
	first := len(f.Tenants)
	for i := first; i < first+n; i++ {
		name := fmt.Sprintf("mtl-%s-%d", t.ResourceSuffix(), i)
		if _, err := f.cl.CreateClusterAndWait(t.Context, f.poolID, controller.CreateClusterRequest{Name: name}); err != nil {
			return fmt.Errorf("tenant %d: create cluster: %w", i, err)
		}
		cluster, err := f.findCluster(t.Context, name)
		if err != nil {
			return fmt.Errorf("tenant %d: %w", i, err)
		}
		// Registered before the endpoint exists: the cluster is already there, so
		// a failure creating its endpoint must still delete the cluster.
		f.Tenants = append(f.Tenants, tenant{
			Index: i, Name: name, ClusterID: cluster.ID,
			BranchID: cluster.DefaultBranch.BranchID, Table: fmt.Sprintf("mt_%d", i),
		})
		if _, err := f.cl.CreateEndpointAndWait(t.Context, f.poolID, cluster.ID, cluster.DefaultBranch.BranchID,
			controller.CreateEndpointRequest{
				Role:      tenantRole,
				Resources: f.endpointResources(),
			}); err != nil {
			return fmt.Errorf("tenant %d: create endpoint: %w", i, err)
		}
		eps, err := f.cl.ListAllEndpoints(t.Context, f.poolID, cluster.ID, cluster.DefaultBranch.BranchID)
		if err != nil {
			return fmt.Errorf("tenant %d: list endpoints: %w", i, err)
		}
		if len(eps) == 0 {
			return fmt.Errorf("tenant %d: the branch lists no endpoint after a successful create", i)
		}
		conn, err := f.env.ConnectEndpoint(t.Context, eps[len(eps)-1])
		if err != nil {
			return fmt.Errorf("tenant %d: connect endpoint: %w", i, err)
		}
		cur := &f.Tenants[len(f.Tenants)-1]
		cur.Endpoint = eps[len(eps)-1]
		cur.Conn = conn
		if _, err := conn.Exec(t.Context, fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)", cur.Table)); err != nil {
			return fmt.Errorf("tenant %d: create table: %w", i, err)
		}
	}
	return nil
}

// endpointResources is the reservation every tenant endpoint is created with.
func (f *tenantFleet) endpointResources() *controller.EndpointResources {
	res := &controller.EndpointResources{CPU: tenantEndpointCPU, Memory: tenantEndpointMemory}
	if f.EndpointCPU != "" {
		res.CPU = f.EndpointCPU
	}
	if f.EndpointMemory != "" {
		res.Memory = f.EndpointMemory
	}
	return res
}

// Close deletes every tenant on its own deadline — the test's context is often
// already cancelled by the time teardown runs.
func (f *tenantFleet) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	for i := len(f.Tenants) - 1; i >= 0; i-- {
		if f.Tenants[i].Conn != nil {
			f.Tenants[i].Conn.Close()
		}
		if _, err := f.cl.DeleteClusterAndWait(ctx, f.poolID, f.Tenants[i].ClusterID); err != nil {
			f.logf("multitenant: could not delete tenant %d's cluster %s: %v", i, f.Tenants[i].ClusterID, err)
		}
	}
}

func (f *tenantFleet) findCluster(ctx context.Context, name string) (controller.Cluster, error) {
	list, err := f.cl.ListClusters(ctx, f.poolID, 500, 0)
	if err != nil {
		return controller.Cluster{}, err
	}
	for i := len(list.Clusters) - 1; i >= 0; i-- {
		if list.Clusters[i].Name == name {
			return list.Clusters[i], nil
		}
	}
	return controller.Cluster{}, fmt.Errorf("cluster %q is absent after a successful create", name)
}

// writeFor drives one tenant's writer until the context ends, recording what it
// committed. Rows carry a payload so the run moves real bytes rather than just
// row counts, and every value is folded into a checksum.
func writeFor(ctx context.Context, env ampTenantEnv, ten *tenant, payloadBytes int) {
	// A real client reconnects. Without this a broken connection turns the
	// tenant into a silent no-op writer, and the fairness gate then reports it as
	// starved by its neighbours — blaming the product for a dead port-forward.
	// It matters most under chaos, where the pod behind the connection is being
	// deleted on purpose.
	const reconnectAfterErrors = 5
	consecutive := 0
	for ctx.Err() == nil {
		id := ten.nextRowID()
		atomic.AddInt64(&ten.Attempted, 1)
		// v is a pure function of id, so every stored row can be checked for
		// corruption without knowing which ids were acknowledged.
		// $3 is cast explicitly: a bare parameter arrives as "unknown", and
		// repeat(text, integer) cannot resolve against it.
		_, err := ten.Conn.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
			id, id*7, payloadBytes)
		if err == nil {
			consecutive = 0
			atomic.AddInt64(&ten.Written, 1)
			continue
		}
		// A cancelled context ends the phase. Any other error leaves this id in
		// doubt — it may well have committed — so it counts as attempted but
		// never as acknowledged.
		if ctx.Err() != nil {
			return
		}
		atomic.AddInt64(&ten.WriteErrors, 1)
		consecutive++
		if consecutive < reconnectAfterErrors {
			continue
		}
		conn, rerr := env.ConnectEndpoint(ctx, ten.Endpoint)
		if rerr != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		ten.Conn.Close()
		ten.Conn = conn
		atomic.AddInt64(&ten.Reconnects, 1)
		consecutive = 0
	}
}

// runWriters drives every tenant concurrently for d.
func runWriters(ctx context.Context, env ampTenantEnv, tenants []tenant, d time.Duration, payloadBytes int) {
	loadCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(1)
		go func(ten *tenant) {
			defer wg.Done()
			writeFor(loadCtx, env, ten, payloadBytes)
		}(&tenants[i])
	}
	wg.Wait()
}

// assertEndpointsServe checks every tenant's endpoint still answers SQL. A
// fresh connection per tenant, because the writers' own connections have been
// open across the whole run and a dead one says nothing about the endpoint.
func assertEndpointsServe(t *core.T, env ampTenantEnv, tenants []tenant, settle time.Duration) error {
	deadline := time.Now().Add(settle)
	for i := range tenants {
		var lastErr error
		for {
			conn, err := env.ConnectEndpoint(t.Context, tenants[i].Endpoint)
			if err == nil {
				_, err = conn.QueryInt(t.Context, "SELECT 1")
				conn.Close()
			}
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if t.Context.Err() != nil {
				return t.Context.Err()
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if lastErr != nil {
			return fmt.Errorf("tenant %d's endpoint does not serve SQL within %s: %w", i, settle, lastErr)
		}
	}
	return nil
}

// awaitControlPlaneReady polls /readyz to the caller's settle budget. The
// controller stores its state in the backing YugabyteDB, so chaos there leaves
// it unready, out of its Service, and refusing connections for minutes. The
// client is re-resolved every round: a controller address that is a pod IP dies
// with the pod, and polling the dead one for the whole budget reads as a hang.
func awaitControlPlaneReady(t *core.T, fleet *tenantFleet, settle time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(settle)
	for {
		cl, err := fleet.env.AmpController(t.Context)
		if err == nil {
			if err = cl.Readyz(t.Context); err == nil {
				return time.Since(start), nil
			}
		}
		if t.Context.Err() != nil {
			return time.Since(start), t.Context.Err()
		}
		if time.Now().After(deadline) {
			return time.Since(start), fmt.Errorf("the control plane is still not ready %s after the chaos stopped: %w", settle, err)
		}
		time.Sleep(5 * time.Second)
	}
}

// assertAmpHealthy reports pods in the control-plane namespace that are not
// running and ready — a crash-looping controller or endpoint is exactly the
// "amp stopped working" this tier is here to catch. Pods that completed
// (Succeeded) are ignored: a finished job is not a failure.
func assertAmpHealthy(t *core.T, kube *k8s.Kubectl, tenants []tenant) (string, error) {
	// No kube context means this check did not run. Reporting success for it
	// is how a "the deployment still stands" step passes without looking at
	// the deployment at all.
	if kube == nil {
		return "", core.AsEnvFailure(fmt.Errorf("no kube context: pod health could not be checked, so this run cannot claim the deployment survived"))
	}
	states, err := kube.PodStates(t.Context)
	if err != nil {
		return "", fmt.Errorf("read pod states: %w", err)
	}
	var unhealthy []string
	for _, p := range states {
		if p.Phase == "Succeeded" {
			continue
		}
		// Another fleet's endpoints churn as their owner runs; their health is
		// their owner's assertion, not ours.
		if foreignEndpointPod(p.Name, tenants) {
			continue
		}
		if p.Phase != "Running" || !p.Ready {
			entry := fmt.Sprintf("%s (%s, ready=%v)", p.Name, p.Phase, p.Ready)
			if why := kube.PodTrouble(t.Context, p.Name); why != "" {
				entry += ": " + why
			}
			unhealthy = append(unhealthy, entry)
		}
	}
	if len(unhealthy) > 0 {
		return "", fmt.Errorf("%d pod(s) in the amp namespace are not running and ready after the load: %s",
			len(unhealthy), strings.Join(unhealthy, ", "))
	}
	return fmt.Sprintf("%d pod(s) running and ready", len(states)), nil
}

// silentTenant names a tenant that committed nothing, with the evidence that
// says why: a client that lost its endpoint rather than a tenant its neighbours
// crowded out. Returns "" when every tenant managed at least one write.
func silentTenant(tenants []tenant) string {
	for i := range tenants {
		if tenants[i].Written == 0 {
			return fmt.Sprintf("tenant %d (%s) never committed a row: %d write error(s), %d reconnect(s) over %d attempted id(s) — its endpoint was unusable for the whole phase",
				i, tenants[i].Name, tenants[i].WriteErrors, tenants[i].Reconnects, tenants[i].Attempted)
		}
	}
	return ""
}

// fairnessOf is the least productive tenant's share of the most productive one.
func fairnessOf(tenants []tenant) float64 {
	if len(tenants) == 0 {
		return 0
	}
	lo, hi := tenants[0].Written, tenants[0].Written
	for _, ten := range tenants {
		lo = min(lo, ten.Written)
		hi = max(hi, ten.Written)
	}
	if hi == 0 {
		return 0
	}
	return float64(lo) / float64(hi)
}

// totalWritten sums the acknowledged writes. It reads atomically because a
// maintenance run samples it WHILE the writers are still going, to attribute
// commits to the operation they landed during.
func totalWritten(tenants []tenant) int64 {
	var n int64
	for i := range tenants {
		n += atomic.LoadInt64(&tenants[i].Written)
	}
	return n
}

// Per-probe deadline: a never-returning COMMIT reads as a hang (meko#727).
const (
	writeProbeTimeout  = 15 * time.Second
	writeProbeInterval = 3 * time.Second
)

// A lapsed write token restarts Postgres, so a window opened at fault-stop times that.
func awaitWritePath(ctx context.Context, env ampTenantEnv, tenants []tenant, settle time.Duration, payloadBytes int) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(settle)
	pending := make([]*tenant, 0, len(tenants))
	for i := range tenants {
		pending = append(pending, &tenants[i])
	}
	for {
		var still []*tenant
		var lastErr error
		for _, ten := range pending {
			if err := probeWrite(ctx, env, ten, payloadBytes); err != nil {
				still = append(still, ten)
				lastErr = err
			}
		}
		if len(still) == 0 {
			return time.Since(start), nil
		}
		pending = still
		if ctx.Err() != nil {
			return time.Since(start), ctx.Err()
		}
		if time.Now().After(deadline) {
			ten := pending[0]
			return time.Since(start), fmt.Errorf(
				"%d of %d tenant(s) could not commit a single row within %s of the backing coming back; tenant %d (%s) is one of them: %d write error(s), %d reconnect(s) over %d attempted id(s): %w",
				len(pending), len(tenants), settle, ten.Index, ten.Name,
				atomic.LoadInt64(&ten.WriteErrors), atomic.LoadInt64(&ten.Reconnects),
				atomic.LoadInt64(&ten.Attempted), lastErr)
		}
		time.Sleep(writeProbeInterval)
	}
}

// One row on the writers' own id scheme, so verify_data cannot tell it apart.
func probeWrite(ctx context.Context, env ampTenantEnv, ten *tenant, payloadBytes int) error {
	if ten.Conn == nil {
		conn, err := env.ConnectEndpoint(ctx, ten.Endpoint)
		if err != nil {
			return fmt.Errorf("connect to the endpoint: %w", err)
		}
		ten.Conn = conn
		atomic.AddInt64(&ten.Reconnects, 1)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, writeProbeTimeout)
	defer cancel()
	id := ten.nextRowID()
	atomic.AddInt64(&ten.Attempted, 1)
	_, err := ten.Conn.Exec(attemptCtx, fmt.Sprintf(
		"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
		id, id*7, payloadBytes)
	if err == nil {
		atomic.AddInt64(&ten.Written, 1)
		return nil
	}
	atomic.AddInt64(&ten.WriteErrors, 1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The connection is the likely casualty, not the endpoint: reconnect next probe.
	ten.Conn.Close()
	ten.Conn = nil
	if attemptCtx.Err() != nil {
		return fmt.Errorf("the write did not return within %s, so the COMMIT is hung rather than refused: %w", writeProbeTimeout, err)
	}
	return err
}
