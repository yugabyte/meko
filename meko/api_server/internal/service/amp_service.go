package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"meko/api_server/internal/repository"
)

// ampDefaultMilliCores is AMP's default writer size (2 cores) — what an endpoint
// with no explicit CPU reservation actually consumes, so it must count toward a
// user's total when summing compute.
const ampDefaultMilliCores int64 = 2000

// ParseMilliCores parses a Kubernetes CPU quantity ("2", "0.5", "500m") into
// milli-cores. Empty or unparseable falls back to AMP's 2-core default, so we
// never under-count compute against a tier limit.
func ParseMilliCores(cpu string) int64 {
	cpu = strings.TrimSpace(cpu)
	if cpu == "" {
		return ampDefaultMilliCores
	}
	q, err := resource.ParseQuantity(cpu)
	if err != nil {
		return ampDefaultMilliCores
	}
	return q.MilliValue()
}

// ErrAMPNotRegistered distinguishes "no AMP controller registered" from
// controller failures, so handlers can map it to a dedicated status/code
// instead of the catch-all AMP_ERROR and clients (UI, MCP, inference
// gateway) don't have to string-match the message.
var ErrAMPNotRegistered = errors.New(
	"AMP is not registered; an admin must register the AMP controller first")

// AMPService orchestrates AMP database provisioning for a datapack. It owns the
// datapack->pool mapping (meko_system.amp_datapack_pool) and drives the AMP
// controller via AMPClient. The controller endpoint is resolved at request time
// from the admin-registered row (meko_system.amp_registration) — not from an
// env var — so operators register AMP dynamically. AMP is the system of record
// for pools/clusters; Meko only records the datapack link.
type AMPService struct {
	repo       *repository.AMPRepo
	reg        *repository.AMPRegistrationRepo
	httpClient *http.Client
}

func NewAMPService(repo *repository.AMPRepo, reg *repository.AMPRegistrationRepo) *AMPService {
	// One shared http.Client (and thus one connection pool) for every AMP call.
	return &AMPService{
		repo:       repo,
		reg:        reg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// client builds an AMP client from the registered controller endpoint, or
// errors if AMP has not been registered yet. The shared http.Client is reused.
func (s *AMPService) client(ctx context.Context) (*AMPClient, error) {
	reg, err := s.reg.Get(ctx)
	if err != nil {
		return nil, err
	}
	if reg == nil || strings.TrimSpace(reg.BaseURL) == "" {
		return nil, ErrAMPNotRegistered
	}
	return NewAMPClient(reg.BaseURL, s.httpClient), nil
}

/* ---- clusters ---- */

// CreateClusterInput is the request to create an AMP cluster (a Postgres
// database — an AMP "cluster") in a datapack.
type CreateClusterInput struct {
	DatapackID   string
	DatapackName string // human-readable datapack name; used to name the AMP pool
	UserID       string // Meko user id (also the AMP user id)
	Name         string
	PgVersion    *int   // optional; omit to take AMP's default (18, the only supported version today)
	CPU          string // writer sizing (Kubernetes quantity, e.g. "2"); only used with_writer
	Memory       string // writer sizing (e.g. "4Gi"); only used with_writer
	// Engine is the optional cluster engine ("postgres" default, "aidb",
	// "oracle") — a named bundle of extensions on the controller. Extensions is
	// the optional set of catalog extensions to seed on the default branch; it
	// composes with the engine's bundle. Both are opt-in: empty = plain Postgres.
	Engine     string
	Extensions []string
}

// ampPoolName is the name given to a datapack's AMP shared pool. storage_pool.name
// is UNIQUE in the controller and datapack names are only unique per user, so a
// short slice of the (globally unique) datapack id is appended to keep the pool
// name unique while staying recognizable in the AMP console (e.g.
// "meko:basketball-test-e1dd8f55"). Falls back to the full id when the datapack
// name is empty.
func ampPoolName(datapackName, datapackID string) string {
	if n := strings.TrimSpace(datapackName); n != "" {
		short := datapackID
		if len(short) > 8 {
			short = short[:8]
		}
		return "meko:" + n + "-" + short
	}
	return "meko-" + datapackID
}

// CreateCluster ensures the datapack's AMP shared pool exists (first-time,
// idempotent) and then creates a cluster in it. The pool id is the datapack id
// by construction, so there is nothing to reconcile.
func (s *AMPService) CreateCluster(ctx context.Context, in CreateClusterInput) (AMPCluster, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return AMPCluster{}, fmt.Errorf("cluster name is required")
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPCluster{}, err
	}

	// Step 1 — ensure the AMP user + shared pool for this datapack (idempotent).
	if err := c.EnsureUser(ctx, in.UserID); err != nil {
		return AMPCluster{}, fmt.Errorf("ensure amp user: %w", err)
	}
	pool, err := c.EnsurePool(ctx, in.DatapackID, in.UserID, ampPoolName(in.DatapackName, in.DatapackID))
	if err != nil {
		// Never downgrade an already-ready pool: EnsurePool re-runs on every
		// create, so a transient blip here must not drop a live datapack's
		// compute out of the core-limit sum. MarkPoolFailed guards the 'ready' row.
		_ = s.repo.MarkPoolFailed(ctx, in.DatapackID, in.DatapackID, in.UserID)
		return AMPCluster{}, fmt.Errorf("ensure amp pool: %w", err)
	}
	if err := s.repo.Upsert(ctx, in.DatapackID, pool.ID, in.UserID, "ready"); err != nil {
		return AMPCluster{}, err
	}

	// Step 2 — create the cluster (an AMP cluster) in the pool.
	return c.CreateCluster(ctx, pool.ID, name, in.PgVersion, in.Engine, in.Extensions)
}

// CreateClusterWithWriter is the composite "create cluster" the wizard uses: it
// ensures the pool (idempotent), then runs ONE background task that provisions
// the database, its `main` branch, and a read_write endpoint together
// (create_cluster with_writer). Like CreateCluster it polls the task to
// completion and returns the materialized cluster.
func (s *AMPService) CreateClusterWithWriter(ctx context.Context, in CreateClusterInput) (AMPCluster, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return AMPCluster{}, fmt.Errorf("cluster name is required")
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPCluster{}, err
	}

	// Step 1 — ensure the AMP user + shared pool for this datapack (idempotent).
	if err := c.EnsureUser(ctx, in.UserID); err != nil {
		return AMPCluster{}, fmt.Errorf("ensure amp user: %w", err)
	}
	pool, err := c.EnsurePool(ctx, in.DatapackID, in.UserID, ampPoolName(in.DatapackName, in.DatapackID))
	if err != nil {
		// Never downgrade an already-ready pool: EnsurePool re-runs on every
		// create, so a transient blip here must not drop a live datapack's
		// compute out of the core-limit sum. MarkPoolFailed guards the 'ready' row.
		_ = s.repo.MarkPoolFailed(ctx, in.DatapackID, in.DatapackID, in.UserID)
		return AMPCluster{}, fmt.Errorf("ensure amp pool: %w", err)
	}
	if err := s.repo.Upsert(ctx, in.DatapackID, pool.ID, in.UserID, "ready"); err != nil {
		return AMPCluster{}, err
	}

	// Step 2 — run the composite task (cluster + main branch + writer).
	return c.CreateClusterWithWriter(ctx, pool.ID, name, in.PgVersion, in.CPU, in.Memory, in.Engine, in.Extensions)
}

// ListClusters returns the clusters in a datapack's AMP pool. If no pool has
// been provisioned yet, it returns an empty slice (no AMP call).
func (s *AMPService) ListClusters(ctx context.Context, datapackID string) ([]AMPCluster, error) {
	// Resolve the controller registration BEFORE the no-pool early return:
	// list is the availability probe agents run ahead of a create, and an
	// unprovisioned datapack (no pool yet) is exactly the first-time case —
	// it must answer AMP_NOT_REGISTERED, not an empty list, when no
	// controller is registered.
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	mapping, err := s.repo.GetByDatapack(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return []AMPCluster{}, nil
	}
	return c.ListClusters(ctx, mapping.SharedPoolID)
}

// SumMilliCoresByOwner totals the AMP compute (milli-cores) a user has reserved
// across ALL their datapacks — every endpoint on every branch of every cluster
// in every ready pool they own. Backs the per-tier core limit and the
// core-budget probe. Returns 0 (no error) when the user owns no pools.
//
// This fans out to the controller (pools × clusters × branches list calls), so
// it runs only on the create / add-compute / resize preflight and the budget
// endpoint — never per request.
//
// A pool the controller cannot list is SKIPPED (counted as 0), not fatal:
// amp_datapack_pool.status='ready' reflects Meko's last successful EnsurePool,
// NOT current controller reachability, so one stale/unreachable pool must never
// block create/resize for every OTHER healthy datapack the same user owns.
// Worst case is a transient under-count of a soft cap — availability over a hard
// account-wide block. Only a failure to even enumerate the user's pools (the DB
// read, or resolving the controller registration) is returned as an error.
func (s *AMPService) SumMilliCoresByOwner(ctx context.Context, userID string) (int64, error) {
	used, _, err := s.sumCores(ctx, userID, "")
	return used, err
}

// sumCores totals the user's reserved compute (milli-cores) and, when
// creditComputeID is non-empty, also returns THAT endpoint's current CPU as
// `credit` (0 if it wasn't found — e.g. its pool was skipped). Capturing the
// credit in the same fan-out is what lets the resize check credit the endpoint's
// current size without a second controller round-trip that could fail open.
//
// A pool the controller cannot list is SKIPPED (see SumMilliCoresByOwner). The
// credit accounting stays correct either way: if the resized endpoint's pool is
// skipped, both `used` (its old cores) and `credit` omit it, so used − credit +
// new still equals (other endpoints) + new — the right post-resize total.
func (s *AMPService) sumCores(ctx context.Context, userID, creditComputeID string) (used, credit int64, err error) {
	pools, err := s.repo.ListByOwner(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	if len(pools) == 0 {
		return 0, 0, nil
	}
	c, err := s.client(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, p := range pools {
		poolUsed, poolCredit, perr := sumPoolMilliCores(ctx, c, p.SharedPoolID, creditComputeID)
		if perr != nil {
			slog.Warn("sum cores: skipping pool the controller could not list",
				"pool", p.SharedPoolID, "user", userID, "error", perr)
			continue
		}
		used += poolUsed
		credit += poolCredit // at most one endpoint matches, in at most one pool
	}
	return used, credit, nil
}

// sumPoolMilliCores totals reserved CPU across one pool's clusters → branches →
// endpoints, and returns creditComputeID's current CPU as `credit` if that
// endpoint is in this pool (0 otherwise). A controller error aborts THIS pool
// only, so its caller can skip it without losing the rest of the user's pools.
func sumPoolMilliCores(ctx context.Context, c *AMPClient, poolID, creditComputeID string) (used, credit int64, err error) {
	clusters, err := c.ListClusters(ctx, poolID)
	if err != nil {
		return 0, 0, fmt.Errorf("list clusters: %w", err)
	}
	for _, cl := range clusters {
		branches, err := c.ListBranches(ctx, poolID, cl.ID)
		if err != nil {
			return 0, 0, fmt.Errorf("list branches (cluster %s): %w", cl.ID, err)
		}
		for _, b := range branches {
			endpoints, err := c.ListEndpoints(ctx, poolID, cl.ID, b.BranchID)
			if err != nil {
				return 0, 0, fmt.Errorf("list endpoints (branch %s): %w", b.BranchID, err)
			}
			for _, e := range endpoints {
				cpu := ""
				if e.Resources != nil {
					cpu = e.Resources.CPU
				}
				milli := ParseMilliCores(cpu)
				used += milli
				if creditComputeID != "" && e.ID == creditComputeID {
					credit = milli
				}
			}
		}
	}
	return used, credit, nil
}

// CoreBudgetExceeded reports whether reserving addMilli more compute (a fresh
// create / add-compute — no credit) would push the user's total vCPU across ALL
// their datapacks over limitMilli. Unlimited tiers (limitMilli == TierUnlimited)
// short-circuit before the controller fan-out. The resize path credits the
// endpoint's current size instead — see ResizeCoreBudgetExceeded.
//
// KNOWN LIMITATION (check-then-act): the read (SumMilliCoresByOwner) and the
// caller's create/resize are not atomic, so two of the same user's requests
// racing (two tabs, a retry) can each observe the same `used` and both pass,
// jointly overshooting the cap. Closing this fully would need an atomic
// reservation counter, but that counter would be a shadow of AMP's own compute
// state (AMP, not Meko, is the source of truth) and could drift. This is a soft
// business cap, not a security boundary, so the small concurrent-overshoot
// window is accepted rather than adding a drift-prone counter. Tracked as a
// follow-up.
func (s *AMPService) CoreBudgetExceeded(ctx context.Context, userID string, limitMilli, addMilli int64) (bool, error) {
	if limitMilli == TierUnlimited {
		return false, nil
	}
	used, err := s.SumMilliCoresByOwner(ctx, userID)
	if err != nil {
		return false, err
	}
	return used+addMilli > limitMilli, nil
}

// ResizeCoreBudgetExceeded is CoreBudgetExceeded for a resize: it credits the
// endpoint's CURRENT CPU (SumMilliCoresByOwner already counts it) and checks
// whether swapping in newMilli would exceed limitMilli. The credit is read from
// the SAME fan-out as the sum (sumCores captures computeID's cores), so there is
// no separate GetCompute that could fail — and therefore no branch that quietly
// drops enforcement for a growth. Any hard sum failure (DB / registration) is
// returned so the caller refuses the resize (fails closed), exactly like create.
func (s *AMPService) ResizeCoreBudgetExceeded(ctx context.Context, userID, computeID string, limitMilli, newMilli int64) (bool, error) {
	if limitMilli == TierUnlimited {
		return false, nil
	}
	used, credit, err := s.sumCores(ctx, userID, computeID)
	if err != nil {
		return false, err
	}
	return used-credit+newMilli > limitMilli, nil
}

// GetCluster reads a single cluster (database) in a datapack's AMP pool.
func (s *AMPService) GetCluster(ctx context.Context, datapackID, clusterID string) (AMPCluster, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPCluster{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPCluster{}, err
	}
	return c.GetCluster(ctx, poolID, clusterID)
}

// DeleteCluster deletes a cluster (database), cascading to its branches and
// compute. Polls the AMP task to completion before returning.
func (s *AMPService) DeleteCluster(ctx context.Context, datapackID, clusterID string) error {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return err
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	return c.DeleteCluster(ctx, poolID, clusterID)
}

// CleanupForDeletedDatapack tears down a datapack's AMP footprint when the
// datapack is deleted. Fast by design:
//   - No pool ever provisioned → instant no-op.
//   - Pool exists but has NO active clusters → just drop the mapping row; no AMP
//     delete calls are made, so datapack deletion is effectively instant.
//   - Pool has clusters → fire each cluster delete WITHOUT polling (the cascade
//     runs on the controller), then drop the mapping row. Datapack deletion is
//     not blocked minutes waiting on the teardown to finish.
//
// The shared pool itself is left in place — AMP has no delete-pool API — but it
// is emptied. If issuing any cluster delete fails synchronously, the mapping row
// is kept so a later reconcile can retry.
func (s *AMPService) CleanupForDeletedDatapack(ctx context.Context, datapackID string) error {
	mapping, err := s.repo.GetByDatapack(ctx, datapackID)
	if err != nil {
		return err
	}
	if mapping == nil {
		return nil // never provisioned; nothing to clean up
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	clusters, err := c.ListClusters(ctx, mapping.SharedPoolID)
	if err != nil {
		return fmt.Errorf("list clusters for cleanup: %w", err)
	}
	// No active clusters — skip AMP entirely and just drop the mapping.
	if len(clusters) == 0 {
		return s.repo.DeleteByDatapack(ctx, datapackID)
	}
	// Fire each cluster delete without polling: the cascade runs on the
	// controller so datapack deletion returns promptly.
	var errs []string
	for _, p := range clusters {
		if err := c.DeleteClusterAsync(ctx, mapping.SharedPoolID, p.ID); err != nil {
			errs = append(errs, fmt.Sprintf("delete cluster %s: %v", p.ID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("amp cleanup: %s", strings.Join(errs, "; "))
	}
	return s.repo.DeleteByDatapack(ctx, datapackID)
}

// poolID resolves a datapack's AMP shared pool id, erroring if none exists yet
// (branch/compute operations require an already-provisioned database).
func (s *AMPService) poolID(ctx context.Context, datapackID string) (string, error) {
	mapping, err := s.repo.GetByDatapack(ctx, datapackID)
	if err != nil {
		return "", err
	}
	if mapping == nil {
		return "", fmt.Errorf("no AMP pool for this datapack yet")
	}
	switch mapping.Status {
	case "ready":
		return mapping.SharedPoolID, nil
	case "failed":
		return "", fmt.Errorf("AMP pool provisioning failed for this datapack; recreate the database")
	default:
		return "", fmt.Errorf("AMP pool is not ready (status: %s)", mapping.Status)
	}
}

// ListBranches returns a database's branch tree.
func (s *AMPService) ListBranches(ctx context.Context, datapackID, clusterID string) ([]AMPBranch, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListBranches(ctx, poolID, clusterID)
}

// GetBranch reads a single branch of a database.
func (s *AMPService) GetBranch(ctx context.Context, datapackID, clusterID, branchID string) (AMPBranch, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPBranch{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPBranch{}, err
	}
	return c.GetBranch(ctx, poolID, clusterID, branchID)
}

// DeleteBranch deletes a branch, cascading to its compute. Polls the AMP task to
// completion; a branch with child branches is rejected (conflict).
func (s *AMPService) DeleteBranch(ctx context.Context, datapackID, clusterID, branchID string) error {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return err
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	return c.DeleteBranch(ctx, poolID, clusterID, branchID)
}

// CreateBranch forks a branch of a database.
func (s *AMPService) CreateBranch(ctx context.Context, datapackID, clusterID string, in CreateBranchInput) (AMPBranch, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPBranch{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPBranch{}, err
	}
	return c.CreateBranch(ctx, poolID, clusterID, in)
}

// ListCompute returns the compute instances on a branch.
func (s *AMPService) ListCompute(ctx context.Context, datapackID, clusterID, branchID string) ([]AMPEndpoint, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListEndpoints(ctx, poolID, clusterID, branchID)
}

// GetCompute reads a single compute instance on a branch.
func (s *AMPService) GetCompute(ctx context.Context, datapackID, clusterID, branchID, computeID string) (AMPEndpoint, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPEndpoint{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPEndpoint{}, err
	}
	return c.GetEndpoint(ctx, poolID, clusterID, branchID, computeID)
}

// DeleteCompute retires a compute instance on a branch. Polls the AMP task to
// completion before returning.
func (s *AMPService) DeleteCompute(ctx context.Context, datapackID, clusterID, branchID, computeID string) error {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return err
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	return c.DeleteEndpoint(ctx, poolID, clusterID, branchID, computeID)
}

// CreateCompute provisions a compute instance on a branch.
func (s *AMPService) CreateCompute(ctx context.Context, datapackID, clusterID, branchID, role, cpu, memory string) (AMPEndpoint, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPEndpoint{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPEndpoint{}, err
	}
	return c.CreateEndpoint(ctx, poolID, clusterID, branchID, role, cpu, memory)
}

// ResizeCompute changes a compute instance's cpu/memory in place, returning the
// endpoint at its new size and the in-place outcome.
func (s *AMPService) ResizeCompute(ctx context.Context, datapackID, clusterID, branchID, computeID, cpu, memory string) (ResizeResult, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return ResizeResult{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return ResizeResult{}, err
	}
	return c.ResizeEndpoint(ctx, poolID, clusterID, branchID, computeID, cpu, memory)
}
