package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// KnowledgeBaseHandler holds Echo handlers for knowledge-base endpoints.
type KnowledgeBaseHandler struct {
	svc *service.KnowledgeBaseService
}

// NewKnowledgeBaseHandler creates a new KnowledgeBaseHandler.
func NewKnowledgeBaseHandler(
	svc *service.KnowledgeBaseService,
) *KnowledgeBaseHandler {
	return &KnowledgeBaseHandler{svc: svc}
}

// Add handles POST /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	Add a knowledge base
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string				true	"Datapack name"
//	@Param		body	body		service.AddRequest	true	"KB request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [post]
func (h *KnowledgeBaseHandler) Add(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.AddRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	kbID, err := h.svc.Add(dpName, req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusCreated, map[string]string{
		"knowledge_base_id": kbID,
		"url":               req.URL,
		"datapack_name":     dpName,
	})
}

// List handles GET /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	List knowledge bases
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [get]
func (h *KnowledgeBaseHandler) List(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	kbs, err := h.svc.List(dpName)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if kbs == nil {
		kbs = []models.KnowledgeBase{}
	}
	return ok(c, http.StatusOK, kbs)
}

// Delete handles DELETE /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	Delete a knowledge base
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string					true	"Datapack name"
//	@Param		body	body		service.DeleteRequest	true	"KB to delete"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [delete]
func (h *KnowledgeBaseHandler) Delete(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.DeleteRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.Delete(dpName, req); err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "knowledge base deleted",
	})
}
