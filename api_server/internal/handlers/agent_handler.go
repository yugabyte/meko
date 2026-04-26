package handlers

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// AgentHandler holds Echo handlers for agent endpoints.
//
// The handler exposes two surfaces:
//
//   - User-scoped CRUD on /api/v1/agents (CreateUser, ListUser,
//     DeleteUser) backed by meko_system.agents.
//   - Link-only operations on /api/v1/datapacks/:name/agents (Link,
//     ListForDatapack, Unlink) backed by meko_system.agent_datapacks.
type AgentHandler struct {
	svc *service.AgentService
}

// NewAgentHandler creates a new AgentHandler.
func NewAgentHandler(svc *service.AgentService) *AgentHandler {
	return &AgentHandler{svc: svc}
}

// CreateUser handles POST /api/v1/agents and creates a user-scoped agent.
// Strictly create-only: returns 409 Conflict when (user_id, agent_id)
// already exists.
//
//	@Summary	Create a user-scoped agent
//	@Tags		agents
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.CreateUserAgentRequest	true	"Agent request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/agents [post]
func (h *AgentHandler) CreateUser(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	var req service.CreateUserAgentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	agent, err := h.svc.CreateUserAgent(account.ID, req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, repository.ErrAgentExists) {
			return apiError(c, http.StatusConflict,
				"CONFLICT", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusCreated, agent)
}

// ListUser handles GET /api/v1/agents and returns all agents for the
// authenticated user.
//
//	@Summary	List agents for the authenticated user
//	@Tags		agents
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/agents [get]
func (h *AgentHandler) ListUser(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	agents, err := h.svc.ListUserAgents(account.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if agents == nil {
		agents = []models.Agent{}
	}
	return ok(c, http.StatusOK, agents)
}

// DeleteUser handles DELETE /api/v1/agents/:agent_id and removes a
// user-scoped agent (and any datapack links).
//
//	@Summary	Delete a user-scoped agent
//	@Tags		agents
//	@Produce	json
//	@Param		agent_id	path		string	true	"Agent ID"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/agents/{agent_id} [delete]
func (h *AgentHandler) DeleteUser(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	agentID := c.Param("agent_id")
	if agentID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "agent_id is required")
	}

	if err := h.svc.DeleteUserAgent(account.ID, agentID); err != nil {
		if errors.Is(err, repository.ErrAgentNotFoundForDelete) ||
			isNotFoundErr(err) {
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

// Link handles POST /api/v1/datapacks/:name/agents and links an
// existing user-scoped agent to the datapack identified by :name.
//
//	@Summary	Link an agent to a datapack
//	@Tags		agents
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string						true	"Datapack name"
//	@Param		body	body		service.LinkAgentRequest	true	"Link request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/agents [post]
func (h *AgentHandler) Link(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "missing datapack context")
	}

	var req service.LinkAgentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	linked, err := h.svc.LinkAgentToDatapack(account.ID, req.AgentID, dp.ID)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if !linked {
		return apiError(c, http.StatusNotFound,
			"NOT_FOUND", "agent not found for user; create it via POST /agents first")
	}

	return ok(c, http.StatusCreated, map[string]string{
		"agent_id":      req.AgentID,
		"datapack_id":   dp.ID,
		"datapack_name": dp.Name,
	})
}

// ListForDatapack handles GET /api/v1/datapacks/:name/agents and
// returns the agents currently linked to the datapack for the
// authenticated user.
//
//	@Summary	List agents linked to a datapack
//	@Tags		agents
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/agents [get]
func (h *AgentHandler) ListForDatapack(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "missing datapack context")
	}

	agents, err := h.svc.ListAgentsForDatapack(account.ID, dp.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if agents == nil {
		agents = []models.Agent{}
	}
	return ok(c, http.StatusOK, agents)
}

// Unlink handles DELETE /api/v1/datapacks/:name/agents/:agent_id and
// removes the link between a user-scoped agent and a datapack. The
// underlying agent row is left intact.
//
//	@Summary	Unlink an agent from a datapack
//	@Tags		agents
//	@Produce	json
//	@Param		name		path		string	true	"Datapack name"
//	@Param		agent_id	path		string	true	"Agent ID"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/agents/{agent_id} [delete]
func (h *AgentHandler) Unlink(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "missing datapack context")
	}

	agentID := c.Param("agent_id")
	if agentID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "agent_id is required")
	}

	if err := h.svc.UnlinkAgentFromDatapack(account.ID, agentID, dp.ID); err != nil {
		if errors.Is(err, repository.ErrAgentLinkNotFound) ||
			isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "agent unlinked from datapack",
	})
}
