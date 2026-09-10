package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// AMPHandler holds Echo handlers for the AMP cluster endpoints scoped to a
// datapack. A cluster is an AMP database (its default branch is `main`).
// Registered only when the AMP feature is enabled.
type AMPHandler struct {
	svc *service.AMPService
}

func NewAMPHandler(svc *service.AMPService) *AMPHandler {
	return &AMPHandler{svc: svc}
}

// ampError maps an AMP service failure to HTTP: a missing controller
// registration gets its own status/code (412 AMP_NOT_REGISTERED) so callers
// can tell "an admin must register AMP first" apart from controller errors.
func ampError(c echo.Context, err error) error {
	if errors.Is(err, service.ErrAMPNotRegistered) {
		return apiError(c, http.StatusPreconditionFailed, "AMP_NOT_REGISTERED", err.Error())
	}
	return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
}

// enforceCoreLimit returns a non-nil (402) echo error when reserving `addMilli`
// more AMP compute (create / add-compute — no credit) would push the user's
// total vCPU across ALL their datapacks over their tier cap. nil = proceed. The
// sum + threshold live in AMPService.CoreBudgetExceeded (unlimited tiers
// short-circuit there before the controller fan-out); the handler just parses,
// calls one service method, and maps the outcome. Checked BEFORE the mutating
// h.svc.* call so it returns a clean 402 instead of being remapped to 502.
func (h *AMPHandler) enforceCoreLimit(c echo.Context, userType, userID string, addMilli int64) error {
	limit := service.LimitsFor(userType).MaxMilliCores
	exceeded, err := h.svc.CoreBudgetExceeded(c.Request().Context(), userID, limit, addMilli)
	if err != nil {
		return ampError(c, err)
	}
	if exceeded {
		return coreLimitError(c, userType)
	}
	return nil
}

// enforceResizeCoreLimit is enforceCoreLimit for a resize: the service credits
// the endpoint's current CPU (captured in the same sum) against the new size.
func (h *AMPHandler) enforceResizeCoreLimit(c echo.Context, userType, userID, computeID string, newMilli int64) error {
	limit := service.LimitsFor(userType).MaxMilliCores
	exceeded, err := h.svc.ResizeCoreBudgetExceeded(c.Request().Context(), userID, computeID, limit, newMilli)
	if err != nil {
		return ampError(c, err)
	}
	if exceeded {
		return coreLimitError(c, userType)
	}
	return nil
}

// coreLimitError is the 402 for a vCPU-cap hit, via the shared tier-limit
// responder so it matches the KB/datapack cap sites.
func coreLimitError(c echo.Context, userType string) error {
	return respondTierLimit(c, service.CoreLimitResponse(userType))
}

type createClusterRequest struct {
	Name      string `json:"name"`
	PgVersion *int   `json:"pg_version,omitempty"`
	// WithWriter runs the composite "create cluster" (cluster + main branch +
	// read_write endpoint) as one background task, sized by CPU/Memory. Default
	// false keeps the cluster-only behavior (e.g. the MCP tool).
	WithWriter bool   `json:"with_writer,omitempty"`
	CPU        string `json:"cpu,omitempty"`
	Memory     string `json:"memory,omitempty"`
	// Engine is the optional cluster engine ("postgres" default, "aidb",
	// "oracle"); Extensions is the optional set of catalog extensions to seed on
	// the default branch. Both are opt-in — empty means a plain Postgres cluster.
	Engine     string   `json:"engine,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// CreateCluster handles POST /api/v1/datapacks/:datapack_id/amp/clusters.
// Ensures the datapack's AMP shared pool (first time) then creates a cluster.
// with_writer:true also provisions the `main`-branch read_write endpoint in the
// same task; either way it polls to completion and returns 201 + the cluster.
func (h *AMPHandler) CreateCluster(c echo.Context) error {
	datapackID := c.Param("datapack_id")
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "datapack_id is required")
	}
	var req createClusterRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "cluster name is required")
	}
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	// The ownership middleware resolves the datapack into context; use its name
	// so the AMP pool is named after the datapack (best-effort — a missing name
	// falls back to the id in ampPoolName).
	var datapackName string
	if dp, dpErr := auth.GetDatapack(c); dpErr == nil && dp != nil {
		datapackName = dp.Name
	}

	in := service.CreateClusterInput{
		DatapackID:   datapackID,
		DatapackName: datapackName,
		UserID:       account.ID,
		Name:         req.Name,
		PgVersion:    req.PgVersion,
		CPU:          strings.TrimSpace(req.CPU),
		Memory:       strings.TrimSpace(req.Memory),
		Engine:       strings.TrimSpace(req.Engine),
		Extensions:   req.Extensions,
	}
	// with_writer provisions the writer endpoint in the same task. Both paths
	// poll the task to completion and return the materialized cluster (201).
	var cluster service.AMPCluster
	if req.WithWriter {
		// Only with_writer reserves compute; enforce the tier vCPU cap first
		// (empty CPU means AMP's 2-core default, which ParseMilliCores counts).
		if lerr := h.enforceCoreLimit(c, account.AccountType, account.ID, service.ParseMilliCores(req.CPU)); lerr != nil {
			return lerr
		}
		cluster, err = h.svc.CreateClusterWithWriter(c.Request().Context(), in)
	} else {
		cluster, err = h.svc.CreateCluster(c.Request().Context(), in)
	}
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusCreated, cluster)
}

// ListClusters handles GET /api/v1/datapacks/:datapack_id/clusters.
func (h *AMPHandler) ListClusters(c echo.Context) error {
	datapackID := c.Param("datapack_id")
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "datapack_id is required")
	}
	clusters, err := h.svc.ListClusters(c.Request().Context(), datapackID)
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, clusters)
}

// GetCluster handles GET /api/v1/datapacks/:datapack_id/clusters/:cluster_id.
func (h *AMPHandler) GetCluster(c echo.Context) error {
	cluster, err := h.svc.GetCluster(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, cluster)
}

// DeleteCluster handles DELETE /api/v1/datapacks/:datapack_id/clusters/:cluster_id.
// Cascades to the cluster's branches and compute; blocks until the AMP task
// completes.
func (h *AMPHandler) DeleteCluster(c echo.Context) error {
	if err := h.svc.DeleteCluster(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id")); err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, map[string]any{"id": c.Param("cluster_id"), "deleted": true})
}

// ListBranches handles GET .../clusters/:cluster_id/branches.
func (h *AMPHandler) ListBranches(c echo.Context) error {
	branches, err := h.svc.ListBranches(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, branches)
}

type createBranchRequest struct {
	Name         string `json:"name"`
	ParentBranch string `json:"parent_branch_id,omitempty"`
	ForkPointLSN string `json:"fork_point_lsn,omitempty"`
}

// CreateBranch handles POST .../clusters/:cluster_id/branches.
func (h *AMPHandler) CreateBranch(c echo.Context) error {
	var req createBranchRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "branch name is required")
	}
	branch, err := h.svc.CreateBranch(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"),
		service.CreateBranchInput{Name: req.Name, ParentBranch: req.ParentBranch, ForkPointLSN: req.ForkPointLSN})
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusCreated, branch)
}

// GetBranch handles GET .../clusters/:cluster_id/branches/:branch_id.
func (h *AMPHandler) GetBranch(c echo.Context) error {
	branch, err := h.svc.GetBranch(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, branch)
}

// DeleteBranch handles DELETE .../clusters/:cluster_id/branches/:branch_id.
// Cascades to the branch's compute; blocks until the AMP task completes.
func (h *AMPHandler) DeleteBranch(c echo.Context) error {
	if err := h.svc.DeleteBranch(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id")); err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, map[string]any{"id": c.Param("branch_id"), "deleted": true})
}

// ListCompute handles GET .../branches/:branch_id/compute.
func (h *AMPHandler) ListCompute(c echo.Context) error {
	instances, err := h.svc.ListCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, instances)
}

// GetCompute handles GET .../branches/:branch_id/compute/:compute_id.
func (h *AMPHandler) GetCompute(c echo.Context) error {
	instance, err := h.svc.GetCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"), c.Param("compute_id"))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, instance)
}

// DeleteCompute handles DELETE .../branches/:branch_id/compute/:compute_id.
// Blocks until the AMP task completes.
func (h *AMPHandler) DeleteCompute(c echo.Context) error {
	if err := h.svc.DeleteCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"), c.Param("compute_id")); err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, map[string]any{"id": c.Param("compute_id"), "deleted": true})
}

// coreBudgetResponse is GET .../amp/core-budget: the user's AMP vCPU used across
// all their datapacks vs the tier cap. MaxCores is null for unlimited tiers.
type coreBudgetResponse struct {
	Tier      string   `json:"tier"`
	NextTier  string   `json:"next_tier"` // "" for pro/enterprise (no upgrade)
	UsedCores float64  `json:"used_cores"`
	MaxCores  *float64 `json:"max_cores"`
	AtLimit   bool     `json:"at_limit"`
}

// CoreBudget handles GET /datapacks/:datapack_id/amp/core-budget. User-scoped
// (uses the account, not the datapack — the route just carries auth) so the UI
// can grey the create button + show remaining vCPU. Unlimited tiers skip the
// controller fan-out. Degrades to used=0 / not-at-limit when AMP is
// unregistered or unreachable, so the UI never hard-blocks on an infra hiccup —
// the create/resize paths still enforce authoritatively.
func (h *AMPHandler) CoreBudget(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	out := coreBudgetResponse{Tier: account.AccountType, NextTier: service.NextTier(account.AccountType)}
	limitMilli := service.LimitsFor(account.AccountType).MaxMilliCores
	if limitMilli == service.TierUnlimited {
		return ok(c, http.StatusOK, out) // unlimited: no cap, no fan-out
	}
	maxCores := float64(limitMilli) / 1000
	out.MaxCores = &maxCores
	usedMilli, err := h.svc.SumMilliCoresByOwner(c.Request().Context(), account.ID)
	if err != nil {
		return ok(c, http.StatusOK, out) // best-effort; server-side enforcement is authoritative
	}
	out.UsedCores = float64(usedMilli) / 1000
	out.AtLimit = usedMilli >= limitMilli
	return ok(c, http.StatusOK, out)
}

type createComputeRequest struct {
	Role string `json:"role"`
	// CPU / Memory are optional Kubernetes quantities (e.g. "2", "4Gi") passed
	// straight through to AMP's endpoint `resources`. Empty = AMP defaults.
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// CreateCompute handles POST .../branches/:branch_id/compute.
func (h *AMPHandler) CreateCompute(c echo.Context) error {
	var req createComputeRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "read_write"
	}
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	// Adding an endpoint reserves compute — enforce the tier vCPU cap.
	if lerr := h.enforceCoreLimit(c, account.AccountType, account.ID, service.ParseMilliCores(req.CPU)); lerr != nil {
		return lerr
	}
	instance, err := h.svc.CreateCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"), role, strings.TrimSpace(req.CPU), strings.TrimSpace(req.Memory))
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusCreated, instance)
}

type resizeComputeRequest struct {
	// CPU / Memory are optional Kubernetes quantities (e.g. "4", "8Gi"). At least
	// one must be set; empty fields are left unchanged.
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// ResizeCompute handles POST .../branches/:branch_id/compute/:compute_id/resize.
// Blocks until the AMP task completes; returns the endpoint at its new size plus
// the in-place resize outcome.
func (h *AMPHandler) ResizeCompute(c echo.Context) error {
	var req resizeComputeRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	cpu, memory := strings.TrimSpace(req.CPU), strings.TrimSpace(req.Memory)
	if cpu == "" && memory == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "at least one of cpu or memory is required")
	}
	datapackID, clusterID, branchID, computeID := c.Param("datapack_id"), c.Param("cluster_id"), c.Param("branch_id"), c.Param("compute_id")
	// Only a CPU change can move a user over the vCPU cap (a memory-only resize
	// is exempt). The service credits the endpoint's current CPU against the new
	// size — captured in the same sum, keyed by compute id.
	if cpu != "" {
		account, aerr := auth.GetAccount(c)
		if aerr != nil || account == nil {
			return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
		}
		if lerr := h.enforceResizeCoreLimit(c, account.AccountType, account.ID, computeID, service.ParseMilliCores(cpu)); lerr != nil {
			return lerr
		}
	}
	result, err := h.svc.ResizeCompute(c.Request().Context(), datapackID, clusterID, branchID, computeID, cpu, memory)
	if err != nil {
		return ampError(c, err)
	}
	return ok(c, http.StatusOK, result)
}
