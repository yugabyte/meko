package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

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
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
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

// ListGET handles GET /api/v1/promoted-memories.
//
//	@Summary	List promoted memories (paginated)
//	@Tags		memories
//	@Produce	json
//	@Param		datapack_id	query	string	true	"Datapack ID"
//	@Param		page		query	integer	false	"Page number (1-based, default 1)"
//	@Param		page_size	query	integer	false	"Items per page (default 20, max 100)"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/promoted-memories [get]
func (h *PromoteMemoriesHandler) ListGET(c echo.Context) error {
	datapackID := strings.TrimSpace(c.QueryParam("datapack_id"))
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("page_size"))
	filter := models.PromotedMemoryListFilter{
		DatapackID: datapackID,
		Page:       page,
		PageSize:   pageSize,
	}

	slog.Info("listing promoted memories",
		"layer", "handler",
		"datapack_id", filter.DatapackID,
	)

	response, err := h.svc.List(c.Request().Context(), filter)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, response)
}
