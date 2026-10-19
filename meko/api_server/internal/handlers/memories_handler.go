package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// DatapackMembershipLookup interface + the access-gate helper now live
// in datapack_access.go and are shared with PromoteMemoriesHandler.

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

func (handler *MemoriesHandler) list(c echo.Context, filter models.MemoryListFilter, grant string) error {
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

	response.LangfuseProjectID = observeProjectID(grant, handler.langfuseSvc.GetProjectID(filter.DatapackID))

	return ok(c, http.StatusOK, response)
}

// ListGET lists memories for a scope using query parameters.
//
//	@Summary		List memories (query)
//	@Description	For a viewer of the datapack, user_id is ignored and who added each memory is left out.
//	@Description	Every row carries is_mine, whether the caller wrote it. It is returned for a viewer too,
//	@Description	whose rows have no attribution to work it out from, because deleting a memory is
//	@Description	author-scoped: a member demoted to viewer may still delete what they wrote before.
//	@Tags		memories
//	@Produce	json
//	@Param		datapack_id	query	string	true	"Datapack ID"
//	@Param		user_id		query	string	false	"User ID (optional; ignored for a viewer)"
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
	accountID, grant, err := RequireDatapackAccessWithGrant(c, handler.dpLookup, datapackID)
	if err != nil {
		return err
	}
	viewer := models.IsViewerGrant(grant)
	userID := strings.TrimSpace(c.QueryParam("user_id"))
	if viewer {
		// Filtering by user would tell a viewer whose memories they are.
		userID = ""
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	pageSize, _ := strconv.Atoi(c.QueryParam("page_size"))
	filter := models.MemoryListFilter{
		DatapackID: datapackID,
		UserID:     userID,
		UserType:   strings.TrimSpace(c.QueryParam("user_type")),
		Page:       page,
		PageSize:   pageSize,
		Q:          strings.TrimSpace(c.QueryParam("q")),
		Scope:      strings.TrimSpace(c.QueryParam("scope")),
		HideOwners: viewer,

		CallerUserID: accountID,
	}
	return handler.list(c, filter, grant)
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
// Viewers may delete their own memories: a member demoted to viewer still owns
// what they wrote before, and nobody else can remove it (the delete is own-rows only).
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
	// Membership gate (shared with PromoteMemoriesHandler): a revoked
	// contributor with a memory they once authored could otherwise still
	// hit DELETE via the SQL `meko_user_id = caller` predicate alone.
	// RequireDatapackAccess validates + parses the UUID, resolves the
	// account, checks owner-then-shared access, and 404s on miss.
	accountID, err := RequireDatapackAccess(c, handler.dpLookup, datapackID)
	if err != nil {
		return err
	}

	slog.Info("deleting memory",
		"layer", "handler",
		"memory_id", memoryID,
		"datapack_id", datapackID,
		"user_id", accountID,
	)

	deleted, err := handler.memoriesSvc.Delete(c.Request().Context(), memoryID, datapackID, accountID)
	if err != nil {
		if errors.Is(err, repository.ErrPreMem0V2Memory) {
			// UI keys on PRE_MEM0_V2_MEMORY to hide the delete affordance.
			return apiError(c, http.StatusBadRequest, "PRE_MEM0_V2_MEMORY",
				"This memory pre-dates the mem0 v2 rollout and cannot be deleted.")
		}
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
	if _, err := RequireDatapackAccess(c, handler.dpLookup, datapackID); err != nil {
		return err
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
