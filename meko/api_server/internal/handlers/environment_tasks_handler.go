package handlers

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// EnvironmentTasksHandler serves the amp task history the reconcile agent relays for
// one environment, so the console can answer "what is running" and "what failed"
// without a path into the customer's cluster.
type EnvironmentTasksHandler struct {
	envSvc *service.EnvironmentService
}

// NewEnvironmentTasksHandler constructs the handler.
func NewEnvironmentTasksHandler(envSvc *service.EnvironmentService) *EnvironmentTasksHandler {
	return &EnvironmentTasksHandler{envSvc: envSvc}
}

const (
	tasksDefaultPageSize = 25
	tasksMaxPageSize     = 200
)

// environmentTaskListResponse mirrors the paginated envelope the other list
// surfaces use, plus the environment's last heartbeat.
type environmentTaskListResponse struct {
	Items    []models.EnvironmentTask `json:"items"`
	Total    int                      `json:"total"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"page_size"`
	HasMore  bool                     `json:"has_more"`
	// LastHeartbeatAt is when the agent last reported anything at all. Without it a
	// console cannot tell a task that has not moved from an agent that has stopped
	// talking: both render as a row whose status is old.
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
}

// List answers GET /environments/:id/tasks.
//
// The environment is resolved against the calling account FIRST, so another
// account's environment is a 404 exactly as on every other fleet route, and the task
// table is not touched. That resolved row's uuid -- not anything in the query string
// -- is what the history is read by, which is what makes the account check an
// authorization check rather than a filter a caller could widen.
func (h *EnvironmentTasksHandler) List(c echo.Context) error {
	e, err := h.envSvc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}

	page := positiveIntParam(c, "page", 1)
	pageSize := positiveIntParam(c, "page_size", tasksDefaultPageSize)
	if pageSize > tasksMaxPageSize {
		pageSize = tasksMaxPageSize
	}

	opts := repository.EnvironmentTaskListOpts{
		Limit:     pageSize,
		Offset:    (page - 1) * pageSize,
		Statuses:  taskStatusFilter(c),
		Kind:      strings.TrimSpace(c.QueryParam("kind")),
		ClusterID: strings.TrimSpace(c.QueryParam("cluster_id")),
	}

	// A meko environment runs no controller and so has no task log; its history is
	// simply empty. Deliberately not an error: the console renders one tab for every
	// environment kind, and "nothing has run yet" and "this kind does not run tasks"
	// should read alike rather than one of them breaking the tab.
	result, err := h.envSvc.Tasks(e.UUID, opts)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "TASK_LIST_FAILED", err.Error())
	}

	resp := environmentTaskListResponse{
		Items:           result.Items,
		Total:           result.Total,
		Page:            page,
		PageSize:        pageSize,
		HasMore:         result.HasMore,
		LastHeartbeatAt: e.LastHeartbeatAt,
	}
	return ok(c, http.StatusOK, resp)
}

// Get answers GET /environments/:id/tasks/:task_id, for a console drilling into one
// operation. Same account resolution as List.
func (h *EnvironmentTasksHandler) Get(c echo.Context) error {
	e, err := h.envSvc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}

	t, err := h.envSvc.Task(e.UUID, strings.TrimSpace(c.Param("task_id")))
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "TASK_GET_FAILED", err.Error())
	}
	if t == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such task for this environment")
	}
	return ok(c, http.StatusOK, t)
}

// taskStatusFilter reads the status filter: repeatable (?status=a&status=b), plus
// ?active=true as shorthand for the non-terminal pair, which is the console's
// primary query.
//
// Unrecognized values are DROPPED rather than rejected, the same choice the KB
// document list makes: a stale client asking for a status this control plane does not
// know gets the unfiltered page rather than a 400 it cannot act on.
func taskStatusFilter(c echo.Context) []string {
	var out []string
	for _, raw := range c.QueryParams()["status"] {
		for _, v := range strings.Split(raw, ",") {
			v = strings.ToLower(strings.TrimSpace(v))
			if slices.Contains(models.EnvironmentTaskStatuses, v) && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	if active, _ := strconv.ParseBool(c.QueryParam("active")); active {
		for _, v := range []string{"pending", "running"} {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// positiveIntParam reads an optional positive-integer query parameter, falling back
// to def for anything absent or unusable.
func positiveIntParam(c echo.Context, param string, def int) int {
	v := strings.TrimSpace(c.QueryParam(param))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
