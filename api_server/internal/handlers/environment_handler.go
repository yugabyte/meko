package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// EnvironmentHandler serves the MCCP fleet-registry API (control-plane mode):
// console/admin routes plus the agent enroll/status/desired-state surface.
type EnvironmentHandler struct {
	svc *service.EnvironmentService
}

// NewEnvironmentHandler constructs the handler.
func NewEnvironmentHandler(svc *service.EnvironmentService) *EnvironmentHandler {
	return &EnvironmentHandler{svc: svc}
}

func agentBearer(c echo.Context) string {
	return strings.TrimSpace(strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer "))
}

// ---- console / admin ----

// List returns all environments.
func (h *EnvironmentHandler) List(c echo.Context) error {
	envs, err := h.svc.List()
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"items": envs})
}

// Get returns one environment.
func (h *EnvironmentHandler) Get(c echo.Context) error {
	e, err := h.svc.Get(c.Param("id"))
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return ok(c, http.StatusOK, e)
}

// Register mints an enrollment token and stores a PENDING environment.
func (h *EnvironmentHandler) Register(c echo.Context) error {
	var req service.RegisterEnvironmentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	e, token, err := h.svc.Register(req)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	hint := "helm install meko-agent meko/meko-agent --set mccp.url=$MCCP --set enrollmentToken=" + token
	return ok(c, http.StatusCreated, map[string]any{"environment": e, "enrollment_token": token, "install_hint": hint})
}

// SetDesired replaces the environment's desired resource set.
func (h *EnvironmentHandler) SetDesired(c echo.Context) error {
	var body struct {
		Resources []models.DesiredResource `json:"resources"`
	}
	if err := c.Bind(&body); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	found, err := h.svc.SetDesired(c.Param("id"), body.Resources)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return c.NoContent(http.StatusAccepted)
}

// ---- agent surface ----

// AgentEnroll performs the enrollment-token handshake.
func (h *EnvironmentHandler) AgentEnroll(c echo.Context) error {
	var req service.EnrollRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	e, statusToken, valid, err := h.svc.EnrollWithToken(req.EnrollmentToken, req.Placement, req.ClusterFacts)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !valid {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or already-used enrollment token")
	}
	return ok(c, http.StatusOK, map[string]any{
		"environment_id": e.ID, "status_token": statusToken, "mcp_base_url": e.MCPBaseURL,
	})
}

// AgentDesiredState returns the generic desired-state resources (bearer-authed).
func (h *EnvironmentHandler) AgentDesiredState(c echo.Context) error {
	envID := c.QueryParam("envId")
	if !h.authAgent(c, envID) {
		return nil
	}
	res, err := h.svc.Desired(envID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if res == nil {
		res = []models.DesiredResource{}
	}
	return ok(c, http.StatusOK, map[string]any{
		"revision":        1,
		"target_versions": map[string]string{"agent": "v1.4.2", "mcp": "v1.4.2", "rag": "v1.4.2"},
		"resources":       res,
	})
}

// AgentStatus ingests a status report (bearer-authed).
func (h *EnvironmentHandler) AgentStatus(c echo.Context) error {
	envID := c.QueryParam("envId")
	if envID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "envId query param required")
	}
	if !h.authAgent(c, envID) {
		return nil
	}
	var rep service.StatusReport
	if err := c.Bind(&rep); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	found, err := h.svc.ApplyStatus(envID, rep)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "unknown environment")
	}
	return c.NoContent(http.StatusNoContent)
}

// authAgent validates the env-scoped status bearer; writes the error + returns false on failure.
func (h *EnvironmentHandler) authAgent(c echo.Context, envID string) bool {
	if envID == "" {
		_ = apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "envId query param required")
		return false
	}
	tok := agentBearer(c)
	if tok == "" {
		_ = apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer status token")
		return false
	}
	valid, err := h.svc.ValidateStatusToken(envID, tok)
	if err != nil {
		_ = apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return false
	}
	if !valid {
		_ = apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid status token")
		return false
	}
	return true
}
