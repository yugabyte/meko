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
	// for — the lower bound the verification holds storage to.
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

// tenantFleet owns the tenants' lifecycle: create them, hand them out, and
// delete them whatever happened.
type tenantFleet struct {
	cl      *controller.Client
	poolID  string
	env     ampTenantEnv
	logf    func(string, ...any)
	Tenants []tenant
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
				Resources: &controller.EndpointResources{CPU: tenantEndpointCPU, Memory: tenantEndpointMemory},
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
	base := int64(ten.Index) * 1_000_000_000
	// A real client reconnects. Without this a broken connection turns the
	// tenant into a silent no-op writer, and the fairness gate then reports it as
	// starved by its neighbours — blaming the product for a dead port-forward.
	// It matters most under chaos, where the pod behind the connection is being
	// deleted on purpose.
	const reconnectAfterErrors = 5
	consecutive := 0
	for ctx.Err() == nil {
		id := base + ten.nextID
		ten.nextID++
		ten.Attempted++
		// v is a pure function of id, so every stored row can be checked for
		// corruption without knowing which ids were acknowledged.
		// $3 is cast explicitly: a bare parameter arrives as "unknown", and
		// repeat(text, integer) cannot resolve against it.
		_, err := ten.Conn.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
			id, id*7, payloadBytes)
		if err == nil {
			consecutive = 0
			ten.Written++
			continue
		}
		// A cancelled context ends the phase. Any other error leaves this id in
		// doubt — it may well have committed — so it counts as attempted but
		// never as acknowledged.
		if ctx.Err() != nil {
			return
		}
		ten.WriteErrors++
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
		ten.Reconnects++
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

// assertAmpHealthy reports pods in the control-plane namespace that are not
// running and ready — a crash-looping controller or endpoint is exactly the
// "amp stopped working" this tier is here to catch. Pods that completed
// (Succeeded) are ignored: a finished job is not a failure.
func assertAmpHealthy(t *core.T, kube *k8s.Kubectl) (string, error) {
	if kube == nil {
		return "no kube context: pod health not checked", nil
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
		if p.Phase != "Running" || !p.Ready {
			unhealthy = append(unhealthy, fmt.Sprintf("%s (%s, ready=%v)", p.Name, p.Phase, p.Ready))
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

func totalWritten(tenants []tenant) int64 {
	var n int64
	for _, ten := range tenants {
		n += ten.Written
	}
	return n
}
