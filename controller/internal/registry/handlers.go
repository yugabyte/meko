package registry

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Register mounts the storage-layer control API on e. The routes sit outside the
// versioned `/api/v1` product API (and its OpenAPI spec), directly on the echo
// instance — the same pattern as the health probes and /metrics. They are the
// surface yb_light_local and the integration harness drive.
func Register(e *echo.Echo, store *Store) {
	h := &handlers{store: store}

	e.GET("/ready", h.ready)

	e.POST("/control/v1/node", h.nodeRegister)
	e.GET("/control/v1/node", h.nodeList)
	e.PUT("/control/v1/node/:node_id/config", h.nodeConfigure)

	e.POST("/debug/v1/attach-hook", h.attachHook)
	e.POST("/debug/v1/inspect", h.inspect)
	e.POST("/debug/v1/branch/:branch_id/import", h.branchImport)

	e.POST("/control/v1/branch/:branch_id/register", h.branchRegister)
	e.PUT("/control/v1/branch/:branch_id/branch_name", h.setBranchName)
	e.GET("/control/v1/timeline_branch_names", h.listBranchNames)
	e.PUT("/control/v1/branch/:branch_id/drop", h.branchDrop)

	e.POST("/control/v1/branch/:branch_id/pending_init", h.recordPendingInit)
	e.DELETE("/control/v1/branch/:branch_id/pending_init", h.deletePendingInit)
	e.GET("/control/v1/pending_timeline_inits", h.listPendingInits)

	e.POST("/control/v1/endpoint", h.endpointCreate)
	e.GET("/control/v1/endpoint", h.endpointList)
	e.DELETE("/control/v1/endpoint/:endpoint_id", h.endpointDelete)
}

type handlers struct {
	store *Store
}

func (h *handlers) ready(c echo.Context) error {
	if err := h.store.Ready(c.Request().Context()); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "database unavailable")
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) attachHook(c echo.Context) error {
	var req AttachHookRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	gen, err := h.store.AttachHook(c.Request().Context(), req)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, AttachHookResponse{Generation: gen})
}

func (h *handlers) inspect(c echo.Context) error {
	var req InspectRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	att, err := h.store.Inspect(c.Request().Context(), req.BranchID)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, InspectResponse{Attachment: att})
}

func (h *handlers) branchImport(c echo.Context) error {
	resp, err := h.store.BranchImport(c.Request().Context(), c.Param("branch_id"))
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *handlers) nodeRegister(c echo.Context) error {
	var req NodeRegisterRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.NodeRegister(c.Request().Context(), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) nodeList(c echo.Context) error {
	nodes, err := h.store.NodeList(c.Request().Context())
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nodes)
}

func (h *handlers) nodeConfigure(c echo.Context) error {
	var req NodeConfigureRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.NodeConfigure(c.Request().Context(), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) branchRegister(c echo.Context) error {
	var req BranchRegisterRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.BranchRegister(c.Request().Context(), c.Param("branch_id"), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) setBranchName(c echo.Context) error {
	var req SetBranchNameRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.SetBranchName(c.Request().Context(), c.Param("branch_id"), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) listBranchNames(c echo.Context) error {
	names, err := h.store.ListBranchNames(c.Request().Context())
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, names)
}

func (h *handlers) branchDrop(c echo.Context) error {
	if err := h.store.BranchDrop(c.Request().Context(), c.Param("branch_id")); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) recordPendingInit(c echo.Context) error {
	var req PendingBranchInitRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.RecordPendingInit(c.Request().Context(), c.Param("branch_id"), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) deletePendingInit(c echo.Context) error {
	if err := h.store.DeletePendingInit(c.Request().Context(), c.Param("branch_id")); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) listPendingInits(c echo.Context) error {
	inits, err := h.store.ListPendingInits(c.Request().Context())
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, inits)
}

func (h *handlers) endpointCreate(c echo.Context) error {
	var req ComputeEndpoint
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.EndpointCreate(c.Request().Context(), req); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (h *handlers) endpointList(c echo.Context) error {
	eps, err := h.store.EndpointList(c.Request().Context())
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, eps)
}

func (h *handlers) endpointDelete(c echo.Context) error {
	if err := h.store.EndpointDelete(c.Request().Context(), c.Param("endpoint_id")); err != nil {
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
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
}
