package handlers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// DatapackHandler holds Echo handlers for datapack endpoints.
type DatapackHandler struct {
	svc         *service.DatapackService
	langfuse    *service.LangfuseService
	memoriesSvc *service.MemoriesService
	kbSvc       *service.KnowledgeBaseService
}

// NewDatapackHandler creates a new DatapackHandler.
func NewDatapackHandler(
	svc *service.DatapackService,
	langfuse *service.LangfuseService,
	memoriesSvc *service.MemoriesService,
	kbSvc *service.KnowledgeBaseService,
) *DatapackHandler {
	return &DatapackHandler{
		svc:         svc,
		langfuse:    langfuse,
		memoriesSvc: memoriesSvc,
		kbSvc:       kbSvc,
	}
}

// Create handles POST /api/v1/datapacks.
//
//	@Summary	Create a datapack
//	@Tags		datapacks
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.CreateRequest	true	"Create request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks [post]
func (h *DatapackHandler) Create(c echo.Context) error {
	var req service.CreateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if account, err := auth.GetAccount(c); err == nil {
		req.UserID = account.ID
	}

	resp, err := h.svc.Create(req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isConflictErr(err) {
			return apiError(c, http.StatusConflict,
				"CONFLICT", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusCreated, resp)
}

// List handles GET /api/v1/datapacks.
//
//	@Summary	List all datapacks
//	@Tags		datapacks
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks [get]
func (h *DatapackHandler) List(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	datapacks, err := h.svc.List(account.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if datapacks == nil {
		datapacks = []models.Datapack{}
	}
	return ok(c, http.StatusOK, datapacks)
}

// Describe handles GET /api/v1/datapacks/:name.
// The datapack is resolved by RequireDatapackAccess middleware which
// supports both owners and shared users. The Grant field is already
// populated by the repository layer.
//
//	@Summary	Describe a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name} [get]
func (h *DatapackHandler) Describe(c echo.Context) error {
	dp, found := c.Get(auth.ContextKeyDatapack).(*models.Datapack)
	if !found || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	if h.memoriesSvc != nil {
		if resp, err := h.memoriesSvc.Count(c.Request().Context(), dp.ID); err != nil {
			slog.Warn("failed to count memories", "datapack_id", dp.ID, "error", err)
		} else {
			dp.MemoryCount = resp.Count
		}
	}

	if h.kbSvc != nil {
		if kbCounts, err := h.kbSvc.Counts(dp); err != nil {
			slog.Warn("failed to count knowledge base items", "datapack_id", dp.ID, "error", err)
		} else {
			dp.KnowledgeCount = int(kbCounts.TotalChunks)
			dp.LearningsCount = int(kbCounts.PromotedMemories)
		}
	}

	return ok(c, http.StatusOK, dp)
}

// Update handles PATCH /api/v1/datapacks/:name.
//
//	@Summary	Update a datapack
//	@Tags		datapacks
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string					true	"Datapack name"
//	@Param		body	body		service.UpdateRequest	true	"Update fields"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name} [patch]
func (h *DatapackHandler) Update(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.UpdateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.Update(account.ID, name, req); err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "datapack updated",
	})
}

// Delete handles DELETE /api/v1/datapacks/:name.
//
//	@Summary	Delete a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name} [delete]
func (h *DatapackHandler) Delete(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	if err := h.svc.Delete(account.ID, name); err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "datapack deleted",
	})
}

// Status handles GET /api/v1/datapacks/:name/status.
//
//	@Summary	Get datapack status
//	@Tags		datapacks
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/status [get]
func (h *DatapackHandler) Status(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	st, err := h.svc.Status(account.ID, name)
	if err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, st)
}

// GenerateLangfuseProjectKeys handles
// POST /api/v1/datapacks/:name/langfuse/project-keys.
//
//	@Summary	Generate Langfuse project keys for a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/langfuse/project-keys [post]
func (h *DatapackHandler) GenerateLangfuseProjectKeys(c echo.Context) error {
	if h.langfuse == nil || !h.langfuse.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"SERVICE_UNAVAILABLE",
			"langfuse is not configured")
	}

	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := h.svc.Describe(account.ID, name)
	if err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	keys, err := h.langfuse.GenerateProjectKeysForDatapack(
		account.ID,
		account.Email,
		account.FirstName,
		account.LastName,
		dp.ID,
		dp.Name,
	)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, keys)
}

func isValidationErr(err error) bool {
	return strings.Contains(err.Error(), "is required")
}

func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

func isConflictErr(err error) bool {
	return strings.Contains(err.Error(), "already exists")
}
