package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/yb-light/amp/internal/db"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

type handlers struct {
	pool  *db.Pool
	store task.Store
}

func registerRoutes(e *echo.Echo, h *handlers) {
	e.GET("/healthz", h.healthz)
	e.GET("/readyz", h.readyz)

	// REST API, versioned in the path. Mutation handlers are stubs until the
	// task framework's step catalog lands; each core workflow (§6) has a route
	// so the surface is visible.
	v1 := e.Group("/api/v1")
	v1.POST("/storage-pools", h.createStoragePool)
	v1.GET("/storage-pools", h.listStoragePools)
	v1.POST("/databases", h.createDatabase)
	v1.POST("/databases/:tenant/branches", h.branchDatabase)
	v1.POST("/timelines/:tenant/:timeline/endpoints", h.createEndpoint)
	v1.DELETE("/endpoints/:id", h.deleteEndpoint)
	v1.GET("/tasks/:id", h.getTask)
}

func (h *handlers) healthz(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handlers) readyz(c echo.Context) error {
	if err := h.pool.Healthy(c.Request().Context()); err != nil {
		return writeError(c, http.StatusServiceUnavailable, "not_ready", "database unavailable")
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}

// Stub mutation handlers — each maps to a core workflow (§6) and will dispatch
// a task once the framework's step catalog exists.

func (h *handlers) createStoragePool(c echo.Context) error {
	return notImplemented(c, "create storage pool")
}
func (h *handlers) listStoragePools(c echo.Context) error {
	return notImplemented(c, "list storage pools")
}
func (h *handlers) createDatabase(c echo.Context) error { return notImplemented(c, "create database") }
func (h *handlers) branchDatabase(c echo.Context) error { return notImplemented(c, "branch database") }
func (h *handlers) createEndpoint(c echo.Context) error { return notImplemented(c, "create endpoint") }
func (h *handlers) deleteEndpoint(c echo.Context) error { return notImplemented(c, "delete endpoint") }

// taskView is the API representation of a task's status.
type taskView struct {
	ID          uuid.UUID       `json:"id"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	TenantID    *uuid.UUID      `json:"tenant_id,omitempty"`
	TargetRef   *string         `json:"target_ref,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Error       *string         `json:"error,omitempty"`
	LeaseExpiry *time.Time      `json:"lease_expiry,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// getTask exposes task status so a caller can poll a long-running operation.
// This is a real read path against the store, not a stub.
func (h *handlers) getTask(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return writeError(c, http.StatusBadRequest, "invalid_id", "task id must be a uuid")
	}
	t, err := h.store.Get(c.Request().Context(), id)
	if err != nil {
		return writeError(c, http.StatusInternalServerError, "internal", "failed to load task")
	}
	if t == nil {
		return writeError(c, http.StatusNotFound, "not_found", "task not found")
	}
	return c.JSON(http.StatusOK, taskView{
		ID:          t.ID,
		Kind:        t.Kind,
		Status:      string(t.Status),
		TenantID:    t.TenantID,
		TargetRef:   t.TargetRef,
		Params:      t.Params,
		Error:       t.Error,
		LeaseExpiry: t.LeaseExpiry,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	})
}
