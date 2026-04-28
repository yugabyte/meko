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

type MemoriesHandler struct {
	memoriesSvc *service.MemoriesService
}

func NewMemoriesHandler(memoriesSvc *service.MemoriesService) *MemoriesHandler {
	return &MemoriesHandler{memoriesSvc: memoriesSvc}
}

func (handler *MemoriesHandler) list(c echo.Context, filter models.MemoryListFilter) error {
	slog.Info("listing memories",
		"layer", "handler",
		"datapack_id", filter.DatapackID,
		"user_id", filter.UserID,
		"user_type", filter.UserType,
	)

	response, err := handler.memoriesSvc.List(c.Request().Context(), filter)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, response)
}

// ListGET lists memories for a scope using query parameters.
//
//	@Summary	List memories (query)
//	@Tags		memories
//	@Produce	json
//	@Param		datapack_id	query	string	true	"Datapack ID"
//	@Param		user_id		query	string	false	"User ID (optional)"
//	@Param		user_type	query	string	false	"Account type (free|pro|enterprise). pro/enterprise enables vector ranking."
//	@Param		page		query	integer	false	"Page number (1-based, default 1)"
//	@Param		page_size	query	integer	false	"Items per page (default 20, max 100)"
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/memories [get]
func (handler *MemoriesHandler) ListGET(c echo.Context) error {
	datapackID := c.QueryParam("datapack_id")
	if strings.TrimSpace(datapackID) == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("page_size"))
	filter := models.MemoryListFilter{
		DatapackID: datapackID,
		UserID:     c.QueryParam("user_id"),
		UserType:   c.QueryParam("user_type"),
		Page:       page,
		PageSize:   pageSize,
	}
	return handler.list(c, filter)
}

// CountGET returns the total number of mem0 memories for a datapack.
//
//	@Summary	Count memories for a datapack
//	@Tags		memories
//	@Produce	json
//	@Param		datapack_id	query		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Router		/memories/count [get]
func (handler *MemoriesHandler) CountGET(c echo.Context) error {
	datapackID := c.QueryParam("datapack_id")
	if strings.TrimSpace(datapackID) == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}

	slog.Info("counting memories",
		"layer", "handler",
		"datapack_id", datapackID,
	)

	response, err := handler.memoriesSvc.Count(c.Request().Context(), datapackID)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, response)
}
