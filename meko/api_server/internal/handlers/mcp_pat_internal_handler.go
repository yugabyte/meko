package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// MCPPATInternalHandler exposes the internal validation endpoint
// hit by the MCP server on every inbound tool call. Routes wired to
// this handler MUST sit behind auth.RequireInternalSecret -- there
// is no JWT / Cognito gate here.
type MCPPATInternalHandler struct {
	svc *service.MCPPATService
}

// NewMCPPATInternalHandler creates a new MCPPATInternalHandler.
func NewMCPPATInternalHandler(svc *service.MCPPATService) *MCPPATInternalHandler {
	return &MCPPATInternalHandler{svc: svc}
}

// Validate handles POST /api/v1/internal/meko-tokens/validate.
//
//	@Summary	Validate an MCP personal access token
//	@Description	Internal endpoint called by the MCP server.
//
// Always returns 200 when the request body is well-formed;
// the verdict is in the body so callers can distinguish "API server reachable,
// token invalid" from "API server unreachable".
// The optional `client_ip` / `client_user_agent` body fields let the MCP server
// forward the originating end-user's identity into the audit log; when omitted,
// the api_server falls back to the direct HTTP connection's RemoteAddr / User-Agent
// (which is the MCP pod itself, so forensic value is degraded).
//
//	@Tags		internal
//	@Accept		json
//	@Produce	json
//	@Param		X-Meko-Internal-Secret	header	string	true	"shared secret protecting the internal endpoint"
//	@Param		body	body		models.ValidatePATRequest	true	"Validation request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Router		/internal/meko-tokens/validate [post]
func (h *MCPPATInternalHandler) Validate(c echo.Context) error {
	var req models.ValidatePATRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if strings.TrimSpace(req.Token) == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "token is required")
	}

	// Prefer body-forwarded client identity (the MCP server's view of
	// the originating Cursor / Claude Desktop user) over the direct
	// HTTP connection metadata, which only describes the MCP server
	// pod itself.
	actorIP := strings.TrimSpace(req.ClientIP)
	if actorIP == "" {
		actorIP = clientIP(c)
	}
	actorUA := strings.TrimSpace(req.ClientUserAgent)
	if actorUA == "" {
		actorUA = c.Request().UserAgent()
	}

	resp, err := h.svc.ValidatePAT(c.Request().Context(), service.MCPPATValidateInput{
		Plaintext:      req.Token,
		ActorIP:        actorIP,
		ActorUserAgent: actorUA,
	})
	if err != nil {
		slog.Error("mcp pat validate: storage failure",
			"remote_addr", c.Request().RemoteAddr, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "validator unavailable")
	}

	// We deliberately do NOT log the token / hash. The token_id is
	// safe (it's the row PK, not a secret) and the verdict gives the
	// MCP server enough to trace.
	slog.Debug("mcp pat validated",
		"valid", resp.Valid,
		"reason", resp.Reason,
		"token_id", resp.TokenID,
		"scopes", resp.Scopes,
	)

	return ok(c, http.StatusOK, resp)
}
