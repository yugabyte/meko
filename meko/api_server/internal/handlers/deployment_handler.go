package handlers

import (
	"errors"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// DeploymentHandler holds Echo handlers for agent-deployment endpoints.
type DeploymentHandler struct {
	svc *service.DeploymentService
}

// NewDeploymentHandler creates a new DeploymentHandler.
func NewDeploymentHandler(svc *service.DeploymentService) *DeploymentHandler {
	return &DeploymentHandler{svc: svc}
}

type createDeploymentRequest struct {
	AgentSlug string `json:"agent_slug"`
}

// Create handles POST /api/v1/deployments -- managed-mode deployment only.
// account_mode is always "managed" here; BYOC only ever goes through
// StartBYOC -> ConfirmBYOC below.
//
//	@Summary	Deploy an agent into Meko's managed AWS account
//	@Tags		deployments
//	@Accept		json
//	@Produce	json
//	@Param		body	body		createDeploymentRequest	true	"Create request"
//	@Success	202		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments [post]
func (h *DeploymentHandler) Create(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	var req createDeploymentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	dep, err := h.svc.CreateManaged(c.Request().Context(), account.ID, req.AgentSlug)
	if err != nil {
		return deploymentServiceError(c, err)
	}
	return ok(c, http.StatusAccepted, dep)
}

// List handles GET /api/v1/deployments.
//
//	@Summary	List the caller's agent deployments
//	@Tags		deployments
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments [get]
func (h *DeploymentHandler) List(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	deployments, err := h.svc.List(c.Request().Context(), account.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if deployments == nil {
		deployments = []models.Deployment{}
	}
	return ok(c, http.StatusOK, deployments)
}

// Get handles GET /api/v1/deployments/:id. Also the status-poll target the
// frontend hits on a 2s interval -- it only ever reads the row, never calls
// AWS itself, so it stays fast under polling.
//
//	@Summary	Get one agent deployment
//	@Tags		deployments
//	@Produce	json
//	@Param		id	path		string	true	"Deployment ID"
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	404	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments/{id} [get]
func (h *DeploymentHandler) Get(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	dep, err := h.svc.Get(c.Request().Context(), account.ID, c.Param("id"))
	if err != nil {
		return deploymentServiceError(c, err)
	}
	if dep == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "deployment not found")
	}
	return ok(c, http.StatusOK, dep)
}

// Delete handles DELETE /api/v1/deployments/:id.
//
//	@Summary	Delete an agent deployment
//	@Tags		deployments
//	@Produce	json
//	@Param		id	path		string	true	"Deployment ID"
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	404	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments/{id} [delete]
func (h *DeploymentHandler) Delete(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	if err := h.svc.Delete(c.Request().Context(), account.ID, c.Param("id")); err != nil {
		return deploymentServiceError(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"message": "deployment deleted"})
}

// Invoke handles POST /api/v1/deployments/:id/invoke -- proxies the raw
// request body (already shaped as the agent's own payload contract, see
// the agent's own README) straight through to InvokeAgentRuntime, and
// streams the response back verbatim as it arrives, instead of buffering
// it. The request body is opaque to this handler and the service layer
// alike -- neither parses or validates it, since its shape belongs to the
// deployed agent's own contract, not api_server's.
//
//	@Summary	Invoke a deployed agent and stream its response
//	@Tags		deployments
//	@Accept		json
//	@Produce	text/event-stream
//	@Param		id		path	string	true	"Deployment ID"
//	@Param		body	body	object	true	"Opaque payload forwarded to the deployed agent"
//	@Success	200
//	@Failure	400	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Failure	404	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments/{id}/invoke [post]
func (h *DeploymentHandler) Invoke(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "could not read request body")
	}

	upstream, err := h.svc.Invoke(c.Request().Context(), account.ID, c.Param("id"), body)
	if err != nil {
		return deploymentServiceError(c, err)
	}
	defer upstream.Close()

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("X-Accel-Buffering", "no") // in case a reverse proxy sits in front
	resp.WriteHeader(http.StatusOK)

	flusher, canFlush := resp.Writer.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := upstream.Read(buf)
		if n > 0 {
			if _, writeErr := resp.Write(buf[:n]); writeErr != nil {
				return nil // client disconnected; nothing more to do
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			// Stream broke mid-flight -- response headers (200) are already
			// sent, so we can't send an error status now. Just stop.
			return nil
		}
	}
}

type startByocDeploymentRequest struct {
	AgentSlug string `json:"agent_slug"`
}

type byocInitResponse struct {
	ID              string `json:"id"`
	ExternalID      string `json:"external_id"`
	TrustPolicyJSON string `json:"trust_policy_json"`
}

// StartBYOC handles POST /api/v1/deployments/byoc/init -- BYOC onboarding
// step 1: mints external_id, creates a pending deployment row, and returns
// the trust-policy JSON the caller pastes into their own AWS account.
//
//	@Summary	Start BYOC deployment onboarding
//	@Tags		deployments
//	@Accept		json
//	@Produce	json
//	@Param		body	body		startByocDeploymentRequest	true	"Start BYOC request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments/byoc/init [post]
func (h *DeploymentHandler) StartBYOC(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	var req startByocDeploymentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	res, err := h.svc.StartBYOC(c.Request().Context(), account.ID, req.AgentSlug)
	if err != nil {
		return deploymentServiceError(c, err)
	}
	var externalID string
	if res.Deployment.ExternalID != nil {
		externalID = *res.Deployment.ExternalID
	}
	return ok(c, http.StatusOK, byocInitResponse{
		ID:              res.Deployment.ID,
		ExternalID:      externalID,
		TrustPolicyJSON: res.TrustPolicyJSON,
	})
}

type confirmByocDeploymentRequest struct {
	RoleARN string `json:"role_arn"`
}

// ConfirmBYOC handles POST /api/v1/deployments/:id/byoc/confirm -- BYOC
// onboarding step 2: validates + live-verifies the caller-supplied role
// ARN, stores it, and starts provisioning.
//
//	@Summary	Confirm BYOC deployment role and start provisioning
//	@Tags		deployments
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string							true	"Deployment ID"
//	@Param		body	body		confirmByocDeploymentRequest	true	"Confirm BYOC request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/deployments/{id}/byoc/confirm [post]
func (h *DeploymentHandler) ConfirmBYOC(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	var req confirmByocDeploymentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	dep, err := h.svc.ConfirmBYOC(c.Request().Context(), account.ID, c.Param("id"), req.RoleARN)
	if err != nil {
		return deploymentServiceError(c, err)
	}
	return ok(c, http.StatusOK, dep)
}

func deploymentServiceError(c echo.Context, err error) error {
	if errors.Is(err, service.ErrValidation) {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	}
	if isNotFoundErr(err) {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", err.Error())
	}
	return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
}
