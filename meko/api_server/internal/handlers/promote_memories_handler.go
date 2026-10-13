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

// firstInvalidUUID returns the first id that is not a canonical 36-char UUID. The
// mem0 fetch casts ids to uuid, so a bad id would surface the Postgres cast error
// as a 500 with raw SQL text (MEKO-834); callers refuse it with 400 first.
// uuid.Parse alone is too lenient — it also accepts the urn:uuid:, {braces} and
// hyphenless-32 forms (its own doc says not to use it to validate), and the uuid
// cast rejects urn:uuid:, so those would still reach SQL. The length guard pins
// every accepted id to the one form the cast takes.
func firstInvalidUUID(ids []string) (string, bool) {
	for _, id := range ids {
		if len(id) != 36 {
			return id, true
		}
		if _, err := uuid.Parse(id); err != nil {
			return id, true
		}
	}
	return "", false
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
	// Promotion writes to the shared tenant KB, so it's owner/maintainer-only —
	// viewers and contributors get 403.
	if _, err := RequireDatapackOwnerOrMaintainer(c, h.dpLookup, req.DatapackID); err != nil {
		return err
	}
	if bad, found := firstInvalidUUID(req.MemoryIDs); found {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"memory id "+strconv.Quote(bad)+" is not a valid UUID")
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
	if bad, found := firstInvalidUUID(req.MemoryIDs); found {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"memory id "+strconv.Quote(bad)+" is not a valid UUID")
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

// DeleteByID handles DELETE /api/v1/promoted-memories/:memory_id.
//
// Owner/maintainer-only: unlike private-delete (which lets any member
// remove their own row), removing a promoted memory affects everyone
// with access to the datapack — the row lives in the shared KB —
// so viewers and contributors are refused with 403 in the handler.
// The gate runs in-handler rather than via auth.RequireDatapackAccess
// middleware because the datapack id arrives as a query param, not a
// :datapack_id path segment (same reason as MemoriesHandler.DeleteByID).
//
//	@Summary	Delete a promoted memory (owner/maintainer only)
//	@Tags		memories
//	@Produce	json
//	@Param		memory_id	path		string	true	"Promoted memory ID (UUID)"
//	@Param		datapack_id	query		string	true	"Datapack ID owning the promoted memory"
//	@Success	200			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	403			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/promoted-memories/{memory_id} [delete]
func (h *PromoteMemoriesHandler) DeleteByID(c echo.Context) error {
	memoryID := strings.TrimSpace(c.Param("memory_id"))
	if memoryID == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "memory_id is required")
	}
	if _, err := uuid.Parse(memoryID); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "memory_id must be a valid UUID")
	}
	datapackID := strings.TrimSpace(c.QueryParam("datapack_id"))
	// Owner/maintainer gate in-handler (datapack_id in query, not path).
	if _, err := RequireDatapackOwnerOrMaintainer(c, h.dpLookup, datapackID); err != nil {
		return err
	}

	slog.Info("deleting promoted memory",
		"layer", "handler",
		"memory_id", memoryID,
		"datapack_id", datapackID,
	)

	deleted, err := h.svc.DeleteByIDForDatapack(c.Request().Context(), memoryID, datapackID)
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
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "promoted memory not found")
	}

	return ok(c, http.StatusOK, map[string]bool{"deleted": true})
}
