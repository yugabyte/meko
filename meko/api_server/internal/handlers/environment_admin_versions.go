package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/service"
)

// The admin tier's routes for the two admin-only desired-state kinds, which no public
// path can name: the version an amp environment's controller release runs, and the
// version its endpoints are rolled onto. A PUT answers 202 with the stored row, since
// the agent applies it on its next tick; a GET answers the declaration beside the
// agent's last report of it.
//
// accountScope is optional on these routes: the admin group never requires an
// account, and one is sent only to resolve an id two accounts share.

// adminActorHeader names the operator an admin write is made for. Optional here,
// unlike on the tier change: it is logged with the write, not audited.
const adminActorHeader = "X-Meko-Admin-Actor-Email"

// AdminPutController declares the controller's version.
func (h *EnvironmentHandler) AdminPutController(c echo.Context) error {
	var v service.ControllerVersion
	if !bindJSONStrict(c, &v) {
		return nil
	}
	d, found, err := h.svc.SetControllerVersion(c.Param("id"), accountScope(c), adminActor(c), v)
	return adminVersionAnswer(c, http.StatusAccepted, found, err, d)
}

// AdminPutFleetVersion declares the fleet's version. 409 CONTROLLER_NOT_DECLARED
// until a controller is declared.
func (h *EnvironmentHandler) AdminPutFleetVersion(c echo.Context) error {
	var v service.FleetVersion
	if !bindJSONStrict(c, &v) {
		return nil
	}
	d, found, err := h.svc.SetFleetVersion(c.Param("id"), accountScope(c), adminActor(c), v)
	return adminVersionAnswer(c, http.StatusAccepted, found, err, d)
}

// AdminGetController reports the controller's declaration and observed state.
func (h *EnvironmentHandler) AdminGetController(c echo.Context) error {
	v, found, err := h.svc.ControllerVersionStatus(c.Param("id"), accountScope(c))
	return adminVersionAnswer(c, http.StatusOK, found, err, v)
}

// AdminGetFleetVersion reports the fleet-version's declaration and observed state.
func (h *EnvironmentHandler) AdminGetFleetVersion(c echo.Context) error {
	v, found, err := h.svc.FleetVersionStatus(c.Param("id"), accountScope(c))
	return adminVersionAnswer(c, http.StatusOK, found, err, v)
}

func adminActor(c echo.Context) string {
	return strings.TrimSpace(c.Request().Header.Get(adminActorHeader))
}

func adminVersionAnswer(c echo.Context, status int, found bool, err error, data any) error {
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, service.ErrControllerNotDeclared) {
			return apiError(c, http.StatusConflict, "CONTROLLER_NOT_DECLARED", err.Error())
		}
		return envLookupError(c, err)
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return ok(c, status, data)
}
