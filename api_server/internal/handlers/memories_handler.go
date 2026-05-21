package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// DatapackMembershipLookup verifies that a caller is currently a member
// of a given datapack (owner OR active accepted share). DELETE /memories
// uses it as a defense-in-depth gate so a revoked member can't keep
// modifying rows they authored before being removed. Implemented by
// repository.DatapackRepo; declared here so the handler depends on the
// narrow interface, not the concrete type.
type DatapackMembershipLookup interface {
	GetByID(userID, datapackID string) (*models.Datapack, error)
	GetByIDForSharedAccess(userID, datapackID string) (*models.Datapack, error)
}

type MemoriesHandler struct {
	memoriesSvc *service.MemoriesService
	langfuseSvc *service.LangfuseService
	dpLookup    DatapackMembershipLookup
}

func NewMemoriesHandler(
	memoriesSvc *service.MemoriesService,
	langfuseSvc *service.LangfuseService,
	dpLookup DatapackMembershipLookup,
) *MemoriesHandler {
	return &MemoriesHandler{
		memoriesSvc: memoriesSvc,
		langfuseSvc: langfuseSvc,
		dpLookup:    dpLookup,
	}
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

	response.LangfuseProjectID = handler.langfuseSvc.GetProjectID(filter.DatapackID)

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
//	@Failure	401	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/memories [get]
func (handler *MemoriesHandler) ListGET(c echo.Context) error {
	datapackID := strings.TrimSpace(c.QueryParam("datapack_id"))
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("page_size"))
	filter := models.MemoryListFilter{
		DatapackID: datapackID,
		UserID:     strings.TrimSpace(c.QueryParam("user_id")),
		UserType:   strings.TrimSpace(c.QueryParam("user_type")),
		Page:       page,
		PageSize:   pageSize,
	}
	return handler.list(c, filter)
}

// DeleteByID handles DELETE /api/v1/memories/:memory_id.
//
// Scoped to the authenticated user: a memory is only deleted when it
// belongs to the caller (meko_user_id = current account). In a shared
// datapack other members can see your memories in the list response,
// but they cannot delete them — and you cannot delete theirs. Memories
// that don't match (wrong datapack, wrong user, or simply not present)
// collapse to a single 404 so we don't reveal which case applied.
//
//	@Summary	Delete one of your own memories
//	@Tags		memories
//	@Produce	json
//	@Param		memory_id	path		string	true	"Memory ID (UUID)"
//	@Param		datapack_id	query		string	true	"Datapack ID owning the memory"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/memories/{memory_id} [delete]
func (handler *MemoriesHandler) DeleteByID(c echo.Context) error {
	memoryID := strings.TrimSpace(c.Param("memory_id"))
	if memoryID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "memory_id is required")
	}
	// Validate UUID format up front so a malformed id surfaces as 400, not
	// a 500 from Postgres' ::uuid cast failing on garbage input.
	if _, err := uuid.Parse(memoryID); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "memory_id must be a valid UUID")
	}
	datapackID := strings.TrimSpace(c.QueryParam("datapack_id"))
	if datapackID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	if _, err := uuid.Parse(datapackID); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id must be a valid UUID")
	}

	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	// Membership gate: confirm the caller is currently a member of the
	// datapack (owner OR accepted share) before letting them delete any
	// row. The SQL meko_user_id = caller predicate alone would still let
	// a revoked contributor delete memories they authored before being
	// removed; this check shuts that path. We collapse "not a member"
	// and "datapack doesn't exist" into one 404 — same convention as
	// auth.RequireDatapackAccess.
	dp, err := handler.dpLookup.GetByID(account.ID, datapackID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if dp == nil {
		dp, err = handler.dpLookup.GetByIDForSharedAccess(account.ID, datapackID)
		if err != nil {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
	}
	if dp == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "datapack not found")
	}

	slog.Info("deleting memory",
		"layer", "handler",
		"memory_id", memoryID,
		"datapack_id", datapackID,
		"user_id", account.ID,
	)

	deleted, err := handler.memoriesSvc.Delete(c.Request().Context(), memoryID, datapackID, account.ID)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !deleted {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "memory not found")
	}

	return ok(c, http.StatusOK, models.Memory{ID: memoryID})
}

// CountGET returns the total number of mem0 memories for a datapack.
//
//	@Summary	Count memories for a datapack
//	@Tags		memories
//	@Produce	json
//	@Param		datapack_id	query		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/memories/count [get]
func (handler *MemoriesHandler) CountGET(c echo.Context) error {
	datapackID := strings.TrimSpace(c.QueryParam("datapack_id"))
	if datapackID == "" {
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
