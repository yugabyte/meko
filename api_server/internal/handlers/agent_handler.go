package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// AgentHandler holds Echo handlers for agent endpoints.
type AgentHandler struct {
	svc *service.AgentService
}

// NewAgentHandler creates a new AgentHandler.
func NewAgentHandler(svc *service.AgentService) *AgentHandler {
	return &AgentHandler{svc: svc}
}

// Create handles POST /api/v1/datapacks/:name/agents.
//
//	@Summary	Create an agent
//	@Tags		agents
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string						true	"Datapack name"
//	@Param		body	body		service.CreateAgentRequest	true	"Agent request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/agents [post]
func (h *AgentHandler) Create(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.CreateAgentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	agentID, err := h.svc.Create(dpName, req)
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
		"agent_id":      agentID,
		"agent_name":    req.AgentName,
		"datapack_name": dpName,
	})
}

// List handles GET /api/v1/datapacks/:name/agents.
//
//	@Summary	List agents in a datapack
//	@Tags		agents
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Router		/datapacks/{name}/agents [get]
func (h *AgentHandler) List(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	agents, err := h.svc.List(dpName)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if agents == nil {
		agents = []models.Agent{}
	}
	return ok(c, http.StatusOK, agents)
}

// Delete handles DELETE /api/v1/datapacks/:name/agents/:agent.
//
//	@Summary	Delete an agent
//	@Tags		agents
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Param		agent	path		string	true	"Agent name"
//	@Success	200		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/agents/{agent} [delete]
func (h *AgentHandler) Delete(c echo.Context) error {
	dpName := c.Param("name")
	agentName := c.Param("agent")
	if dpName == "" || agentName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			"datapack name and agent name are required")
	}

	if err := h.svc.Delete(dpName, agentName); err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "agent deleted",
	})
}
