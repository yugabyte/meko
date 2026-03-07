package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// DatapackHandler holds HTTP handlers for datapack endpoints.
type DatapackHandler struct {
	svc *service.DatapackService
}

// NewDatapackHandler creates a new DatapackHandler.
func NewDatapackHandler(svc *service.DatapackService) *DatapackHandler {
	return &DatapackHandler{svc: svc}
}

// Create handles POST /api/v1/datapacks.
func (h *DatapackHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req service.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
		return
	}

	resp, err := h.svc.Create(req)
	if err != nil {
		if isValidationErr(err) {
			WriteError(w, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}

	WriteJSON(w, http.StatusCreated, resp)
}

// List handles GET /api/v1/datapacks.
func (h *DatapackHandler) List(w http.ResponseWriter, _ *http.Request) {
	datapacks, err := h.svc.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}
	if datapacks == nil {
		datapacks = []models.Datapack{}
	}
	WriteJSON(w, http.StatusOK, datapacks)
}

// Describe handles GET /api/v1/datapacks/{name}.
func (h *DatapackHandler) Describe(
	w http.ResponseWriter, r *http.Request,
) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
		return
	}

	dp, err := h.svc.Describe(name)
	if err != nil {
		if isNotFoundErr(err) {
			WriteError(w, http.StatusNotFound,
				"NOT_FOUND", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, dp)
}

// Update handles PATCH /api/v1/datapacks/{name}.
func (h *DatapackHandler) Update(
	w http.ResponseWriter, r *http.Request,
) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
		return
	}

	var req service.UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
		return
	}

	if err := h.svc.Update(name, req); err != nil {
		if isNotFoundErr(err) {
			WriteError(w, http.StatusNotFound,
				"NOT_FOUND", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, map[string]string{
		"message": "datapack updated",
	})
}

// Delete handles DELETE /api/v1/datapacks/{name}.
func (h *DatapackHandler) Delete(
	w http.ResponseWriter, r *http.Request,
) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
		return
	}

	if err := h.svc.Delete(name); err != nil {
		if isNotFoundErr(err) {
			WriteError(w, http.StatusNotFound,
				"NOT_FOUND", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, map[string]string{
		"message": "datapack deleted",
	})
}

// Status handles GET /api/v1/datapacks/{name}/status.
func (h *DatapackHandler) Status(
	w http.ResponseWriter, r *http.Request,
) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
		return
	}

	st, err := h.svc.Status(name)
	if err != nil {
		if isNotFoundErr(err) {
			WriteError(w, http.StatusNotFound,
				"NOT_FOUND", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
		return
	}

	WriteJSON(w, http.StatusOK, st)
}

func isValidationErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "is required")
}

func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "not found")
}
