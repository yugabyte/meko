package registry

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// Register mounts the storage-layer control API on e. The routes sit outside the
// versioned `/api/v1` product API (and its OpenAPI spec), directly on the echo
// instance — the same pattern as the health probes and /metrics. They are the
// surface yb_amp_local and the integration harness drive.
//
// Every route is project-scoped. The storage layer's own namespace is flat, but
// the rows it addresses live in the control plane's resource model, which is not:
// a branch belongs to a project. yb_amp_local provisions one through /api/v1 at
// startup and passes its id here, so a launcher branch is an ordinary branch that
// shows up on the product API rather than a parallel universe of its own.
func Register(e *echo.Echo, store *Store) {
	h := &handlers{store: store}

	e.GET("/ready", h.ready)

	const base = "/control/v1/projects/:project_id"

	e.PUT(base+"/branch/:branch_id/branch_name", h.setBranchName)
	e.GET(base+"/timeline_branch_names", h.listBranchNames)
	e.PUT(base+"/branch/:branch_id/drop", h.branchDrop)

	e.POST(base+"/branch/:branch_id/record", h.recordBranch)
	e.GET(base+"/branches", h.listBranches)

	e.POST(base+"/endpoint", h.endpointCreate)
	e.GET(base+"/endpoint", h.endpointList)
	e.DELETE(base+"/endpoint/:endpoint_id", h.endpointDelete)
}

type handlers struct {
	store *Store
}

// projectID pulls the path's project id. A malformed one is a client error, not a
// lookup that happens to miss: it can never name a project.
func projectID(c echo.Context) (uuid.UUID, error) {
	raw := c.Param("project_id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("malformed project id %q", raw))
	}
	return id, nil
}

func (h *handlers) ready(c echo.Context) error {
	if err := h.store.Ready(c.Request().Context()); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "database unavailable")
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) setBranchName(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	var req SetBranchNameRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.SetBranchName(c.Request().Context(), pid, c.Param("branch_id"), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) listBranchNames(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	names, err := h.store.ListBranchNames(c.Request().Context(), pid)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, names)
}

func (h *handlers) branchDrop(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	if err := h.store.BranchDrop(c.Request().Context(), pid, c.Param("branch_id")); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) recordBranch(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	var req BranchCreateRecord
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.RecordBranch(c.Request().Context(), pid, c.Param("branch_id"), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) listBranches(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	branches, err := h.store.ListBranches(c.Request().Context(), pid)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, branches)
}

func (h *handlers) endpointCreate(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	var req ComputeEndpoint
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.EndpointCreate(c.Request().Context(), pid, req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) endpointList(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	eps, err := h.store.EndpointList(c.Request().Context(), pid)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, eps)
}

func (h *handlers) endpointDelete(c echo.Context) error {
	pid, err := projectID(c)
	if err != nil {
		return err
	}
	if err := h.store.EndpointDelete(c.Request().Context(), pid, c.Param("endpoint_id")); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

// mapErr translates a store sentinel into the matching HTTP status.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalid):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
}
