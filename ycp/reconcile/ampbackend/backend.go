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
// extensions — the externalized extensions every endpoint on it mounts — but
// only once the branch exists: amp applies them with addBranchExtensions, a
// separate call against a branch id no create has produced yet, so Create
// refuses a spec that declares them rather than dropping the field.
package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// Resource kinds an operator declares in desired state for an amp environment.
const (
	KindPool     = "storage-pool"
	KindCluster  = "cluster"
	KindBranch   = "branch"
	KindEndpoint = "endpoint"
)

const sep = "/"

// Backend drives one amp controller. Safe for concurrent use within a tick:
// List rebuilds the id index under lock, and the apply verbs only read it.
type Backend struct {
	api *ampclient.Client

	mu  sync.RWMutex
	idx index
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
}

func newIndex() index {
	return index{
		pool:      map[string]openapi_types.UUID{},
		cluster:   map[string]openapi_types.UUID{},
		branch:    map[string]openapi_types.UUID{},
		endpoint:  map[string]openapi_types.UUID{},
		branchRow: map[string]ampclient.Branch{},
	}
}

// New builds a Backend against the controller at baseURL. authToken is sent as
// a bearer when non-empty; the controller API is unauthenticated in-cluster for
// now, so it is usually empty.
func New(baseURL, authToken string, httpDoer ampclient.HttpRequestDoer) (*Backend, error) {
	opts := []ampclient.ClientOption{}
	if httpDoer != nil {
		opts = append(opts, ampclient.WithHTTPClient(httpDoer))
	}
	if authToken != "" {
		opts = append(opts, ampclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+authToken)
			return nil
		}))
	}
	c, err := ampclient.NewClient(strings.TrimRight(baseURL, "/"), opts...)
	if err != nil {
		return nil, err
	}
	return &Backend{api: c, idx: newIndex()}, nil
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
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
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
// LSN it was forked at, its lineage, and its externalized-extension set. The id
// pairs with a fork's parent_branch_id so a UI can rebuild the fork tree. The
// extension set is per branch — seeded from the cluster, inherited on fork, and
// added to later — so each branch reports its own.
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
	return m
}

// endpointAttrs reports a compute endpoint's amp id, role, connection
// addresses, and reserved compute.
func endpointAttrs(ep ampclient.Endpoint) map[string]string {
	var m map[string]string
	m = attrPut(m, "id", ep.Id.String())
	m = attrPut(m, "role", string(ep.Role))
	m = attrPut(m, "address", ep.Address)
	m = attrPut(m, "tailnet_address", ep.TailnetAddress)
	if ep.Resources != nil {
		if ep.Resources.Cpu != nil {
			m = attrPut(m, "cpu", *ep.Resources.Cpu)
		}
		if ep.Resources.Memory != nil {
			m = attrPut(m, "memory", *ep.Resources.Memory)
		}
	}
	return m
}

// List traverses pools -> clusters -> branches -> endpoints and returns every
// YCP-owned resource, rebuilding the id index as it goes.
func (b *Backend) List(ctx context.Context) ([]reconcile.Actual, error) {
	idx := newIndex()
	var out []reconcile.Actual

	var pools ampclient.StoragePoolList
	if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.ListStoragePools(c, &ampclient.ListStoragePoolsParams{})
	}, &pools); err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
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
			return nil, fmt.Errorf("list clusters in %q: %w", poolPath, err)
		}
		for _, cl := range clusters.Clusters {
			clusterPath := join([]string{poolPath, cl.Name})
			idx.cluster[clusterPath] = cl.Id
			clAttrs := attrPutInt(attrPut(nil, "id", cl.Id.String()), "pg_version", cl.DefaultBranch.PgVersion)
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
				return nil, fmt.Errorf("list branches in %q: %w", clusterPath, err)
			}
			for _, br := range branches.Branches {
				branchPath := join([]string{poolPath, cl.Name, br.Name})
				idx.branch[branchPath] = br.BranchId
				idx.branchRow[branchPath] = br
				out = append(out, reconcile.Actual{
					Key:    reconcile.ResourceKey{Kind: KindBranch, Name: branchPath},
					Parent: reconcile.ResourceKey{Kind: KindCluster, Name: clusterPath},
					Ready:  true,
					State:  "ready",
					Attrs:  branchAttrs(br),
				})

				var endpoints ampclient.EndpointList
				if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
					return b.api.ListEndpoints(c, p.Id, cl.Id, br.BranchId, &ampclient.ListEndpointsParams{})
				}, &endpoints); err != nil {
					return nil, fmt.Errorf("list endpoints in %q: %w", branchPath, err)
				}
				for _, ep := range endpoints.Endpoints {
					epName := ep.Name
					if epName == "" {
						epName = ep.Id.String()
					}
					epPath := join([]string{poolPath, cl.Name, br.Name, epName})
					idx.endpoint[epPath] = ep.Id
					ready := ep.Address != ""
					state := "provisioning"
					if ready {
						state = "ready"
					}
					out = append(out, reconcile.Actual{
						Key:    reconcile.ResourceKey{Kind: KindEndpoint, Name: epPath},
						Parent: reconcile.ResourceKey{Kind: KindBranch, Name: branchPath},
						Ready:  ready,
						State:  state,
						Attrs:  endpointAttrs(ep),
					})
				}
			}
		}
	}

	b.mu.Lock()
	b.idx = idx
	b.mu.Unlock()
	return out, nil
}

// Create issues the async create task for r. A 409 (already exists / in flight)
// is treated as success so an in-flight create is never re-driven into an
// error.
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
		req.Name = name
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
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
			return reconcile.Permanent(fmt.Errorf(
				"create branch %q: extensions cannot be declared at branch create (a new branch inherits its parent's set) — declare them once the branch exists",
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
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
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
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.CreateEndpoint(c, poolID, clusterID, branchID, req)
		})
	}
	return fmt.Errorf("create: unknown kind %q", r.Kind)
}

// Update carries the two spec changes this agent applies to a resource that
// already exists: resizing an endpoint, and enabling externalized extensions on
// a branch. Pools and clusters have no controller update path at all, so a spec
// change to one is a permanent (non-retryable) error the engine surfaces as
// drift rather than retrying forever; the branch fields Update does not apply
// are reported the same way (see updateBranch).
func (b *Backend) Update(ctx context.Context, r api.DesiredResource) error {
	switch r.Kind {
	case KindBranch:
		return b.updateBranch(ctx, r)
	case KindEndpoint:
		return b.resizeEndpoint(ctx, r)
	}
	return reconcile.Permanent(fmt.Errorf("update not supported for kind %q (only endpoint resize and branch extensions)", r.Kind))
}

func (b *Backend) resizeEndpoint(ctx context.Context, r api.DesiredResource) error {
	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	poolID, clusterID, branchID, ok := b.resolveBranch(idx, r.Name)
	if !ok {
		return fmt.Errorf("resize endpoint %q: parent branch not found", r.Name)
	}
	endpointID, ok := idx.endpoint[r.Name]
	if !ok {
		return fmt.Errorf("resize endpoint %q: endpoint not found", r.Name)
	}
	var req ampclient.ResizeEndpointRequest
	if err := unmarshalSpec(r, &req); err != nil {
		return err
	}
	return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.ResizeEndpoint(c, poolID, clusterID, branchID, endpointID, req)
	})
}

// updateBranch applies the one branch change amp can make after create — adding
// externalized extensions — and reports whatever else the spec declares that it
// could not. The add is an absolute union, so re-issuing it (a replayed tick, an
// add still in flight) is a no-op, which is what lets the engine drive it with
// no new verb; only the missing names are sent, because every add redeploys the
// branch's endpoints.
//
// Anything else the spec declares is create-time here, so a value the branch
// does not already carry is drift, not work: a name dropped from extensions
// cannot be removed at all (a branch's set only grows, deliberately, so a fork's
// endpoints keep mounting what its inherited objects need), and pg_settings has
// a controller verb this agent does not wire (see updateBranchSettings). Adds
// are issued before drift is reported, so one unfixable field does not block the
// rest.
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

	if spec.Extensions != nil {
		if add := sortedMissing(want, have); len(add) > 0 {
			if err := b.mutate(ctx, func(c context.Context) (*http.Response, error) {
				return b.api.AddBranchExtensions(c, poolID, clusterID, branchID,
					ampclient.AddBranchExtensionsRequest{Extensions: add})
			}); err != nil {
				return fmt.Errorf("add extensions %s to branch %q: %w", strings.Join(add, ","), r.Name, err)
			}
		}
	}

	var drift []string
	if spec.Extensions != nil {
		if extra := sortedMissing(have, want); len(extra) > 0 {
			drift = append(drift, "extensions "+strings.Join(extra, ",")+" are enabled and cannot be removed")
		}
	}
	drift = append(drift, unapplied(spec, br)...)
	if len(drift) > 0 {
		return reconcile.Permanent(fmt.Errorf("update branch %q: %s", r.Name, strings.Join(drift, "; ")))
	}
	return nil
}

// unapplied names the branch-spec fields whose declared value the controller does
// not report, i.e. the ones this update did not and cannot apply. Without it a
// spec whose extensions are already converged would report success on a
// generation bump that changed something else entirely, and the engine would
// latch that generation as applied.
//
// fork_point_lsn is deliberately not compared: a branch records the fork LSN the
// controller resolved, which is not textually what a caller requested, so a
// mismatch would not reliably mean drift.
func unapplied(spec branchSpec, br ampclient.Branch) []string {
	var out []string
	if spec.PgSettings != nil {
		have := map[string]string{}
		if br.PgSettings != nil {
			have = *br.PgSettings
		}
		for k, v := range *spec.PgSettings {
			if cur, ok := have[k]; !ok || cur != v {
				out = append(out, "pg_settings."+k)
			}
		}
	}
	if spec.ParentBranchId != nil && (br.ParentBranchId == nil || *br.ParentBranchId != *spec.ParentBranchId) {
		out = append(out, "parent_branch_id")
	}
	sort.Strings(out)
	return out
}

// branchSpec is a branch's desired spec as Update reads it: the field it can
// apply (extensions) plus the ones it can only check (Create decodes the whole
// thing into CreateBranchRequest instead). Every field is a pointer so an
// omitted one — unmanaged — is distinguishable from a declared empty one.
type branchSpec struct {
	Extensions     *[]string           `json:"extensions"`
	PgSettings     *map[string]string  `json:"pg_settings"`
	ParentBranchId *openapi_types.UUID `json:"parent_branch_id"`
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
// Storage pools have no delete endpoint, so a withdrawn pool is a permanent
// (non-retryable) error the engine surfaces as drift rather than retrying.
func (b *Backend) Delete(ctx context.Context, key reconcile.ResourceKey) error {
	b.mu.RLock()
	idx := b.idx
	b.mu.RUnlock()

	switch key.Kind {
	case KindPool:
		return reconcile.Permanent(fmt.Errorf("delete storage pool %q: not supported by the controller API", key.Name))
	case KindCluster:
		poolID, clusterID, ok := b.resolveCluster(idx, key.Name)
		if !ok {
			return nil // already gone
		}
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.DeleteCluster(c, poolID, clusterID)
		})
	case KindBranch:
		poolID, clusterID, branchID, ok := b.resolveBranch(idx, key.Name)
		if !ok {
			return nil
		}
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
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
		return b.mutate(ctx, func(c context.Context) (*http.Response, error) {
			return b.api.DeleteEndpoint(c, poolID, clusterID, branchID, endpointID)
		})
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
		return fmt.Errorf("decode %s %q spec: %w", r.Kind, r.Name, err)
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

// mutate issues a create/update/delete. 2xx and 409 (already exists / in
// flight) are success; any other non-2xx is an error carrying the controller's
// message so a failed task surfaces in the tick result. A 400 is permanent: the
// controller is refusing the spec as declared (an endpoint memory quantity with
// no unit, say), so every retry sends the same rejected body — surface it as
// drift instead of backing off forever.
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
		return reconcile.Permanent(httpErr(resp.StatusCode, body))
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
