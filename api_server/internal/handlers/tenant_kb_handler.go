package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

type TenantKBHandler struct {
	svc *service.TenantKBService
}

func NewTenantKBHandler(svc *service.TenantKBService) *TenantKBHandler {
	return &TenantKBHandler{svc: svc}
}

// Sync handles POST /api/v1/tenant-kb/sync.
//
//	@Summary	Sync mem0 memories into knowledge_base
//	@Tags		tenant-kb
//	@Accept		json
//	@Produce	json
//	@Param		body	body		models.TenantKBSyncRequest	true	"optional user_id + optional agent_id + datapack_id + list of mem0 memory IDs to sync"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Router		/tenant-kb/sync [post]
func (h *TenantKBHandler) Sync(c echo.Context) error {
	var req models.TenantKBSyncRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	result, err := h.svc.Sync(c.Request().Context(), req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, result)
}
