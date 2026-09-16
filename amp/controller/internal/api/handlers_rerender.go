package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/internal/task"
)

// RerenderEndpoints plans a re-render roll: one parent task owning a
// rerender_endpoint child per endpoint, run by the same machinery the version
// upgrade uses (architecture/ac-endpoint-tls.md).
//
// It validates only the request. Whether a given endpoint can be re-rendered is
// the planner's question, answered at Execute time; an unknown id comes back as
// a `skipped` line rather than a 404, so a roll is never refused because one
// endpoint is gone. The body is optional: no body rolls the whole fleet at the
// default parallelism.
//
// The 409 covers BOTH roll kinds and in both directions (the upgrade submit
// path shares `noFleetRollInFlight`), because they contend for the same
// `fleet:endpoints` lock. As with the upgrade it is the fast path, and the
// parent step's no-wait lock is what actually decides — without the plan-time
// check the loser would get a 202 and a task that fails in the worker.
func (c *Controllers) RerenderEndpoints(ctx context.Context, request RerenderEndpointsRequestObject) (RerenderEndpointsResponseObject, error) {
	if c.prov.Deployer == nil {
		return nil, echo.NewHTTPError(http.StatusNotImplemented, "endpoint deployment is not enabled on this controller")
	}
	// The spec's minimum/maximum are documentation: there is no wire-level
	// validator middleware, so an out-of-range value has to be rejected here or it
	// reaches the step and fails the task instead of the request.
	maxParallel := task.DefaultRollParallel
	var endpointIDs []uuid.UUID
	if request.Body != nil {
		if request.Body.MaxParallel != nil {
			maxParallel = *request.Body.MaxParallel
			if maxParallel < 1 || maxParallel > task.MaxRollParallel {
				return RerenderEndpoints400JSONResponse{BadRequestJSONResponse(apiError("invalid_request",
					fmt.Sprintf("max_parallel must be between 1 and %d", task.MaxRollParallel)))}, nil
			}
		}
		if request.Body.EndpointIds != nil {
			endpointIDs = *request.Body.EndpointIds
		}
	}

	t, err := c.tasks.RerenderEndpoints(ctx, task.RerenderEndpointsInput{
		EndpointIDs: endpointIDs,
		MaxParallel: maxParallel,
	})
	switch {
	case errors.Is(err, task.ErrFleetRollInProgress):
		c.logger.LogAttrs(ctx, slog.LevelInfo, "endpoint re-render refused: a roll is already in flight",
			slog.String("error", err.Error()))
		return RerenderEndpoints409JSONResponse{ConflictJSONResponse(apiError("conflict",
			"a fleet endpoint roll is already in progress; wait for it to finish "+
				"(GET /tasks?kind=rerender_endpoints, or kind=upgrade_endpoints) and re-run"))}, nil
	case err != nil:
		return nil, fmt.Errorf("rerender endpoints task: %w", err)
	}

	c.logger.LogAttrs(ctx, slog.LevelInfo, "endpoint re-render task accepted",
		slog.String(applog.KeyTaskID, t.ID.String()),
		slog.Int("endpoints", len(endpointIDs)),
		slog.Int("max_parallel", maxParallel))

	return RerenderEndpoints202JSONResponse{AcceptedJSONResponse{
		Body:    taskToAPI(t),
		Headers: AcceptedResponseHeaders{Location: "/api/v1/tasks/" + t.ID.String()},
	}}, nil
}
