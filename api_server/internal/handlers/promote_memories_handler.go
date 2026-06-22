package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
	// NOTE: re-add "errors" and "meko/api_server/internal/repository" imports
	// when the commented-out ErrMemoriesNotReady block in Promote() is re-enabled
	// (after PromoteNew lands).
)

type PromoteMemoriesHandler struct {
	svc      *service.PromoteMemoriesService
	dpLookup DatapackMembershipLookup
}

func NewPromoteMemoriesHandler(
	svc *service.PromoteMemoriesService,
	dpLookup DatapackMembershipLookup,
) *PromoteMemoriesHandler {
	return &PromoteMemoriesHandler{svc: svc, dpLookup: dpLookup}
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
	if _, err := RequireDatapackAccess(c, h.dpLookup, req.DatapackID); err != nil {
		return err
	}

	result, err := h.svc.Promote(c.Request().Context(), req)
	if err != nil {
		// Enable once the new promote-memories function (PromoteNew) is wired up.
		// if errors.Is(err, repository.ErrMemoriesNotReady) {
		// 	// Mem0 hasn't finished extracting graph entities for one
		// 	// of the memories. UI maps this to a friendly retry toast.
		// 	return apiError(c, http.StatusServiceUnavailable, "MEMORY_PROCESSING",
		// 		"This memory is still being processed by mem0. Please wait a few seconds and try again.")
		// }
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, result)
}

// KeepAsMemory handles POST /api/v1/keep-as-memory. Stamps mem0 rows
// with `meko_promotion_user_action = 'keep_as_memory'`. One-way.
func (h *PromoteMemoriesHandler) KeepAsMemory(c echo.Context) error {
	var req models.KeepAsMemoryRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if _, err := RequireDatapackAccess(c, h.dpLookup, req.DatapackID); err != nil {
		return err
	}
	result, err := h.svc.KeepAsMemory(c.Request().Context(), req)
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
	if _, err := RequireDatapackAccess(c, h.dpLookup, datapackID); err != nil {
		return err
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
