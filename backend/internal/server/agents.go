package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/agents"
	"github.com/yugabyte/meko/backend/internal/envelope"
)

// The agents' routes (internal/agents, docs/api.md#agents), on both listeners: GET /api/v1/agents
// lists the agents on this computer and whether each reaches Meko's target, for any credential;
// POST /api/v1/agents/:name/connect and /disconnect change one, for the owner (the web listener's
// browsers, with local or remote access) or the admin token. A client token mayn't: while Meko Cloud is linked,
// connect writes its account-wide key into the agent's config. No reply holds a token or key.
func (s *Server) registerAgents(v1 *echo.Group) {
	if s.opts.Agents == nil {
		return
	}
	v1.GET("/agents", s.agentList)
	v1.POST("/agents/:name/connect", s.agentChange)
	v1.POST("/agents/:name/disconnect", s.agentChange)
}

func (s *Server) agentList(c echo.Context) error {
	l, err := s.opts.Agents.List()
	if err != nil {
		return err
	}
	return envelope.OK(c, l)
}

func (s *Server) agentChange(c echo.Context) error {
	switch Credential(c) {
	case CredentialAdmin, CredentialRemote, CredentialLocal:
	default:
		return envelope.Fail(c, http.StatusForbidden, "OWNER_ONLY",
			"Connecting and disconnecting agents takes Meko's owner: use meko agent, or Meko's web UI.")
	}
	ctx, name := c.Request().Context(), c.Param("name")
	var r agents.Result
	var err error
	if c.Path() == "/api/v1/agents/:name/connect" {
		r, err = s.opts.Agents.Connect(ctx, name)
	} else {
		r, err = s.opts.Agents.Disconnect(ctx, name)
	}
	var ae *agents.Error
	if errors.As(err, &ae) {
		status := map[string]int{agents.CodeUnknown: http.StatusNotFound, agents.CodeNotInstalled: http.StatusConflict,
			agents.CodeFailed: http.StatusBadGateway}[ae.Code]
		return envelope.Fail(c, status, ae.Code, ae.Error())
	}
	if err != nil {
		return err
	}
	if r.Changed {
		s.opts.Logger.Info("agent changed", "agent", name, "route", c.Path(), "state", r.Agent.State)
	}
	return envelope.OK(c, r)
}
