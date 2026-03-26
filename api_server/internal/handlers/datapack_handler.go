package handlers

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// DatapackHandler holds Echo handlers for datapack endpoints.
type DatapackHandler struct {
	svc *service.DatapackService
}

// NewDatapackHandler creates a new DatapackHandler.
func NewDatapackHandler(svc *service.DatapackService) *DatapackHandler {
	return &DatapackHandler{svc: svc}
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
//	@Router		/datapacks [post]
func (h *DatapackHandler) Create(c echo.Context) error {
	var req service.CreateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if account, err := auth.GetAccount(c); err == nil {
		req.AccountID = account.ID
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
//
//	@Summary	Describe a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name} [get]
func (h *DatapackHandler) Describe(c echo.Context) error {
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

	dp, err := h.svc.Describe(account.ID, name)
	if err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
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
//	@Failure	404		{object}	APIResponse
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
//	@Failure	404		{object}	APIResponse
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
//	@Failure	404		{object}	APIResponse
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

func isValidationErr(err error) bool {
	return strings.Contains(err.Error(), "is required")
}

func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

func isConflictErr(err error) bool {
	return strings.Contains(err.Error(), "already exists")
}
