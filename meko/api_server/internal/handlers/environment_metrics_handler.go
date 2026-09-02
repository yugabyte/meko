package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/service"
)

// EnvironmentMetricsHandler serves one environment's amp metrics out of the
// control plane's metrics store, the way YBM apiserver serves a cluster's.
type EnvironmentMetricsHandler struct {
	envSvc     *service.EnvironmentService
	metricsSvc *service.MetricsService
}

// NewEnvironmentMetricsHandler constructs the handler.
func NewEnvironmentMetricsHandler(
	envSvc *service.EnvironmentService, metricsSvc *service.MetricsService,
) *EnvironmentMetricsHandler {
	return &EnvironmentMetricsHandler{envSvc: envSvc, metricsSvc: metricsSvc}
}

// Get answers GET /environments/:id/metrics.
//
// The environment is resolved against the calling account first, so another
// account's environment is a 404 exactly as it is on every other fleet route. That
// resolved row — not anything in the query string — supplies the environment_id and
// account_id the PromQL is scoped by, which is what stops a caller reading beyond
// the environment it named.
func (h *EnvironmentMetricsHandler) Get(c echo.Context) error {
	e, err := h.envSvc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}

	query := service.MetricsQuery{
		Names:    c.QueryParams()["metrics"],
		Endpoint: c.QueryParam("endpoint"),
		Pod:      c.QueryParam("pod"),
	}
	if query.Start, err = epochParam(c, "start_time"); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	}
	if query.End, err = epochParam(c, "end_time"); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	}

	result, err := h.metricsSvc.Range(c.Request().Context(), e, query)
	switch {
	case errors.Is(err, service.ErrMetricsNotConfigured):
		return apiError(c, http.StatusServiceUnavailable, "METRICS_NOT_CONFIGURED",
			"this control plane is not configured with a metrics store")
	case errors.Is(err, service.ErrValidation):
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case err != nil:
		return apiError(c, http.StatusBadGateway, "METRICS_QUERY_FAILED", err.Error())
	}
	return ok(c, http.StatusOK, result)
}

// epochParam reads an optional epoch-seconds query parameter. Absent is the zero
// time, which the service reads as "default this end of the window".
func epochParam(c echo.Context, param string) (time.Time, error) {
	raw := c.QueryParam(param)
	if raw == "" {
		return time.Time{}, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, errors.New(param + " must be a non-negative epoch-seconds integer")
	}
	return time.Unix(seconds, 0), nil
}
