package api

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/amp/controller/internal/ycp"
)

// ycpBindingStore is the write-once binding persistence the bind endpoint needs.
// *ycp.BindingStore satisfies it; the interface keeps this handler testable
// without a database.
type ycpBindingStore interface {
	Load(ctx context.Context) (*ycp.Binding, error)
	Bind(ctx context.Context, ycpURL, envID string) (*ycp.Binding, bool, error)
}

// bindRequest is the reconcile-agent's claim that this controller belongs to a
// given YCP environment.
type bindRequest struct {
	YCPURL        string `json:"ycp_url"`
	EnvironmentID string `json:"environment_id"`
}

type bindResponse struct {
	YCPURL        string `json:"ycp_url"`
	EnvironmentID string `json:"environment_id"`
}

// RegisterYCPBinding mounts the write-once binding routes. They sit outside the
// OpenAPI spec and the versioned resource API — like /healthz and /metrics —
// because the binding is operational state the agent pins, not a user-addressable
// resource. No-op when store is nil (the controller runs bindingless in local
// dev / tests).
func (s *Server) RegisterYCPBinding(store ycpBindingStore) {
	if store == nil {
		return
	}
	s.echo.POST(apiBaseURL+"/ycp/binding", func(c echo.Context) error {
		var req bindRequest
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
		}
		if req.YCPURL == "" || req.EnvironmentID == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "ycp_url and environment_id are required")
		}
		cur, created, err := store.Bind(c.Request().Context(), req.YCPURL, req.EnvironmentID)
		if err != nil {
			return err
		}
		// A conflict is an incumbent binding that differs from the claim: the
		// controller is already owned by a different YCP environment. 409 so the
		// agent refuses to drive it rather than fighting another agent.
		if !created && (cur.YCPURL != req.YCPURL || cur.EnvironmentID != req.EnvironmentID) {
			return c.JSON(http.StatusConflict, bindResponse{YCPURL: cur.YCPURL, EnvironmentID: cur.EnvironmentID})
		}
		return c.JSON(http.StatusOK, bindResponse{YCPURL: cur.YCPURL, EnvironmentID: cur.EnvironmentID})
	})
	s.echo.GET(apiBaseURL+"/ycp/binding", func(c echo.Context) error {
		cur, err := store.Load(c.Request().Context())
		if err != nil {
			return err
		}
		if cur == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not bound")
		}
		return c.JSON(http.StatusOK, bindResponse{YCPURL: cur.YCPURL, EnvironmentID: cur.EnvironmentID})
	})
}
