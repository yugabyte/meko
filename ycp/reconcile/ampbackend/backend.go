// Package ampbackend implements the reconcile engine's Backend against the amp
// controller's REST API. It is the first (and, until meko folds in, only)
// backend: the engine stays kind-agnostic and this package holds all the amp
// knowledge — how the flat desired-state kinds map onto amp's pool -> cluster ->
// branch -> endpoint hierarchy, and how each verb becomes a controller call.
//
// Desired resources are named by hierarchical path so that (kind, name) is
// unique within an environment even though amp only requires names to be unique
// within a parent (every cluster has a "main" branch):
//
//	storage-pool  "<pool>"
//	cluster       "<pool>/<cluster>"
//	branch        "<pool>/<cluster>/<branch>"
//	endpoint      "<pool>/<cluster>/<branch>/<endpoint>"
//
// The path yields both the parent (for the engine's ordering/gating) and, at
// apply time, the parent's server-assigned UUIDs — resolved from the index
// List() builds — which amp needs in the request path but YCP desired state
// (names only) cannot carry.
//
// A resource's spec is the matching amp create request (a pool's owner_user_id,
// a cluster's pg_version/extensions, a branch's parent_branch_id/fork_point_lsn/
// pg_settings, an endpoint's role/resources). A branch spec also takes
// extensions — the externalized extensions every endpoint on it mounts — and
// write_token_settings, but only once the branch exists: amp applies each with a
// separate call against a branch id no create has produced yet, so Create refuses
// a spec that declares either rather than dropping the field.
//
// Settings are the other post-create surface. A branch's pg_settings and an
// endpoint's pg_settings/pageserver_settings/safekeeper_settings converge through
// the two settings PATCHes, and a declared surface owns the whole map: what the
// spec says the surface IS is what the row is set to, so a key stored there and
// not declared is removed. Omitting a surface leaves it unmanaged. Each is
// reported back in Attrs as JSON, because a caller that cannot see the map it is
// about to replace cannot declare one without deleting keys it never saw.
package ampbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// Resource kinds an operator declares in desired state for an amp environment.
const (
	KindPool       = "storage-pool"
	KindCluster    = "cluster"
	KindBranch     = "branch"
	KindEndpoint   = "endpoint"
	KindController = "controller"
	// KindFleetVersion is the version the controller's endpoint fleet should run
	// (fleet.go).
	KindFleetVersion = "fleet-version"
)

// controllerName is the only name a controller resource may carry: one
// environment runs one amp controller, so a second name would be two
// declarations driving one helm release.
const controllerName = "controller"

const sep = "/"

// Backend drives one amp controller. Safe for concurrent use within a tick:
// List rebuilds the id index under lock, and the apply verbs only read it.
type Backend struct {
	api *ampclient.Client

	mu  sync.RWMutex
	idx index
	// inflight is the task the last update for a resource planned, so the next tick
	// can ask the controller how it went instead of guessing. In-process and lost on
	// restart, which is safe because losing it only costs the verdict, never the
	// convergence: an update whose generation the controller has not recorded is
	// re-issued regardless, and a task still holding the resource answers 409, which
	// this path reads as pending.
	inflight map[reconcile.ResourceKey]openapi_types.UUID
	// creating, deleting, removals and rejected are the create path's equivalent;
	// see creates.go. All guarded by mu.
	creating map[reconcile.ResourceKey]createMark
	deleting map[reconcile.ResourceKey]openapi_types.UUID
	removals map[string]int
	rejected map[reconcile.ResourceKey]rejectMark
	// ctl is everything the controller kind needs that is not the REST API. See
	// ControllerConfig.
	ctl *controllerState
	// fleetVersion is the fleet-version spec last handed to Update, guarded by mu.
	// "" until one has been, which is what keeps the fleet row out of List (see
	// fleetActual).
	fleetVersion string
	// lastProgress is the last done/total read for the roll lastProgressRoll,
	// guarded by mu, so a subtask read that fails repeats it (see rollProgress).
	lastProgressRoll openapi_types.UUID
	lastProgress     string
	// lastSeen is the key set the last successful tree read returned, guarded by
	// mu. It is what lets a failed read name what it could not reach.
	lastSeen []reconcile.ResourceKey
	// lastSeenExt is the same for registered extensions, which are read apart from
	// the tree (see listExtensions).
	lastSeenExt []string
	// adminReady is set once the agent's admin user exists (see ensureYCPAdmin).
	adminReady bool
	// lastHealth is each endpoint's last health verdict and when it was read,
	// guarded by mu, so a tick whose read came back without one repeats it (see
	// endpointHealth).
	lastHealth map[reconcile.ResourceKey]heldHealth
	// lastCA is each endpoint's last CA answer, guarded by mu (see endpointCA).
	lastCA map[reconcile.ResourceKey]heldCA
	// now is the clock endpointHealth ages verdicts by; nil means time.Now.
	now func() time.Time
}

type heldHealth struct {
	health ampclient.EndpointHealth
	at     time.Time
}

// healthStaleAfter is how long a held verdict is repeated without a note that
// it is old. The engine's default StuckAfter, so a reading this old is flagged
// on the same clock as a resource that has stopped moving.
const healthStaleAfter = 10 * time.Minute

// updateFuncs is every kind Update can converge, mapped to how. It is the one
// place that set is written down: Update dispatches through it, and
// TestUpdatableKindsEchoTheirGeneration walks it to assert each of those kinds also
// has its applied generation echoed by List — a kind that accepts updates without
// the echo is the silent latch meko#985 is about.
var updateFuncs = map[string]func(*Backend, context.Context, api.DesiredResource) error{
	KindBranch:       (*Backend).updateBranch,
	KindEndpoint:     (*Backend).updateEndpoint,
	KindController:   (*Backend).updateController,
	KindFleetVersion: (*Backend).updateFleet,
	KindExtension:    (*Backend).registerExtension,
}

// index maps a resource's hierarchical path to the controller UUIDs its
// children need in their request paths, plus the branch state Update diffs
// against.
type index struct {
	pool     map[string]openapi_types.UUID // "<pool>"
	cluster  map[string]openapi_types.UUID // "<pool>/<cluster>"
	branch   map[string]openapi_types.UUID // "<pool>/<cluster>/<branch>"
	endpoint map[string]openapi_types.UUID // full path
	// branchRow is each branch as the controller last reported it, kept because
	// Update diffs a branch spec against it: which extensions to add, and which
	// other declared fields it did not apply.
	branchRow map[string]ampclient.Branch
	// endpointRow is the same for endpoints, kept so Update can tell a declared
	// connectivity apart from the one the endpoint was created with.
	endpointRow map[string]ampclient.Endpoint
	// ext is the extension catalog as List last read it.
	ext extensionCatalog
}

func newIndex() index {
	return index{
		pool:        map[string]openapi_types.UUID{},
		cluster:     map[string]openapi_types.UUID{},
		branch:      map[string]openapi_types.UUID{},
		endpoint:    map[string]openapi_types.UUID{},
		branchRow:   map[string]ampclient.Branch{},
		endpointRow: map[string]ampclient.Endpoint{},
	}
}

// New builds a Backend against the controller at baseURL. authToken is sent as
// a bearer when non-empty; the controller API is unauthenticated in-cluster for
// now, so it is usually empty.
func New(baseURL, authToken string, httpDoer ampclient.HttpRequestDoer, ctl ControllerConfig) (*Backend, error) {
	clientOpts := []ampclient.ClientOption{}
	if httpDoer != nil {
		clientOpts = append(clientOpts, ampclient.WithHTTPClient(httpDoer))
	}
	if authToken != "" {
		clientOpts = append(clientOpts, ampclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+authToken)
			return nil
		}))
	}
	c, err := ampclient.NewClient(strings.TrimRight(baseURL, "/"), clientOpts...)
	if err != nil {
		return nil, err
	}
	return &Backend{
		api:      c,
		idx:      newIndex(),
		inflight: map[reconcile.ResourceKey]openapi_types.UUID{},
		creating: map[reconcile.ResourceKey]createMark{},
		deleting: map[reconcile.ResourceKey]openapi_types.UUID{},
		removals: map[string]int{},
		rejected: map[reconcile.ResourceKey]rejectMark{},
		ctl:      &controllerState{cfg: ctl},
	}, nil
}

// Parent returns the resource's parent key derived from its path.
func (b *Backend) Parent(r api.DesiredResource) (reconcile.ResourceKey, bool) {
	segs := strings.Split(r.Name, sep)
	switch r.Kind {
	case KindCluster:
		if len(segs) >= 2 {
			return reconcile.ResourceKey{Kind: KindPool, Name: segs[0]}, true
		}
	case KindBranch:
		if len(segs) >= 3 {
			return reconcile.ResourceKey{Kind: KindCluster, Name: join(segs[:2])}, true
		}
	case KindEndpoint:
		if len(segs) >= 4 {
			return reconcile.ResourceKey{Kind: KindBranch, Name: join(segs[:3])}, true
		}
	case KindFleetVersion:
		// The fleet can only reach a version the controller deploys, so the roll
		// waits for the controller to be Ready at its declared version.
		return reconcile.ResourceKey{Kind: KindController, Name: controllerName}, true
	}
	return reconcile.ResourceKey{}, false
}

func join(segs []string) string { return strings.Join(segs, sep) }

func leaf(name string) string {
	segs := strings.Split(name, sep)
	return segs[len(segs)-1]
}

// attrPut sets m[k]=v when v is non-empty, allocating m on first use, so a
// resource with no attributes reports a nil Attrs (and omitempty holds).
func attrPut(m map[string]string, k, v string) map[string]string {
	if v == "" {
		return m
	}
	return attrSet(m, k, v)
}

// attrSet is attrPut without the drop rule, for the attrs where an empty value is
// itself the observation rather than an absent one.
func attrSet(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// attrPutSettings reports one settings surface as its canonical JSON — whole, not
// flattened per setting, because the PATCH replaces a named surface and the map is
// what a consumer has to see before it can safely declare one.
//
// The key is written UNCONDITIONALLY, as "{}" for a surface the controller reports
// absent or empty: its GET omits both, so nothing tells them apart on the wire, and
// both say no key is stored. An ABSENT attr therefore means the agent predates this
// release, never that the surface is empty — read as empty it is a blank editor for
// a resource full of settings and every key deleted on save (meko#1295).
func attrPutSettings[T any](m map[string]string, k string, reported *map[string]T) map[string]string {
	surface := map[string]T{}
	if reported != nil {
		surface = *reported
	}
	b, err := json.Marshal(surface)
	if err != nil {
		// Unreachable: the surface was decoded from the controller's own JSON.
		// Omitting the key reports it as unknown, which is the safe reading — "{}"
		// would claim it is empty and invite exactly the destructive save above.
		return m
	}
	return attrSet(m, k, string(b))
}

// observedGen reads the controller's echoed ycp_generation. A NULL column means
// no YCP update has been applied to the row, which is generation 0. Only the kinds
// in updateFuncs echo one; the rest report 0 and rely on the engine's cache.
func observedGen(echoed *int64) int64 {
	if echoed == nil {
		return 0
	}
	return *echoed
}

// attrPutInt is attrPut for an int, skipping the zero value so an unset
// pg_version is not reported as "0".
func attrPutInt(m map[string]string, k string, v int) map[string]string {
	if v == 0 {
		return m
	}
	return attrPut(m, k, strconv.Itoa(v))
}

// branchAttrs reports a branch's amp id and durable details: pg version, the
// LSN it was forked at, its lineage, its externalized-extension set, and the two
// settings surfaces. The id pairs with a fork's parent_branch_id so a UI can
// rebuild the fork tree. The extension set is per branch — seeded from the
// cluster, inherited on fork, and added to later — so each branch reports its own.
func branchAttrs(br ampclient.Branch) map[string]string {
	var m map[string]string
	m = attrPut(m, "id", br.BranchId.String())
	m = attrPutInt(m, "pg_version", br.PgVersion)
	if br.ForkLsn != nil {
		m = attrPut(m, "fork_lsn", *br.ForkLsn)
	}
	if br.ParentBranchId != nil {
		m = attrPut(m, "parent_branch_id", br.ParentBranchId.String())
	}
	if br.IsDefault {
		m = attrPut(m, "is_default", "true")
	}
	if br.Extensions != nil && len(*br.Extensions) > 0 {
		m = attrPut(m, "extensions", strings.Join(*br.Extensions, ","))
	}
	m = attrPutSettings(m, "pg_settings", br.PgSettings)
	m = attrPutSettings(m, "write_token_settings", (*map[string]interface{})(br.WriteTokenSettings))
	m = attrPutAppliedGen(m, br.YcpGeneration)
	return m
}

// attrPutAppliedGen reports the generation the controller has fully applied, so an
// operator reading the inventory can see how far behind a resource is instead of
// inferring it from a stale attr. Omitted when the controller has applied no YCP
// update to the row, which is the same as reporting nothing.
func attrPutAppliedGen(m map[string]string, echoed *int64) map[string]string {
	if echoed == nil {
		return m
	}
	return attrPut(m, "applied_generation", strconv.FormatInt(*echoed, 10))
}

// endpointAttrs reports a compute endpoint's amp id, role, connection addresses,
// client reachability, reserved compute, and its settings surfaces.
//
// The mode is reported resolved and unconditionally, because an endpoint created
// before connectivity was configurable reports none and behaves as cluster_ip. It
// is also the only record of the exposure that was ASKED for, which an address
// alone cannot show: an nlb_* create whose load balancer outruns the controller's
// poll falls back to the Service-type address and nothing re-polls. On the default
// ClusterIP deployment that fallback is a .svc.cluster.local name, so the pair is
// conclusive for either mode; on a NodePort or LoadBalancer deployment it is an IP
// again, and for nlb_internal — whose LB address is itself private on GKE/AKS — the
// mode is then only a reason to go and check the Service (controller_runbook.md,
// "Endpoint stuck on the in-cluster address after an nlb_* create").
func endpointAttrs(ep ampclient.Endpoint, ca *ampclient.EndpointCaBundle) map[string]string {
	var m map[string]string
	m = attrPut(m, "id", ep.Id.String())
	m = attrPut(m, "role", string(ep.Role))
	m = attrPut(m, "address", ep.Address)
	m = attrPut(m, "tailnet_address", ep.TailnetAddress)
	if ep.DocumentdbAddress != nil {
		m = attrPut(m, "documentdb_address", *ep.DocumentdbAddress)
	}
	m = attrPutCA(m, ca)
	mode := connectivityMode(ep.Connectivity)
	m = attrPut(m, "connectivity_mode", string(mode))
	if mode == ampclient.NlbInternal || mode == ampclient.NlbExternal {
		// Written even when the list is empty, which attrPut would drop: empty
		// means UNRESTRICTED, and on nlb_external that is Postgres reachable from
		// the internet with the password as the only barrier. Under cluster_ip the
		// field has no meaning (the controller refuses it with a 400), so the key
		// is absent there rather than empty — which would read as unrestricted.
		// Sorted through the same helper the drift comparison normalises with, so
		// the reported value matches what that check treats as equal and does not
		// churn tick to tick on a re-ordering the controller considers identical.
		m = attrSet(m, "connectivity_allowed_cidrs", strings.Join(allowedCidrs(ep.Connectivity), ","))
	}
	if ep.Resources != nil {
		if ep.Resources.Cpu != nil {
			m = attrPut(m, "cpu", *ep.Resources.Cpu)
		}
		if ep.Resources.Memory != nil {
			m = attrPut(m, "memory", *ep.Resources.Memory)
		}
	}
	m = attrPutSettings(m, "pg_settings", ep.PgSettings)
	m = attrPutSettings(m, "pageserver_settings", (*map[string]interface{})(ep.PageserverSettings))
	m = attrPutSettings(m, "safekeeper_settings", (*map[string]interface{})(ep.SafekeeperSettings))
	// Reported although updateEndpointSettings cannot write it: extended_config is
	// declarable at create, so a spec disagreeing with the row is permanent drift,
	// and without this there is nothing to see what it disagrees with.
	m = attrPutSettings(m, "extended_config", ep.ExtendedConfig)
	m = attrPutAppliedGen(m, ep.YcpGeneration)
	return m
}

// List traverses pools -> clusters -> branches -> endpoints and returns every
// YCP-owned resource, rebuilding the id index as it goes.
//
// The controller row is read first and kept even when the rest fails. The
// controller kind converges on helm and the k8s rollout, not on the controller's
// REST API — which is exactly what a bad controller version takes down — so a
// plain error here would make the version that rolls it back unappliable and
// leave recovery to a human with helm on the cluster.
//
// What the tree read could not reach is reported per resource rather than
// dropped (reconcile.Actual.DiscoveryError): a short list would have the engine
// create every endpoint again.
func (b *Backend) List(ctx context.Context) ([]reconcile.Actual, error) {
	var singletons []reconcile.Actual
	if ctl, ok := b.controllerActual(ctx); ok {
		singletons = append(singletons, ctl)
	}
	if fleet, ok := b.fleetActual(ctx); ok {
		singletons = append(singletons, fleet)
	}
	// Read apart from the tree: the catalog is not under any pool, so a tree that
	// cannot be read is no reason to lose it, nor it a reason to lose the tree.
	exts, cat := b.listExtensions(ctx)
	singletons = append(singletons, exts...)
	out, idx, err := b.listTree(ctx)
	if err != nil {
		partial := append(b.unreadable(err), singletons...)
		if len(partial) == 0 {
			return nil, err
		}
		b.mu.Lock()
		b.idx.ext = cat
		b.mu.Unlock()
		return partial, nil
	}
	out = append(out, singletons...)
	idx.ext = cat
	b.mu.Lock()
	b.noteRemovals(b.idx, idx)
	b.idx = idx
	b.mu.Unlock()
	return out, nil
}

// unreadable names what this tick could not read, so the engine can tell it from
// what is genuinely absent. The keys come from the last good List: a resource
// seen before and unreachable now is one the engine must not create again.
//
// Resources never yet seen are not named, and do not need to be: a create for
// one is issued against a remote that is down, fails, and is retried with
// backoff. It is the ones that EXIST that a short list would duplicate.
func (b *Backend) unreadable(cause error) []reconcile.Actual {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]reconcile.Actual, 0, len(b.lastSeen))
	for _, k := range b.lastSeen {
		out = append(out, reconcile.Actual{Key: k, DiscoveryError: cause})
	}
	return out
}

func (b *Backend) listTree(ctx context.Context) ([]reconcile.Actual, index, error) {
	idx := newIndex()
	var out []reconcile.Actual

	var pools ampclient.StoragePoolList
	if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.ListStoragePools(c, &ampclient.ListStoragePoolsParams{})
	}, &pools); err != nil {
		return nil, idx, fmt.Errorf("list storage pools: %w", err)
	}
	for _, p := range pools.StoragePools {
		poolPath := p.Name
		idx.pool[poolPath] = p.Id
		out = append(out, reconcile.Actual{
			Key:   reconcile.ResourceKey{Kind: KindPool, Name: poolPath},
			Ready: true,
			State: "ready",
			Attrs: attrPut(nil, "id", p.Id.String()),
		})

		var clusters ampclient.ClusterList
		if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.ListClusters(c, p.Id, &ampclient.ListClustersParams{})
		}, &clusters); err != nil {
			return nil, idx, fmt.Errorf("list clusters in %q: %w", poolPath, err)
		}
		for _, cl := range clusters.Clusters {
			clusterPath := join([]string{poolPath, cl.Name})
			idx.cluster[clusterPath] = cl.Id
			clAttrs := attrPutInt(attrPut(nil, "id", cl.Id.String()), "pg_version", cl.DefaultBranch.PgVersion)
			// The engine the cluster was provisioned as. Reported so a desired
			// state that named one can see it took effect, rather than the drop
			// being invisible.
			if cl.Engine != nil && *cl.Engine != "" {
				clAttrs = attrPut(clAttrs, "engine", *cl.Engine)
			}
			// Extensions are opted in per branch; surface the cluster's
			// default-branch set here as a convenience view (each branch reports its
			// own copy via branchAttrs).
			if cl.Extensions != nil && len(*cl.Extensions) > 0 {
				clAttrs = attrPut(clAttrs, "extensions", strings.Join(*cl.Extensions, ","))
			}
			out = append(out, reconcile.Actual{
				Key:    reconcile.ResourceKey{Kind: KindCluster, Name: clusterPath},
				Parent: reconcile.ResourceKey{Kind: KindPool, Name: poolPath},
				Ready:  true,
				State:  "ready",
				Attrs:  clAttrs,
			})

			var branches ampclient.BranchList
			if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
				return b.api.ListBranches(c, p.Id, cl.Id, &ampclient.ListBranchesParams{})
			}, &branches); err != nil {
				return nil, idx, fmt.Errorf("list branches in %q: %w", clusterPath, err)
			}
			for _, br := range branches.Branches {
				branchPath := join([]string{poolPath, cl.Name, br.Name})
				idx.branch[branchPath] = br.BranchId
				idx.branchRow[branchPath] = br
				out = append(out, reconcile.Actual{
					Key:         reconcile.ResourceKey{Kind: KindBranch, Name: branchPath},
					Parent:      reconcile.ResourceKey{Kind: KindCluster, Name: clusterPath},
					ObservedGen: observedGen(br.YcpGeneration),
					Ready:       true,
					State:       "ready",
					Attrs:       branchAttrs(br),
				})

				var endpoints ampclient.EndpointList
				if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
					return b.api.ListEndpoints(c, p.Id, cl.Id, br.BranchId, &ampclient.ListEndpointsParams{})
				}, &endpoints); err != nil {
					return nil, idx, fmt.Errorf("list endpoints in %q: %w", branchPath, err)
				}
				for _, ep := range endpoints.Endpoints {
					epName := ep.Name
					if epName == "" {
						epName = ep.Id.String()
					}
					epPath := join([]string{poolPath, cl.Name, br.Name, epName})
					idx.endpoint[epPath] = ep.Id
					idx.endpointRow[epPath] = ep
					epKey := reconcile.ResourceKey{Kind: KindEndpoint, Name: epPath}
					h, readAt := b.endpointHealth(epKey, ep.Health)
					ready, state, detail := endpointState(ep.Address, h, readAt, b.clock())
					out = append(out, reconcile.Actual{
						Key:         epKey,
						Parent:      reconcile.ResourceKey{Kind: KindBranch, Name: branchPath},
						ObservedGen: observedGen(ep.YcpGeneration),
						Ready:       ready,
						State:       state,
						Detail:      detail,
						Attrs:       endpointAttrs(ep, b.endpointCA(ctx, epKey, p.Id, cl.Id, ep)),
					})
				}
			}
		}
	}

	// Remembered so a later tick that cannot reach the controller can name what it
	// failed to read rather than return a short list (see unreadable).
	seen := make([]reconcile.ResourceKey, 0, len(out))
	for _, a := range out {
		seen = append(seen, a.Key)
	}
	b.mu.Lock()
	b.lastSeen = seen
	for k := range b.lastHealth {
		if _, ok := idx.endpoint[k.Name]; !ok {
			delete(b.lastHealth, k)
		}
	}
	for k := range b.lastCA {
		if _, ok := idx.endpoint[k.Name]; !ok {
			delete(b.lastCA, k)
		}
	}
	b.mu.Unlock()

	return out, idx, nil
}

// Create issues the async create task for r. A 409 (already exists / in flight)
// is treated as success so an in-flight create is never re-driven into an
// error. Cluster, branch and endpoint creates plan a task, which createTask
// follows; the pool and its owner are synchronous and go through mutate.
func (b *Backend) Create(ctx context.Context, r api.DesiredResource) error {
	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	name := leaf(r.Name)
	switch r.Kind {
	case KindPool:
		var req ampclient.CreateStoragePoolRequest
		if err := unmarshalSpec(r, &req); err != nil {
			return err
		}
		req.Name = name
		// A pool needs an existing owner, but users are not part of YCP desired
		// state (no user kind — the engine would List/GC them, and the controller
		// has no user delete). The operator only carries owner_user_id in the pool
		// spec, so materialize that user here — idempotent on the explicit id — or
		// the pool create 404s on a missing owner on a fresh controller.
		if owner := req.OwnerUserId; owner != (openapi_types.UUID{}) {
			if err := b.mutate(ctx, func(c context.Context) (*http.Response, error) {
				return b.api.CreateUser(c, ampclient.CreateUserRequest{Id: &owner})
			}); err != nil {
				return fmt.Errorf("ensure pool owner %s: %w", owner, err)
			}
		}
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.CreateStoragePool(c, req)
		})
	case KindCluster:
		poolID, ok := idx.pool[parentPath(r.Name)]
		if !ok {
			return fmt.Errorf("create cluster %q: parent pool not found", r.Name)
		}
		var req ampclient.CreateClusterRequest
		if err := unmarshalSpec(r, &req); err != nil {
			return err
		}
		if req.Extensions != nil {
			if err := notInCatalog(idx.ext, *req.Extensions); err != nil {
				return fmt.Errorf("create cluster %q: %w", r.Name, err)
			}
		}
		req.Name = name
		return b.createTask(ctx, r, func(c context.Context) (*http.Response, error) {
			return b.api.CreateCluster(c, poolID, req)
		})
	case KindBranch:
		// Extensions are not a create-time field: addBranchExtensions takes a branch
		// id this call has not produced yet. Refuse rather than let json.Unmarshal
		// drop the field into CreateBranchRequest, which has no place for it — a
		// declared extension would otherwise vanish with no error anywhere.
		spec, err := branchSpecOf(r)
		if err != nil {
			return err
		}
		if len(extensionSet(spec.Extensions)) > 0 {
			return reconcile.Rejected(fmt.Errorf(
				"create branch %q: extensions cannot be declared at branch create (a new branch inherits its parent's set) — declare them once the branch exists",
				r.Name))
		}
		// Same shape, same reason: write_token_settings reaches a branch only through
		// updateBranchSettings, which needs a branch id this call has not produced.
		// CreateBranchRequest has no field for it either, so letting it through would
		// drop a declared write lease silently — and the lease is what stops two
		// computes both believing they may write.
		if spec.WriteTokenSettings != nil {
			return reconcile.Rejected(fmt.Errorf(
				"create branch %q: write_token_settings cannot be declared at branch create (no create verb carries it) — declare it once the branch exists",
				r.Name))
		}
		poolID, clusterID, ok := b.resolveCluster(idx, r.Name)
		if !ok {
			return fmt.Errorf("create branch %q: parent cluster not found", r.Name)
		}
		var req ampclient.CreateBranchRequest
		if err := unmarshalSpec(r, &req); err != nil {
			return err
		}
		req.Name = name
		return b.createTask(ctx, r, func(c context.Context) (*http.Response, error) {
			return b.api.CreateBranch(c, poolID, clusterID, req)
		})
	case KindEndpoint:
		poolID, clusterID, branchID, ok := b.resolveBranch(idx, r.Name)
		if !ok {
			return fmt.Errorf("create endpoint %q: parent branch not found", r.Name)
		}
		var req ampclient.CreateEndpointRequest
		if err := unmarshalSpec(r, &req); err != nil {
			return err
		}
		req.Name = &name
		return b.createTask(ctx, r, func(c context.Context) (*http.Response, error) {
			return b.api.CreateEndpoint(c, poolID, clusterID, branchID, req)
		})
	case KindController:
		// The release exists — this agent runs inside it — so a create is an update
		// that has not seen a row yet.
		return b.updateController(ctx, r)
	case KindFleetVersion:
		// Absent only because this process has not been handed a declaration yet.
		return b.updateFleet(ctx, r)
	case KindExtension:
		return b.registerExtension(ctx, r)
	}
	return fmt.Errorf("create: unknown kind %q", r.Kind)
}

// Update carries the spec changes this agent applies to a resource that already
// exists: a branch's or endpoint's settings, resizing an endpoint, enabling
// externalized extensions on a branch, and the controller's own version.
// Clusters have no controller update path at all, and a pool's only
// one is rename — which is not an update here, since a pool's resource key IS
// its name, so renaming one reaches the engine as a different resource. Either
// way a spec change to one is a permanent (non-retryable) error the engine
// surfaces as drift rather than retrying forever; the branch fields Update does
// not apply are reported the same way (see updateBranch).
func (b *Backend) Update(ctx context.Context, r api.DesiredResource) error {
	// A resource whose create is still in flight, or landed carrying this very
	// generation, has nothing to update: see priorCreate.
	key := reconcile.ResourceKey{Kind: r.Kind, Name: r.Name}
	if gen, err := b.priorCreate(ctx, key); err != nil {
		return err
	} else if gen != nil {
		b.mu.Lock()
		delete(b.creating, key)
		b.mu.Unlock()
		if *gen == r.Generation {
			return nil
		}
	}
	apply, ok := updateFuncs[r.Kind]
	if !ok {
		return reconcile.Permanent(fmt.Errorf("update not supported for kind %q (only branch and endpoint settings, endpoint resize and allowed CIDRs, branch extensions, extension registrations, and the controller and fleet versions)", r.Kind))
	}
	return apply(b, ctx, r)
}

// applyUpdate is the one seam every Update arm goes through, so none of them can
// be written without the bookkeeping that makes an async update converge.
//
// It owns what a caller must not have to remember: reading the previous attempt's
// task before issuing anything — so an in-flight one reports pending and a failed
// one reports the controller's own error, instead of being silently retried
// forever — and recording the task the 202 planned for the next tick to read.
//
// The generation reaches the request body through the closure's argument rather
// than being set here, because each verb has its own request type and there is no
// field to assign generically. Passing it in means an arm cannot receive the
// generation and quietly drop it without that being visible at the call site;
// TestUpdateSendsAppliedGeneration checks it actually lands in the body.
//
// Every update call that plans a TASK hands it here and gets all of it. One that
// bypasses this gets none of it — a whole missing mechanism rather than a subtle
// omission. The synchronous settings PATCHes are the deliberate exception and go
// through patchSettings instead: they leave no task behind, so there is nothing
// here for them to inherit.
func (b *Backend) applyUpdate(ctx context.Context, key reconcile.ResourceKey, gen int64,
	call func(ctx context.Context, gen int64) (*http.Response, error)) error {

	if err := b.priorTask(ctx, key); err != nil {
		return err
	}

	resp, err := call(ctx, gen)
	if err != nil {
		return err
	}
	taskID, err := b.mutateTask(resp)
	if err != nil {
		return err
	}
	if taskID == nil {
		// Accepted with no task to follow, so there is nothing further to wait on
		// and nothing this backend can add: report it applied and let the echo be
		// the authority on the next tick.
		return nil
	}
	b.setInflight(key, *taskID)
	// PENDING, not success. A 202 means the task is planned, so the update is now
	// in flight — saying "applied" here is the exact claim meko#985 is about, and it
	// is what let the engine cache a generation for work that then failed. An update
	// only records as applied by returning nil, which happens when this backend
	// started no work at all.
	return reconcile.Pending(fmt.Errorf("controller task %s planned for generation %d", *taskID, gen))
}

// priorTask is the verdict on the task the LAST update for this resource planned:
// nil to go ahead, Pending while it is still running, its own error when it failed.
//
// Every path that can claim a generation has to consult this before doing so, not
// just the ones that plan a task. A synchronous settings PATCH records the
// generation the moment it returns, so issuing one while a task is still rolling
// claims that generation for work which has not finished — and because the echo
// then matches, the engine Noops the resource and no later tick ever revisits it.
// That is the meko#985 latch, reached through the one door applyUpdate does not
// cover.
//
// The branch path is where it bites. `adding` is a delta against the branch row,
// and add_branch_extensions unions that row as its FIRST effect, before it
// redeploys a single endpoint — so one tick after the 202 there is nothing left to
// add, nothing to change, and an unstamped generation: exactly the shape that
// reaches the pure stamp. Before the whole-set re-send was retired this was covered
// by accident, because the re-send went through applyUpdate.
func (b *Backend) priorTask(ctx context.Context, key reconcile.ResourceKey) error {
	taskID, ok := b.inflightTask(key)
	if !ok {
		return nil
	}
	status, taskErr, _, err := b.taskOutcome(ctx, taskID)
	switch {
	case err != nil:
		return fmt.Errorf("read task %s for %s: %w", taskID, key, err)
	case status == taskStatusPending || status == taskStatusRunning:
		return reconcile.Pending(fmt.Errorf("controller task %s is %s", taskID, status))
	case status == taskStatusFailed:
		// The failure the 202 could not report. Clearing the entry lets the next
		// tick re-issue after the engine's backoff.
		b.clearInflight(key)
		return fmt.Errorf("controller task %s failed: %s", taskID, taskErr)
	default:
		// Succeeded, yet the caller still sees the resource as stale — the engine
		// only plans an update when the echoed generation is behind. Fall through
		// and re-issue rather than reporting converged on the task's word.
		b.clearInflight(key)
		return nil
	}
}

func (b *Backend) inflightTask(key reconcile.ResourceKey) (openapi_types.UUID, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	id, ok := b.inflight[key]
	return id, ok
}

func (b *Backend) setInflight(key reconcile.ResourceKey, id openapi_types.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inflight[key] = id
}

func (b *Backend) clearInflight(key reconcile.ResourceKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inflight, key)
}

// Controller task statuses (openapi_spec.yaml: Task.status).
const (
	taskStatusPending   = "pending"
	taskStatusRunning   = "running"
	taskStatusSucceeded = "succeeded"
	taskStatusFailed    = "failed"

	// taskErrorConflict is the failed task's error_code for work that collides
	// with existing state — what a synchronous call would have answered 409.
	taskErrorConflict = "conflict"
)

// taskOutcome reads a planned task's status and, when it failed, its error. A
// task the controller no longer knows (404) is reported succeeded: there is nothing
// left to wait for, and the echoed generation is the authority on whether the work
// landed. Defensive rather than observed — the controller has no task retention
// today, so its log only grows; nothing in YCP should depend on that staying true.
func (b *Backend) taskOutcome(ctx context.Context, taskID openapi_types.UUID) (status, taskErr, code string, err error) {
	resp, err := b.api.GetTask(ctx, taskID)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return taskStatusSucceeded, "", "", nil
	}
	if resp.StatusCode >= 300 {
		return "", "", "", httpErr(resp.StatusCode, body)
	}
	var t ampclient.Task
	if err := json.Unmarshal(body, &t); err != nil {
		return "", "", "", fmt.Errorf("decode task: %w", err)
	}
	if t.Error != nil {
		taskErr = *t.Error
	}
	if taskErr == "" {
		taskErr = "no error reported"
	}
	if t.ErrorCode != nil {
		code = *t.ErrorCode
	}
	return string(t.Status), taskErr, code, nil
}

// driftSet collects the reasons an update could not fully land, and the reason
// they add up to. Both update paths mix two kinds: a field no verb carries
// (ReasonUnsupported — the resource is simply like that), and a call the controller
// refused (ReasonRejected — the operator re-declares and it works). Reported as one
// word the second reads as the first, which is CLOUDGA-36992.
type driftSet struct {
	reasons []string
	reason  reconcile.PermanentReason
}

func (d *driftSet) add(reasons ...string) { d.reasons = append(d.reasons, reasons...) }

func (d *driftSet) any() bool { return len(d.reasons) > 0 }

// fold records a verb's failure as one more reason, and lets a rejection or a
// block outrank the plain unsupported ones — those are the halves an operator can
// act on, and burying them under "immutable" is the whole bug.
//
// EVERY failure, including a Pending one, because this row gets exactly one
// report. A drifted spec claims generation 0, so nothing ever advances the
// echoed generation, the engine's permanent mark never expires (loop.go), and
// this update is never issued again — priorTask does not run a second time and the
// task recorded by applyUpdate is never read back. Naming the task here is
// therefore the only trace of it on the resource; its outcome comes from the task
// log the agent relays, keyed by that id.
func (d *driftSet) fold(err error) {
	d.reasons = append(d.reasons, err.Error())
	if r := reconcile.PermanentReasonOf(err); r != reconcile.ReasonUnsupported {
		d.reason = r
	}
}

// err is the permanent error to return for the whole update: every reason joined,
// under the verb and resource the caller names.
func (d *driftSet) err(format string, args ...any) error {
	return reconcile.PermanentAs(d.reason,
		fmt.Errorf(format+": %s", append(args, strings.Join(d.reasons, "; "))...))
}

// updateEndpoint converges an endpoint's settings, its allowed CIDRs and its
// compute reservation.
//
// Up to three calls, and which one carries the generation is the whole ordering
// argument: only the LAST call issued may. The settings PATCH goes FIRST and is
// synchronous, so a 200 means the row is written; the connectivity update and the
// resize after it are tasks, and the worker stamps the generation only once its
// task has succeeded — by which time everything before it has provably landed. A
// retryable PATCH failure returns before any task is issued, so no generation can
// ever be claimed for settings that did not land.
//
// The two tasks are issued in one tick only on a drifted update, which has no next
// tick. Otherwise this backend follows one task per resource (setInflight), so the
// connectivity update goes alone, carrying the generation only when no resize
// follows, and returns Pending; the next tick finds the row reporting the new
// ranges and issues the resize with the generation.
//
// The resize is issued only when the spec declares resources. It used to go out
// unconditionally, which was harmless — an omitted dimension keeps the endpoint's
// current value, so the task resized it to what it already was — but it was also
// the only thing stamping the generation, and a settings-only spec would have
// planned a pointless task to get one. Now the PATCH can carry it instead.
func (b *Backend) updateEndpoint(ctx context.Context, r api.DesiredResource) error {
	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	poolID, clusterID, branchID, ok := b.resolveBranch(idx, r.Name)
	if !ok {
		return fmt.Errorf("update endpoint %q: parent branch not found", r.Name)
	}
	endpointID, ok := idx.endpoint[r.Name]
	if !ok {
		return fmt.Errorf("update endpoint %q: endpoint not found", r.Name)
	}

	// Computed before anything is issued, for the same reason as updateBranch: a
	// spec declaring a field no update verb carries (connectivity.mode, role,
	// tags) never fully lands, and claiming the generation would leave the engine Noop'ing the
	// endpoint forever at a generation its spec never met.
	spec, err := endpointSpecOf(r)
	if err != nil {
		return err
	}
	ep := idx.endpointRow[r.Name]
	drift := driftSet{reasons: unappliedEndpoint(spec, ep)}
	resizing := spec.Resources != nil
	cidrs, recidr := cidrsToApply(spec, ep)

	// Before anything is issued, and before anything can claim a generation: the
	// last update may have planned a task that is still running or has failed. See
	// priorTask — a settings PATCH stamps synchronously, so it must not go out on
	// top of work that has not finished.
	key := reconcile.ResourceKey{Kind: r.Kind, Name: r.Name}
	if err := b.priorTask(ctx, key); err != nil {
		return err
	}

	body, changed := endpointSettingsPatch(spec, ep)
	// Nothing to change, nothing else to issue, no drift, and the controller has not
	// recorded this generation: PATCH an empty body purely to stamp it. Every surface
	// is omitted, so it touches nothing a compute reads — the controller skips telling
	// them for exactly that reason — but it is the only call left that can record the
	// generation, and without it the engine would re-plan this update forever.
	//
	// Drift excluded because there would be nothing to stamp: a spec that never fully
	// lands claims no generation, so the call would carry an empty body and no
	// generation either.
	if !changed && !resizing && !recidr && !drift.any() && b.generationBehind(r, ep.YcpGeneration) {
		changed = true
	}
	if changed {
		gen := r.Generation
		if drift.any() {
			gen = 0 // claim nothing; see the drift note above
		}
		if !resizing && !recidr && gen > 0 {
			body.YcpGeneration = &gen
		}
		if err := b.patchSettings(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.UpdateEndpointSettings(c, poolID, clusterID, branchID, endpointID, body)
		}); err != nil {
			var perm *reconcile.PermanentError
			if !errors.As(err, &perm) {
				// Retryable: the settings did not land, so nothing may be issued that
				// could stamp this generation. The engine backs off and asks again.
				return fmt.Errorf("update endpoint %q settings: %w", r.Name, err)
			}
			// The controller refused these settings as written and stored none of
			// them. Report it with the rest of the drift rather than returning here,
			// so one bad setting does not block a resources change — but the
			// generation is now unclaimable, which the drift list takes care of.
			drift.fold(err)
		}
	}

	if recidr && !drift.any() {
		gen := r.Generation
		if resizing {
			gen = 0 // the resize carries it, next tick
		}
		// Pending on success; the resize, if any, follows once this task is done.
		return b.applyUpdate(ctx, key, gen,
			func(c context.Context, gen int64) (*http.Response, error) {
				req := ampclient.UpdateEndpointConnectivityRequest{AllowedCidrs: cidrs}
				if gen > 0 {
					req.YcpGeneration = &gen
				}
				return b.api.UpdateEndpointConnectivity(c, poolID, clusterID, branchID, endpointID, req)
			})
	}

	if resizing {
		var req ampclient.ResizeEndpointRequest
		if err := unmarshalSpec(r, &req); err != nil {
			return err
		}
		gen := r.Generation
		if drift.any() {
			gen = 0 // claim nothing; see the drift note above
		}
		if err := b.applyUpdate(ctx, key, gen,
			func(c context.Context, gen int64) (*http.Response, error) {
				if gen > 0 {
					req.YcpGeneration = &gen
				}
				return b.api.ResizeEndpoint(c, poolID, clusterID, branchID, endpointID, req)
			}); err != nil {
			if !drift.any() {
				return err
			}
			// Drift already decided this update reports permanent, so the resize's own
			// outcome has nowhere to be returned to. Folding it in is the only way it
			// is reported at all: dropped, a refused resize leaves the row naming an
			// unrelated immutable field and nothing else.
			drift.fold(err)
		}
	}
	if recidr && drift.any() {
		// A drifted update is reported once and never re-issued (driftSet.fold), so
		// there is no next tick to defer to: both tasks go out now, the resize first.
		// Not through applyUpdate, whose priorTask would read the resize just planned
		// as in flight and send nothing; nothing reads the in-flight record of a
		// drifted update back, so there is none to keep. The controller's endpoint
		// lock serializes the two.
		resp, err := b.api.UpdateEndpointConnectivity(ctx, poolID, clusterID, branchID, endpointID,
			ampclient.UpdateEndpointConnectivityRequest{AllowedCidrs: cidrs})
		if err == nil {
			var taskID *openapi_types.UUID
			if taskID, err = b.mutateTask(resp); err == nil && taskID != nil {
				err = reconcile.Pending(fmt.Errorf("controller task %s planned for generation 0", *taskID))
			}
		}
		if err != nil {
			drift.fold(err)
		}
	}
	if drift.any() {
		return drift.err("update endpoint %q", r.Name)
	}
	return nil
}

// endpointSpec is an endpoint's desired spec as Update reads it: the three
// settings surfaces it can now apply, resources (read only to decide whether a
// resize is wanted at all — the resize request is decoded separately), and the
// fields it can still only check. Every field is a pointer so an omitted one —
// unmanaged — is distinguishable from a declared empty one.
type endpointSpec struct {
	Connectivity       *ampclient.EndpointConnectivity      `json:"connectivity"`
	Role               *ampclient.CreateEndpointRequestRole `json:"role"`
	Resources          *ampclient.EndpointResources         `json:"resources"`
	PgSettings         *map[string]string                   `json:"pg_settings"`
	PageserverSettings *ampclient.DaemonSettings            `json:"pageserver_settings"`
	SafekeeperSettings *ampclient.DaemonSettings            `json:"safekeeper_settings"`
	ExtendedConfig     *map[string]interface{}              `json:"extended_config"`
	Tags               *map[string]string                   `json:"tags"`
}

func endpointSpecOf(r api.DesiredResource) (endpointSpec, error) {
	var spec endpointSpec
	err := unmarshalSpec(r, &spec)
	return spec, err
}

// connectivityMode is the mode an endpoint effectively has. An endpoint created
// before connectivity was configurable reports none and behaves as cluster_ip
// (openapi_spec.yaml: EndpointConnectivity), so absent and an explicit
// cluster_ip are the same thing — a spec declaring the default is not drift.
func connectivityMode(c *ampclient.EndpointConnectivity) ampclient.EndpointConnectivityMode {
	if c == nil || c.Mode == nil {
		return ampclient.ClusterIp
	}
	return *c.Mode
}

// allowedCidrs is the declared or reported source ranges, normalised as the
// controller stores them — trimmed, empty entries dropped — and sorted, so neither
// a padded entry nor an order-only difference reads as a change.
func allowedCidrs(c *ampclient.EndpointConnectivity) []string {
	if c == nil || c.AllowedCidrs == nil {
		return nil
	}
	var out []string
	for _, cidr := range *c.AllowedCidrs {
		if cidr = strings.TrimSpace(cidr); cidr != "" {
			out = append(out, cidr)
		}
	}
	sort.Strings(out)
	return out
}

// unappliedEndpoint names the endpoint-spec fields whose declared value the
// controller does not report, i.e. the ones this update did not and cannot
// apply. What is left here is create-time in the controller — connectivity.mode,
// role and tags, which no update verb carries — plus extended_config, which is the
// one surface updateEndpointSettings does NOT take, so it stays unfixable for a
// reason of its own. allowed_cidrs is here only alongside a mode change: the
// controller validates a new list against the CURRENT mode, so it cannot be
// applied while the mode is wrong (cidrsToApply).
//
// disk_type is deliberately not compared: the Endpoint response does not report
// it, so a declared value cannot be told from the one the endpoint was created
// with, and a mismatch would not reliably mean drift.
func unappliedEndpoint(spec endpointSpec, ep ampclient.Endpoint) []string {
	var out []string
	if spec.Connectivity != nil {
		if connectivityMode(spec.Connectivity) != connectivityMode(ep.Connectivity) {
			out = append(out, "connectivity.mode")
		}
		if _, recidr := cidrsToApply(spec, ep); !recidr && cidrsDiffer(spec, ep) {
			out = append(out, "connectivity.allowed_cidrs")
		}
	}
	if spec.Role != nil && string(*spec.Role) != string(ep.Role) {
		out = append(out, "role")
	}
	if spec.ExtendedConfig != nil && !sameJSON(spec.ExtendedConfig, ep.ExtendedConfig) {
		out = append(out, "extended_config")
	}
	// Create-time like role. An endpoint reports only its own tags, not the
	// labels it inherits from its cluster, so the two compare like for like.
	if spec.Tags != nil && !maps.Equal(*spec.Tags, map[string]string(ep.Tags)) {
		out = append(out, "tags")
	}
	sort.Strings(out)
	return out
}

// sameJSON compares a declared value against the reported one structurally. Go
// marshals map keys sorted and struct fields in declaration order, so the
// encoding is stable enough to compare; an unreported value encodes as null and
// so differs from anything declared.
func sameJSON(declared, reported any) bool {
	a, err := json.Marshal(declared)
	if err != nil {
		return false
	}
	b, err := json.Marshal(reported)
	if err != nil {
		return false
	}
	return string(a) == string(b)
}

// sameSurface compares a declared settings surface against what the controller
// reports for it, treating an absent reported value as empty.
//
// That normalisation is the whole point: the GET omits an empty surface, so a
// surface someone cleared and one never set arrive identically, and a spec
// declaring `{}` against either must read as converged rather than as a change to
// send forever. Both sides are decoded maps by the time they get here, so
// comparing their re-encoding is structural — a number that survived a round trip
// through the controller renders the same as the declared one.
func sameSurface[T any](declared *map[string]T, reported *map[string]T) bool {
	empty := map[string]T{}
	d, r := empty, empty
	if declared != nil {
		d = *declared
	}
	if reported != nil {
		r = *reported
	}
	return sameJSON(d, r)
}

// settingsSurface narrows a declared surface to one worth sending: the value to
// PATCH, or nil when the row already carries it.
//
// Omitting an unchanged surface is required, not an optimisation. An omitted field
// means "do not touch", and re-sending an identical one makes the controller push
// a reload to every running endpoint for a change that is not one.
func settingsSurface[T any](declared *map[string]T, reported *map[string]T) *map[string]T {
	if declared == nil || sameSurface(declared, reported) {
		return nil
	}
	return declared
}

// endpointSettingsPatch is the body converging an endpoint's three surfaces, and
// whether any of them needed it.
func endpointSettingsPatch(spec endpointSpec, ep ampclient.Endpoint) (ampclient.UpdateEndpointSettingsRequest, bool) {
	var req ampclient.UpdateEndpointSettingsRequest
	req.PgSettings = settingsSurface(spec.PgSettings, ep.PgSettings)
	req.PageserverSettings = (*ampclient.DaemonSettings)(settingsSurface(
		(*map[string]interface{})(spec.PageserverSettings), (*map[string]interface{})(ep.PageserverSettings)))
	req.SafekeeperSettings = (*ampclient.DaemonSettings)(settingsSurface(
		(*map[string]interface{})(spec.SafekeeperSettings), (*map[string]interface{})(ep.SafekeeperSettings)))
	return req, req.PgSettings != nil || req.PageserverSettings != nil || req.SafekeeperSettings != nil
}

// branchSettingsPatch is endpointSettingsPatch for a branch's two surfaces. The
// daemon surfaces have no branch equivalent — a pageserver serves the branch's
// endpoints, but its settings are each endpoint's own.
func branchSettingsPatch(spec branchSpec, br ampclient.Branch) (ampclient.UpdateBranchSettingsRequest, bool) {
	var req ampclient.UpdateBranchSettingsRequest
	req.PgSettings = settingsSurface(spec.PgSettings, br.PgSettings)
	req.WriteTokenSettings = (*ampclient.WriteTokenSettings)(settingsSurface(
		(*map[string]interface{})(spec.WriteTokenSettings), (*map[string]interface{})(br.WriteTokenSettings)))
	return req, req.PgSettings != nil || req.WriteTokenSettings != nil
}

// cidrsDiffer reports whether the spec declares an allowed_cidrs list the
// endpoint does not report. Only compared when declared: an omitted list leaves
// whatever is in force.
func cidrsDiffer(spec endpointSpec, ep ampclient.Endpoint) bool {
	return spec.Connectivity != nil && spec.Connectivity.AllowedCidrs != nil &&
		!equalStrings(allowedCidrs(spec.Connectivity), allowedCidrs(ep.Connectivity))
}

// cidrsToApply is the list updateEndpointConnectivity should be sent, and whether
// it should be sent at all: the declared list differs from the reported one and
// the mode matches, since the controller judges a list by the mode in force.
func cidrsToApply(spec endpointSpec, ep ampclient.Endpoint) ([]string, bool) {
	if !cidrsDiffer(spec, ep) || connectivityMode(spec.Connectivity) != connectivityMode(ep.Connectivity) {
		return nil, false
	}
	out := allowedCidrs(spec.Connectivity)
	if out == nil {
		out = []string{} // [] removes the restriction; the field is required
	}
	return out, true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// updateBranch applies the branch changes amp can make after create — its two
// settings surfaces and adding externalized extensions — and reports whatever
// else the spec declares that it could not.
//
// Same shape as updateEndpoint: the synchronous settings PATCH first, the
// task-planning add after, and the generation on whichever is last. The add is an
// absolute union, so re-issuing it (a replayed tick, an add still in flight) is a
// no-op, which is what lets the engine drive it with no new verb.
//
// What remains drift is create-time here: a name dropped from extensions cannot
// be removed at all (a branch's set only grows, deliberately, so a fork's
// endpoints keep mounting what its inherited objects need), and parent_branch_id
// is fixed at the fork. Work is issued before drift is reported, so one unfixable
// field does not block the rest.
func (b *Backend) updateBranch(ctx context.Context, r api.DesiredResource) error {
	spec, err := branchSpecOf(r)
	if err != nil {
		return err
	}

	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	poolID, clusterID, branchID, ok := b.resolveBranch(idx, r.Name)
	if !ok {
		return fmt.Errorf("update branch %q: branch not found", r.Name)
	}
	br := idx.branchRow[r.Name]
	have := extensionSet(br.Extensions)
	want := extensionSet(spec.Extensions)

	// Drift is computed BEFORE anything is issued, because it decides whether this
	// update may claim the generation at all. The generation asserts the whole spec
	// landed; a spec declaring something no verb here can apply never fully lands,
	// so claiming it would stamp the row, echo back at the desired generation, and
	// have the engine Noop the resource forever — silently converged at a generation
	// that was never met, which is the failure this whole change removes.
	var drift driftSet
	if spec.Extensions != nil {
		if extra := sortedMissing(have, want); len(extra) > 0 {
			drift.add("extensions " + strings.Join(extra, ",") + " are enabled and cannot be removed")
		}
	}
	drift.add(unapplied(spec, br)...)

	// As in updateEndpoint, and for the sharper reason described on priorTask: an
	// add_branch_extensions task unions the branch row before it redeploys anything,
	// so a tick landing mid-roll sees nothing to add and would otherwise stamp the
	// generation for a roll still in progress.
	key := reconcile.ResourceKey{Kind: r.Kind, Name: r.Name}
	if err := b.priorTask(ctx, key); err != nil {
		return err
	}

	// Only the names the branch lacks: every add redeploys the endpoints that are
	// behind, so asking for one already on would be work for nothing.
	var send []string
	if spec.Extensions != nil {
		send = sortedMissing(want, have)
	}
	adding := len(send) > 0
	// An add naming an extension the catalog does not have yet is held back (see
	// notInCatalog), but the settings still land. Like drift, it means the spec has
	// not fully landed, so nothing below may claim the generation.
	var waiting error
	if adding {
		if waiting = notInCatalog(idx.ext, send); waiting != nil {
			adding = false
		}
	}

	body, changed := branchSettingsPatch(spec, br)
	// As in updateEndpoint: an empty body is a pure stamp. This replaces re-sending
	// the whole extension set to get a generation recorded, which was the only way
	// before this verb could carry one — and which cost an add_branch_extensions
	// task on every generation bump of a branch that declares extensions.
	if !changed && !adding && !drift.any() && waiting == nil && b.generationBehind(r, br.YcpGeneration) {
		changed = true
	}
	if changed {
		gen := r.Generation
		if drift.any() || waiting != nil {
			gen = 0 // claim nothing; see the drift note above
		}
		if !adding && gen > 0 {
			body.YcpGeneration = &gen
		}
		if err := b.patchSettings(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.UpdateBranchSettings(c, poolID, clusterID, branchID, body)
		}); err != nil {
			var perm *reconcile.PermanentError
			if !errors.As(err, &perm) {
				return fmt.Errorf("update branch %q settings: %w", r.Name, err)
			}
			drift.fold(err)
		}
	}

	if adding {
		gen := r.Generation
		if drift.any() {
			gen = 0 // claim nothing; see the drift note above
		}
		if err := b.applyUpdate(ctx, key, gen,
			func(c context.Context, gen int64) (*http.Response, error) {
				req := ampclient.AddBranchExtensionsRequest{Extensions: send}
				if gen > 0 {
					req.YcpGeneration = &gen
				}
				return b.api.AddBranchExtensions(c, poolID, clusterID, branchID, req)
			}); err != nil {
			if !drift.any() {
				return fmt.Errorf("add extensions %s to branch %q: %w", strings.Join(send, ","), r.Name, err)
			}
			// As in updateEndpoint: drift already decided the verdict, so the add's own
			// outcome is folded in rather than lost.
			drift.fold(err)
		}
	}

	// Retryable outranks drift here: a permanent verdict would stop the add from
	// being retried once the extension is registered.
	if waiting != nil {
		if drift.any() {
			return fmt.Errorf("update branch %q: %w; also %s", r.Name, waiting, strings.Join(drift.reasons, "; "))
		}
		return fmt.Errorf("update branch %q: %w", r.Name, waiting)
	}
	if drift.any() {
		return drift.err("update branch %q", r.Name)
	}
	return nil
}

// generationBehind reports whether the controller has yet to record the generation
// this update is applying. Generation 0 is never behind: the engine only plans an
// update once the desired generation exceeds what was echoed, so a resource at 0
// is never here in the first place.
func (b *Backend) generationBehind(r api.DesiredResource, echoed *int64) bool {
	return r.Generation > observedGen(echoed)
}

// unapplied names the branch-spec fields whose declared value the controller does
// not report, i.e. the ones this update did not and cannot apply. Without it a
// spec whose extensions and settings are already converged would report success on
// a generation bump that changed something else entirely, and the engine would
// latch that generation as applied.
//
// Only parent_branch_id is left: it is fixed at the fork and no verb moves it.
//
// fork_point_lsn is deliberately not compared: a branch records the fork LSN the
// controller resolved, which is not textually what a caller requested, so a
// mismatch would not reliably mean drift.
func unapplied(spec branchSpec, br ampclient.Branch) []string {
	var out []string
	if spec.ParentBranchId != nil && (br.ParentBranchId == nil || *br.ParentBranchId != *spec.ParentBranchId) {
		out = append(out, "parent_branch_id")
	}
	sort.Strings(out)
	return out
}

// branchSpec is a branch's desired spec as Update reads it: the fields it can
// apply (extensions, and the two settings surfaces) plus the one it can only
// check (Create decodes the whole thing into CreateBranchRequest instead). Every
// field is a pointer so an omitted one — unmanaged — is distinguishable from a
// declared empty one.
type branchSpec struct {
	Extensions         *[]string                     `json:"extensions"`
	PgSettings         *map[string]string            `json:"pg_settings"`
	WriteTokenSettings *ampclient.WriteTokenSettings `json:"write_token_settings"`
	ParentBranchId     *openapi_types.UUID           `json:"parent_branch_id"`
}

func branchSpecOf(r api.DesiredResource) (branchSpec, error) {
	var spec branchSpec
	err := unmarshalSpec(r, &spec)
	return spec, err
}

// extensionSet turns a (possibly absent) extension list into a set, dropping
// blanks so a stray "" is never sent to the controller as a name.
func extensionSet(names *[]string) map[string]bool {
	set := map[string]bool{}
	if names == nil {
		return set
	}
	for _, n := range *names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// sortedMissing returns the members of names that present lacks, in a stable
// order (the controller logs what it was asked to add).
func sortedMissing(names, present map[string]bool) []string {
	var out []string
	for n := range names {
		if !present[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Delete removes the resource. Deleting the top of a withdrawn subtree cascades
// in the controller, so the engine only asks to delete the highest node.
//
// Storage pools are the exception, and still a permanent (non-retryable) error
// the engine surfaces as drift. The controller does have DELETE /storage-pools
// now, but it deliberately does NOT cascade — it refuses with 409 while the pool
// holds a live cluster (amp#1083). That is incompatible with the highest-node
// contract above: a fully withdrawn pool subtree would ask only for the pool
// delete, get a 409, and leave the clusters behind. Wiring it up needs the
// engine to stop treating a pool as a cascading root, which is not modelled here.
func (b *Backend) Delete(ctx context.Context, key reconcile.ResourceKey) error {
	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	switch key.Kind {
	case KindPool:
		return reconcile.Permanent(fmt.Errorf(
			"delete storage pool %q: the controller's pool delete does not cascade, so the engine cannot drive it as a subtree root", key.Name))
	case KindCluster:
		poolID, clusterID, ok := b.resolveCluster(idx, key.Name)
		if !ok {
			return nil // already gone
		}
		return b.deleteTask(ctx, key, func(c context.Context) (*http.Response, error) {
			return b.api.DeleteCluster(c, poolID, clusterID)
		})
	case KindBranch:
		poolID, clusterID, branchID, ok := b.resolveBranch(idx, key.Name)
		if !ok {
			return nil
		}
		return b.deleteTask(ctx, key, func(c context.Context) (*http.Response, error) {
			return b.api.DeleteBranch(c, poolID, clusterID, branchID)
		})
	case KindEndpoint:
		poolID, clusterID, branchID, ok := b.resolveBranch(idx, key.Name)
		if !ok {
			return nil
		}
		endpointID, ok := idx.endpoint[key.Name]
		if !ok {
			return nil
		}
		return b.deleteTask(ctx, key, func(c context.Context) (*http.Response, error) {
			return b.api.DeleteEndpoint(c, poolID, clusterID, branchID, endpointID)
		})
	case KindController:
		return reconcile.Permanent(fmt.Errorf(
			"delete controller %q: the controller is not deleted through desired state — it is the release this agent runs in", key.Name))
	case KindFleetVersion:
		return reconcile.Permanent(fmt.Errorf("delete fleet-version %q: the fleet is not deleted through desired state", key.Name))
	case KindExtension:
		return b.deregisterExtension(ctx, key.Name)
	}
	return fmt.Errorf("delete: unknown kind %q", key.Kind)
}

// parentPath drops the last path segment.
func parentPath(name string) string {
	segs := strings.Split(name, sep)
	if len(segs) <= 1 {
		return ""
	}
	return join(segs[:len(segs)-1])
}

func (b *Backend) resolveCluster(idx index, name string) (openapi_types.UUID, openapi_types.UUID, bool) {
	segs := strings.Split(name, sep)
	if len(segs) < 2 {
		return openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	poolID, ok := idx.pool[segs[0]]
	if !ok {
		return openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	clusterID, ok := idx.cluster[join(segs[:2])]
	if !ok {
		return openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	return poolID, clusterID, true
}

func (b *Backend) resolveBranch(idx index, name string) (openapi_types.UUID, openapi_types.UUID, openapi_types.UUID, bool) {
	segs := strings.Split(name, sep)
	if len(segs) < 3 {
		return openapi_types.UUID{}, openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	poolID, clusterID, ok := b.resolveCluster(idx, join(segs[:2]))
	if !ok {
		return openapi_types.UUID{}, openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	branchID, ok := idx.branch[join(segs[:3])]
	if !ok {
		return openapi_types.UUID{}, openapi_types.UUID{}, openapi_types.UUID{}, false
	}
	return poolID, clusterID, branchID, true
}

func unmarshalSpec(r api.DesiredResource, dst any) error {
	if len(r.Spec) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Spec, dst); err != nil {
		// Rejected, not retryable: every tick would decode the same bytes and fail
		// the same way, so a backoff only hides that the declaration is malformed.
		return reconcile.Rejected(fmt.Errorf("decode %s %q spec: %w", r.Kind, r.Name, err))
	}
	return nil
}

// getJSON issues a GET and decodes a 2xx body into out.
func (b *Backend) getJSON(ctx context.Context, call func(context.Context) (*http.Response, error), out any) error {
	resp, err := call(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return httpErr(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// mutateTask classifies an update's response and returns the task it planned, so
// applyUpdate can follow it. A nil task with a nil error means "accepted, but no
// task to follow": the body was not a Task.
//
// The status mapping differs from mutate in the one place that matters: a 409 is
// Pending, not success. On the update path it means the kind's first-writer-wins
// lock is held by a task already doing this work, and counting that as applied is
// exactly the latch this change removes. 400 stays permanent.
func (b *Backend) mutateTask(resp *http.Response) (*openapi_types.UUID, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil, reconcile.Pending(fmt.Errorf("a task already holds this resource: %s", httpErr(resp.StatusCode, body)))
	case resp.StatusCode == http.StatusBadRequest:
		return nil, reconcile.Rejected(httpErr(resp.StatusCode, body))
	case resp.StatusCode >= 300:
		return nil, httpErr(resp.StatusCode, body)
	}
	var t ampclient.Task
	if err := json.Unmarshal(body, &t); err != nil || t.Id == (openapi_types.UUID{}) {
		// A 2xx carrying no task to follow. Accepted, and the echoed generation is
		// the authority on whether the work landed anyway — so not an error.
		return nil, nil
	}
	return &t.Id, nil
}

// patchSettings issues one of the two settings PATCHes. They are SYNCHRONOUS —
// a 200 means the row is written, with no task to follow and nothing left in
// flight — which is why they do not go through applyUpdate (there is no task to
// read, record or wait on) and why a caller may treat a nil return as applied
// rather than pending.
//
// They are not the API's only synchronous verbs (createUser, createStoragePool,
// the pool rename and the pool delete plan no task either); they are the only ones
// this agent's Update path issues, and the only ones that carry a ycp_generation.
//
// Deliberately not mutate: that maps 409 to success, which is right for a create
// racing itself but would be a silent lie here. These endpoints answer no 409 at
// all, so one would mean something unmodelled, and calling it success would claim
// settings landed that never did. 400 is permanent for mutate's reason — the
// controller refused the body as written (a setting Postgres will not take, a
// daemon key it does not know) and every retry sends the same bytes.
//
// The 200's SettingsUpdateResult is deliberately dropped. It reports what running
// endpoints did with the change, and convergence here is the row: a setting stored
// but listed in not_applied, or an endpoint listed unreachable, is applied as far
// as desired state goes — it takes effect when that compute next starts.
func (b *Backend) patchSettings(ctx context.Context, call func(context.Context) (*http.Response, error)) error {
	resp, err := call(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return reconcile.Rejected(httpErr(resp.StatusCode, body))
	}
	return httpErr(resp.StatusCode, body)
}

// mutate issues a create/delete. 2xx and 409 (already exists / in
// flight) are success; any other non-2xx is an error carrying the controller's
// message so a failed task surfaces in the tick result. A 400 is Rejected: the
// controller is refusing the spec as declared (an endpoint memory quantity with
// no unit, say), so every retry sends the same body — name it on the resource
// instead of backing off forever.
//
// Updates go through mutateTask instead — see its 409 note.
func (b *Backend) mutate(ctx context.Context, call func(context.Context) (*http.Response, error)) error {
	resp, err := call(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict || resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return reconcile.Rejected(httpErr(resp.StatusCode, body))
	}
	return httpErr(resp.StatusCode, body)
}

func httpErr(code int, body []byte) error {
	var e ampclient.Error
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("HTTP %d: %s (%s)", code, e.Error.Message, e.Error.Code)
	}
	return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
}

var _ reconcile.Backend = (*Backend)(nil)

// endpointHealth returns the verdict to judge an endpoint by, and when it was
// read. The controller omits health when its pod read fails, and the agent
// cannot tell that from a controller that predates the field, so an endpoint
// that has reported health keeps its last verdict through a tick without one:
// otherwise a single failed read flips a crash-looping endpoint to ready and
// restarts its stuck timer. Falling back to the address instead would show a
// held not_ready as ready — exactly the illusion of recovery this avoids. A
// status this agent does not know is handled the same way. Nil only when the
// endpoint has never reported a verdict this agent understands.
func (b *Backend) endpointHealth(key reconcile.ResourceKey, h *ampclient.EndpointHealth) (*ampclient.EndpointHealth, time.Time) {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if h != nil && (h.Status == ampclient.Ready || h.Status == ampclient.NotReady) {
		if b.lastHealth == nil {
			b.lastHealth = map[reconcile.ResourceKey]heldHealth{}
		}
		b.lastHealth[key] = heldHealth{health: *h, at: now}
		return h, now
	}
	if prev, ok := b.lastHealth[key]; ok {
		return &prev.health, prev.at
	}
	return nil, time.Time{}
}

func (b *Backend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// endpointState reads an endpoint's readiness. The address says only that it
// was deployed once — the controller never clears it — so where there is a
// health verdict, that decides: a crash-looping pod is not_ready, and past
// StuckAfter degrades the environment with the pod's reason. With none (a
// controller that predates health, or one that has never managed to read the
// pod) the address is all there is, and it reads as ready.
//
// A verdict read more than healthStaleAfter ago keeps its state but says how old
// it is, so a held reading is not mistaken for a current one.
func endpointState(address string, h *ampclient.EndpointHealth, readAt, now time.Time) (ready bool, state, detail string) {
	if address == "" {
		return false, "provisioning", ""
	}
	if h == nil {
		return true, "ready", ""
	}
	ready = h.Status != ampclient.NotReady
	state = "ready"
	if !ready {
		state = "not_ready"
		if h.Reason != nil {
			detail = *h.Reason
		}
	}
	if now.Sub(readAt) >= healthStaleAfter {
		last := "last reading " + state
		if detail != "" {
			last = "last: " + detail
		}
		detail = fmt.Sprintf("health not read since %s; %s", readAt.UTC().Format(time.RFC3339), last)
	}
	return ready, state, detail
}
