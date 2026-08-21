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
package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// children need in their request paths.
type index struct {
	pool     map[string]openapi_types.UUID // "<pool>"
	cluster  map[string]openapi_types.UUID // "<pool>/<cluster>"
	branch   map[string]openapi_types.UUID // "<pool>/<cluster>/<branch>"
	endpoint map[string]openapi_types.UUID // full path
}

func newIndex() index {
	return index{
		pool:     map[string]openapi_types.UUID{},
		cluster:  map[string]openapi_types.UUID{},
		branch:   map[string]openapi_types.UUID{},
		endpoint: map[string]openapi_types.UUID{},
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
			out = append(out, reconcile.Actual{
				Key:    reconcile.ResourceKey{Kind: KindCluster, Name: clusterPath},
				Parent: reconcile.ResourceKey{Kind: KindPool, Name: poolPath},
				Ready:  true,
				State:  "ready",
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
				out = append(out, reconcile.Actual{
					Key:    reconcile.ResourceKey{Kind: KindBranch, Name: branchPath},
					Parent: reconcile.ResourceKey{Kind: KindCluster, Name: clusterPath},
					Ready:  true,
					State:  "ready",
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

// Update supports endpoint resize; the other kinds have no controller update
// path, so a spec change to them is a permanent (non-retryable) error the
// engine surfaces as drift rather than retrying forever.
func (b *Backend) Update(ctx context.Context, r api.DesiredResource) error {
	if r.Kind != KindEndpoint {
		return reconcile.Permanent(fmt.Errorf("update not supported for kind %q (only endpoint resize)", r.Kind))
	}
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
// message so a failed task surfaces in the tick result.
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
