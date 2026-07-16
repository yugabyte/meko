// admin_handler serves the meko_admin console endpoints (/admin/users,
// /admin/metrics). Access is gated by the admin shared secret at the
// route group; these handlers do no per-user auth of their own.
package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	adminsvc "meko/api_server/internal/service/admin"
)

// dateLayout is the accepted format for the metrics start/end query params.
const dateLayout = "2006-01-02"

// parseDateParam parses a YYYY-MM-DD query param into *time.Time. Blank
// returns (nil, nil); malformed returns an error for a 400 response.
func parseDateParam(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// AdminHandler serves the admin metrics endpoints.
type AdminHandler struct {
	svc   *adminsvc.AdminMetricsService
	dpSvc *adminsvc.AdminDataPlaneService
}

// NewAdminHandler constructs an AdminHandler.
func NewAdminHandler(svc *adminsvc.AdminMetricsService, dpSvc *adminsvc.AdminDataPlaneService) *AdminHandler {
	return &AdminHandler{svc: svc, dpSvc: dpSvc}
}

// DataPlane handles GET /api/v1/admin/dataplane — live universe configuration
// and utilization from YBA for the meko_admin Data Plane tab.
//
//	@Summary	Data plane status (admin)
//	@Description	Live YugabyteDB Anywhere universe configuration (nodes, instance type, storage, region, version) plus best-effort CPU/memory/disk utilization. `configured` is false when no YBA token is set; `available` is false when the universe can't be read.
//	@Tags		admin
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/admin/dataplane [get]
func (h *AdminHandler) DataPlane(c echo.Context) error {
	info, err := h.dpSvc.DataPlane(c.Request().Context())
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, info)
}

// Users handles GET /api/v1/admin/users.
//
//	@Summary	List all users (admin)
//	@Tags		admin
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/admin/users [get]
func (h *AdminHandler) Users(c echo.Context) error {
	users, err := h.svc.ListUsers(c.Request().Context())
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"users": users})
}

// Metrics handles GET /api/v1/admin/metrics. Query param user_id is
// optional; "" or "all" returns the overall dashboard.
//
//	@Summary	Admin dashboard metrics (overall or per-user)
//	@Tags		admin
//	@Produce	json
//	@Param		user_id	query		string	false	"User UUID, or 'all'/omitted for overall"
//	@Param		start	query		string	false	"Range start (inclusive), YYYY-MM-DD"
//	@Param		end		query		string	false	"Range end (inclusive), YYYY-MM-DD"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Router		/admin/metrics [get]
func (h *AdminHandler) Metrics(c echo.Context) error {
	userID := strings.TrimSpace(c.QueryParam("user_id"))
	start, err := parseDateParam(c.QueryParam("start"))
	if err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "start must be YYYY-MM-DD")
	}
	end, err := parseDateParam(c.QueryParam("end"))
	if err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "end must be YYYY-MM-DD")
	}
	metrics, err := h.svc.Metrics(c.Request().Context(), userID, start, end)
	if errors.Is(err, adminsvc.ErrUserNotFound) {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "user not found")
	}
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, metrics)
}

// Executive handles GET /api/v1/admin/token-savings — the executive
// dashboard payload: headcount totals, account-wide contextual-search token
// savings, and the top datapack owners by tokens saved.
//
//	@Summary	Executive dashboard summary (admin)
//	@Description	Account-wide adoption + token-savings rollup: total users/datapacks/conversations, total contextual-search requests and tokens saved, and a top-10 leaderboard of datapack owners by tokens saved (min 20 requests). Backed by the maintained token_savings_summary counters, attributed by datapack owner.
//	@Tags		admin
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/admin/token-savings [get]
func (h *AdminHandler) Executive(c echo.Context) error {
	summary, err := h.svc.Executive(c.Request().Context())
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, summary)
}

// UpdateUserTier handles PATCH /api/v1/admin/users/:id/tier. Body is
// {"tier": "pro"}; the tier is validated against the account_type enum.
//
//	@Summary	Override a user's tier (admin)
//	@Tags		admin
//	@Accept		json
//	@Produce	json
//	@Param		id	path		string	true	"User UUID"
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Failure	404	{object}	APIResponse
//	@Failure	500	{object}	APIResponse
//	@Router		/admin/users/{id}/tier [patch]
func (h *AdminHandler) UpdateUserTier(c echo.Context) error {
	userID := strings.TrimSpace(c.Param("id"))
	var body struct {
		Tier string `json:"tier"`
	}
	if err := c.Bind(&body); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_BODY", "invalid request body")
	}
	tier := strings.TrimSpace(body.Tier)
	if tier == "" {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "tier is required")
	}
	err := h.svc.UpdateUserTier(c.Request().Context(), userID, tier)
	if errors.Is(err, adminsvc.ErrInvalidTier) {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "invalid tier")
	}
	if errors.Is(err, adminsvc.ErrUserNotFound) {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "user not found")
	}
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"id": userID, "tier": tier})
}
