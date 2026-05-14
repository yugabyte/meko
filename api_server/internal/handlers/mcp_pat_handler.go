package handlers

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// MCPPATHandler exposes the self-service /me/meko-tokens endpoints
// users hit from the meko UI (and `cli_app` in the future) to mint,
// list, and revoke personal access tokens used to authenticate
// against the MCP server.
//
// All endpoints require the standard auth middleware to have run
// first; the caller's account is read from the Echo context and used
// as both the row owner and the audit-log actor.
type MCPPATHandler struct {
	svc *service.MCPPATService
}

// NewMCPPATHandler creates a new MCPPATHandler.
func NewMCPPATHandler(svc *service.MCPPATService) *MCPPATHandler {
	return &MCPPATHandler{svc: svc}
}

// Create handles POST /api/v1/me/meko-tokens.
//
//	@Summary	Create an MCP personal access token
//	@Description	Mints a new long-lived bearer token. The plaintext token is returned in the response exactly once and is never recoverable.
//	@Tags		meko-tokens
//	@Accept		json
//	@Produce	json
//	@Param		body	body		models.CreateMCPTokenRequest	true	"Token create request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Failure	429		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/me/meko-tokens [post]
func (h *MCPPATHandler) Create(c echo.Context) error {
	var req models.CreateMCPTokenRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	resp, err := h.svc.CreatePAT(c.Request().Context(), service.MCPPATCreateInput{
		UserID:         account.ID,
		Name:           req.Name,
		TTLDays:        req.TTLDays,
		Scopes:         req.Scopes,
		ActorIP:        clientIP(c),
		ActorUserAgent: c.Request().UserAgent(),
	})
	if err != nil {
		return mapMCPPATError(c, err)
	}

	slog.Info("mcp pat created (handler)",
		"user_id", account.ID,
		"id", resp.TokenRecord.ID,
		"token_prefix", resp.TokenRecord.TokenPrefix,
		"scopes", resp.TokenRecord.Scopes,
	)
	return ok(c, http.StatusCreated, resp)
}

// List handles GET /api/v1/me/meko-tokens.
//
//	@Summary	List MCP personal access tokens
//	@Description	Returns all PATs (active, expired, revoked) for the caller, newest first. Plaintext tokens are never included.
//	@Tags		meko-tokens
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/me/meko-tokens [get]
func (h *MCPPATHandler) List(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	tokens, err := h.svc.ListPATs(c.Request().Context(), account.ID)
	if err != nil {
		return mapMCPPATError(c, err)
	}
	if tokens == nil {
		tokens = []models.MCPPersonalAccessToken{}
	}
	return ok(c, http.StatusOK, tokens)
}

// Revoke handles DELETE /api/v1/me/meko-tokens/:id.
//
//	@Summary	Revoke an MCP personal access token
//	@Description	Marks the token as revoked. Idempotent: revoking an already-revoked token returns 204 (the audit row is only emitted on the first revoke).
//	@Tags		meko-tokens
//	@Param		id	path	string	true	"Token ID (UUID)"
//	@Success	204
//	@Failure	401	{object}	APIResponse
//	@Failure	404	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/me/meko-tokens/{id} [delete]
func (h *MCPPATHandler) Revoke(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "token id is required")
	}

	if err := h.svc.RevokePAT(c.Request().Context(), service.MCPPATRevokeInput{
		UserID:         account.ID,
		TokenID:        id,
		ActorIP:        clientIP(c),
		ActorUserAgent: c.Request().UserAgent(),
	}); err != nil {
		return mapMCPPATError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// mapMCPPATError translates service-layer sentinel errors into the
// standard API envelope statuses.
func mapMCPPATError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, service.ErrMCPPATValidation):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", err.Error())
	case errors.Is(err, service.ErrMCPPATCapReached):
		return apiError(c, http.StatusTooManyRequests,
			"TOKEN_CAP_REACHED", err.Error())
	case errors.Is(err, service.ErrMCPPATNameInUse):
		return apiError(c, http.StatusConflict,
			"CONFLICT", err.Error())
	case errors.Is(err, service.ErrMCPPATNotFound):
		return apiError(c, http.StatusNotFound,
			"NOT_FOUND", err.Error())
	default:
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
}

// clientIP returns the best-effort caller IP. We prefer the
// X-Forwarded-For first hop when present (Echo + LB deployments) and
// fall back to the direct RemoteAddr otherwise.
func clientIP(c echo.Context) string {
	if xff := strings.TrimSpace(c.Request().Header.Get("X-Forwarded-For")); xff != "" {
		if comma := strings.Index(xff, ","); comma >= 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return xff
	}
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		return c.Request().RemoteAddr
	}
	return host
}
