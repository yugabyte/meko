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
		"user_id", filter.UserID,
		"agent_id", filter.AgentID,
		"datapack_id", filter.DatapackID,
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
//	@Param		user_id		query	string	false	"User ID (optional)"
//	@Param		agent_id	query	string	false	"Agent ID"
//	@Param		datapack_id	query	string	true	"Datapack ID"
//	@Param		free_user	query	boolean	false	"Skip vector similarity ranking (free tier users)"
//	@Param		page		query	integer	false	"Page number (1-based, default 1)"
//	@Param		page_size	query	integer	false	"Items per page (default 20, max 100)"
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/memories [get]
func (handler *MemoriesHandler) ListGET(c echo.Context) error {
	userID := c.QueryParam("user_id")
	datapackID := c.QueryParam("datapack_id")
	if strings.TrimSpace(datapackID) == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("page_size"))
	filter := models.MemoryListFilter{
		UserID:     userID,
		AgentID:    c.QueryParam("agent_id"),
		DatapackID: datapackID,
		FreeUser:   c.QueryParam("free_user") == "true",
		Page:       page,
		PageSize:   pageSize,
	}
	return handler.list(c, filter)
}
