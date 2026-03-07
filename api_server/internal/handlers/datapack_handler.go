package handlers

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

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
func (h *DatapackHandler) Create(c echo.Context) error {
	var req service.CreateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	resp, err := h.svc.Create(req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusCreated, resp)
}

// List handles GET /api/v1/datapacks.
func (h *DatapackHandler) List(c echo.Context) error {
	datapacks, err := h.svc.List()
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
func (h *DatapackHandler) Describe(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	dp, err := h.svc.Describe(name)
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
func (h *DatapackHandler) Update(c echo.Context) error {
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

	if err := h.svc.Update(name, req); err != nil {
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
func (h *DatapackHandler) Delete(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	if err := h.svc.Delete(name); err != nil {
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
func (h *DatapackHandler) Status(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	st, err := h.svc.Status(name)
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
