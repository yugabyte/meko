package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
)

// DatapackMetricsHandler exposes per-datapack analytics metrics for the
// dashboard (MEKO-629).
type DatapackMetricsHandler struct {
	repo *repository.DatapackMetricsRepo
}

// NewDatapackMetricsHandler creates a new DatapackMetricsHandler.
func NewDatapackMetricsHandler(repo *repository.DatapackMetricsRepo) *DatapackMetricsHandler {
	return &DatapackMetricsHandler{repo: repo}
}

// GetMetrics handles GET /api/v1/datapacks/{datapack_id}/metrics.
//
//	@Summary	Get all analytics metrics for a datapack
//	@Description	Returns token savings, score bands, agent/human access counts, daily activity (last 30 days), conversation count, and memory extraction rate for the given datapack.
//	@Tags		datapacks
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack UUID"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/metrics [get]
func (h *DatapackMetricsHandler) GetMetrics(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing datapack context")
	}

	metrics, err := h.repo.GetMetrics(c.Request().Context(), dp.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, metrics)
}
