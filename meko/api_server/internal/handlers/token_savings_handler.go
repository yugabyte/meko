package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// TokenSavingsHandler exposes read access to token-savings data recorded by
// meko-mcp-server's contextual_prominence_search MCP tool.
type TokenSavingsHandler struct {
	svc *service.TokenSavingsService
}

// NewTokenSavingsHandler creates a new TokenSavingsHandler.
func NewTokenSavingsHandler(svc *service.TokenSavingsService) *TokenSavingsHandler {
	return &TokenSavingsHandler{svc: svc}
}

// GetForDatapack handles GET /api/v1/datapacks/{datapack_id}/token-savings.
//
//	@Summary	Get token savings for a datapack
//	@Description	Lifetime token savings recorded by contextual_prominence_search: totals, a by-source breakdown (conversation/memory/knowledge_base+memory), and a single by_datapack entry for this datapack. Backed by running counters (meko_system.token_savings_summary), not a per-request aggregation.
//	@Tags		token-savings
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack UUID"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/token-savings [get]
func (h *TokenSavingsHandler) GetForDatapack(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing datapack context")
	}

	summary, err := h.svc.GetForDatapack(c.Request().Context(), dp.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, summary)
}

// GetForAccount handles GET /api/v1/me/token-savings.
//
//	@Summary	Get token savings across every datapack the caller can access
//	@Description	Lifetime rollup of token savings recorded by contextual_prominence_search across every datapack the caller's account owns or has shared access to, broken down by datapack. Backed by running counters (meko_system.token_savings_summary), not a per-request aggregation.
//	@Tags		token-savings
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/me/token-savings [get]
func (h *TokenSavingsHandler) GetForAccount(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	summary, err := h.svc.GetForAccount(c.Request().Context(), account.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, summary)
}
