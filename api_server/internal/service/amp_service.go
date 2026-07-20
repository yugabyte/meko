package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meko/api_server/internal/repository"
)

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
		return nil, fmt.Errorf("AMP is not registered; an admin must register the AMP controller first")
	}
	return NewAMPClient(reg.BaseURL, s.httpClient), nil
}

/* ---- clusters ---- */

// CreateClusterInput is the request to create an AMP cluster (a Postgres
// database — an AMP "project") in a datapack.
type CreateClusterInput struct {
	DatapackID   string
	DatapackName string // human-readable datapack name; used to name the AMP pool
	UserID       string // Meko user id (also the AMP user id)
	Name         string
	PgVersion    *int // optional; AMP defaults to 17
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
func (s *AMPService) CreateCluster(ctx context.Context, in CreateClusterInput) (AMPProject, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return AMPProject{}, fmt.Errorf("cluster name is required")
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPProject{}, err
	}

	// Step 1 — ensure the AMP user + shared pool for this datapack (idempotent).
	if err := c.EnsureUser(ctx, in.UserID); err != nil {
		return AMPProject{}, fmt.Errorf("ensure amp user: %w", err)
	}
	pool, err := c.EnsurePool(ctx, in.DatapackID, in.UserID, ampPoolName(in.DatapackName, in.DatapackID))
	if err != nil {
		_ = s.repo.Upsert(ctx, in.DatapackID, in.DatapackID, in.UserID, "failed")
		return AMPProject{}, fmt.Errorf("ensure amp pool: %w", err)
	}
	if err := s.repo.Upsert(ctx, in.DatapackID, pool.ID, in.UserID, "ready"); err != nil {
		return AMPProject{}, err
	}

	// Step 2 — create the cluster (an AMP project) in the pool.
	return c.CreateProject(ctx, pool.ID, name, in.PgVersion)
}

// ListClusters returns the clusters in a datapack's AMP pool. If no pool has
// been provisioned yet, it returns an empty slice (no AMP call).
func (s *AMPService) ListClusters(ctx context.Context, datapackID string) ([]AMPProject, error) {
	mapping, err := s.repo.GetByDatapack(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return []AMPProject{}, nil
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListProjects(ctx, mapping.SharedPoolID)
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
func (s *AMPService) ListBranches(ctx context.Context, datapackID, projectID string) ([]AMPBranch, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListBranches(ctx, poolID, projectID)
}

// CreateBranch forks a branch of a database.
func (s *AMPService) CreateBranch(ctx context.Context, datapackID, projectID string, in CreateBranchInput) (AMPBranch, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPBranch{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPBranch{}, err
	}
	return c.CreateBranch(ctx, poolID, projectID, in)
}

// ListCompute returns the compute instances on a branch.
func (s *AMPService) ListCompute(ctx context.Context, datapackID, projectID, branchID string) ([]AMPEndpoint, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return nil, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListEndpoints(ctx, poolID, projectID, branchID)
}

// CreateCompute provisions a compute instance on a branch.
func (s *AMPService) CreateCompute(ctx context.Context, datapackID, projectID, branchID, role string) (AMPEndpoint, error) {
	poolID, err := s.poolID(ctx, datapackID)
	if err != nil {
		return AMPEndpoint{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return AMPEndpoint{}, err
	}
	return c.CreateEndpoint(ctx, poolID, projectID, branchID, role)
}
