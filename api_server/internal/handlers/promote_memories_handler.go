package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

type PromoteMemoriesHandler struct {
	svc *service.PromoteMemoriesService
}

func NewPromoteMemoriesHandler(svc *service.PromoteMemoriesService) *PromoteMemoriesHandler {
	return &PromoteMemoriesHandler{svc: svc}
}

// Promote handles POST /api/v1/promote-memories.
//
//	@Summary	Promote mem0 memories into the tenant knowledge base
//	@Tags		memories
//	@Accept		json
//	@Produce	json
//	@Param		body	body		models.PromoteMemoriesRequest	true	"datapack_id + list of mem0 memory IDs to promote (optional user_id / agent_id)"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Router		/promote-memories [post]
func (h *PromoteMemoriesHandler) Promote(c echo.Context) error {
	var req models.PromoteMemoriesRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	result, err := h.svc.Promote(c.Request().Context(), req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, result)
}
