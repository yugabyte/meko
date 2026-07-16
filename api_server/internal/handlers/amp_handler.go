package handlers

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// AMPHandler holds Echo handlers for the AMP (Postgres databases) endpoints
// scoped to a datapack. Registered only when the AMP feature is enabled.
type AMPHandler struct {
	svc *service.AMPService
}

func NewAMPHandler(svc *service.AMPService) *AMPHandler {
	return &AMPHandler{svc: svc}
}

type createDatabaseRequest struct {
	Name      string `json:"name"`
	PgVersion *int   `json:"pg_version,omitempty"`
}

// CreateDatabase handles POST /api/v1/datapacks/:datapack_id/databases.
// Ensures the datapack's AMP shared pool (first time) then creates a database.
func (h *AMPHandler) CreateDatabase(c echo.Context) error {
	datapackID := c.Param("datapack_id")
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "datapack_id is required")
	}
	var req createDatabaseRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "database name is required")
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

	project, err := h.svc.CreateDatabase(c.Request().Context(), service.CreateDatabaseInput{
		DatapackID:   datapackID,
		DatapackName: datapackName,
		UserID:       account.ID,
		Name:         req.Name,
		PgVersion:    req.PgVersion,
	})
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusCreated, project)
}

// ListDatabases handles GET /api/v1/datapacks/:datapack_id/databases.
func (h *AMPHandler) ListDatabases(c echo.Context) error {
	datapackID := c.Param("datapack_id")
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "datapack_id is required")
	}
	projects, err := h.svc.ListDatabases(c.Request().Context(), datapackID)
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, projects)
}

// ListBranches handles GET .../databases/:project_id/branches.
func (h *AMPHandler) ListBranches(c echo.Context) error {
	branches, err := h.svc.ListBranches(c.Request().Context(), c.Param("datapack_id"), c.Param("project_id"))
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, branches)
}

type createBranchRequest struct {
	Name         string `json:"name"`
	ParentBranch string `json:"parent_branch_id,omitempty"`
	ForkPointLSN string `json:"fork_point_lsn,omitempty"`
}

// CreateBranch handles POST .../databases/:project_id/branches.
func (h *AMPHandler) CreateBranch(c echo.Context) error {
	var req createBranchRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if strings.TrimSpace(req.Name) == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", "branch name is required")
	}
	branch, err := h.svc.CreateBranch(c.Request().Context(), c.Param("datapack_id"), c.Param("project_id"),
		service.CreateBranchInput{Name: req.Name, ParentBranch: req.ParentBranch, ForkPointLSN: req.ForkPointLSN})
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusCreated, branch)
}

// ListCompute handles GET .../branches/:branch_id/compute.
func (h *AMPHandler) ListCompute(c echo.Context) error {
	instances, err := h.svc.ListCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("project_id"), c.Param("branch_id"))
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, instances)
}

type createComputeRequest struct {
	Role string `json:"role"`
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
	instance, err := h.svc.CreateCompute(c.Request().Context(), c.Param("datapack_id"), c.Param("project_id"), c.Param("branch_id"), role)
	if err != nil {
		return apiError(c, http.StatusBadGateway, "AMP_ERROR", err.Error())
	}
	return ok(c, http.StatusCreated, instance)
}
