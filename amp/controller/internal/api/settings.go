package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	applog "github.com/yugabyte/amp/controller/internal/log"
)

// Updating settings is two steps that must not be confused for one: the row is
// the durable answer, and telling a running endpoint is best effort on top.
//
// The row is written first and the write is what the response reports as
// applied. An endpoint that could not be reached, or that answers an error, does
// not fail the request — it picks the change up when it next starts, which is
// what happened before this API existed. Failing the call would leave the caller
// unsure whether the settings were stored, which is the one thing that is never
// in doubt.
//
// The endpoint applies each change the cheapest way that actually applies it,
// and reports which. A Postgres setting that can change while the server is
// running is reloaded — connections stay up — and one fixed at server start
// restarts the compute, with `restart_forced_by` naming it so the interruption
// is explained. The pageserver and safekeeper have no reload path, so a change
// to theirs is still a restart.
//
// Nothing is restarted that did not need to be: the rendered config is compared
// with what is on disk, so a write that changes nothing costs nothing.

// notifyTimeout bounds one endpoint's reload. Short because the settings are
// already durable by the time it runs: waiting longer on an endpoint that is
// wedged buys nothing the next restart will not deliver.
const notifyTimeout = 10 * time.Second

// reloadReply is the supervisor's answer to POST /v1/settings/reload.
type reloadReply struct {
	Restarted       []string `json:"restarted"`
	Reloaded        []string `json:"reloaded"`
	RestartForcedBy []string `json:"restart_forced_by"`
	NotApplied      []string `json:"not_applied"`
}

func (c *Controllers) UpdateBranchSettings(ctx context.Context, request UpdateBranchSettingsRequestObject) (UpdateBranchSettingsResponseObject, error) {
	if request.Body == nil {
		return UpdateBranchSettings400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "a settings body is required"))}, nil
	}
	// Doubles as the 404: no branch, nothing to set.
	if _, err := c.backend.GetBranch(ctx, request.PoolId, request.ClusterId, request.BranchId); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return UpdateBranchSettings404JSONResponse{NotFoundJSONResponse(apiError("not_found", "branch not found"))}, nil
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if err := validateWriteTokenSettings(request.Body.WriteTokenSettings); err != nil {
		return UpdateBranchSettings400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", err.Error()))}, nil
	}

	if err := c.backend.UpdateBranchSettings(ctx, request.BranchId,
		pgSettingsOf(request.Body.PgSettings),
		daemonSettingsOf(request.Body.WriteTokenSettings),
	); err != nil {
		return nil, fmt.Errorf("update branch settings: %w", err)
	}

	// Branch-wide, so every endpoint on it is affected.
	eps, _, err := c.backend.ListEndpoints(ctx, request.PoolId, request.ClusterId, request.BranchId, 1000, 0)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(eps))
	for _, ep := range eps {
		ids = append(ids, ep.ID)
	}
	return UpdateBranchSettings200JSONResponse{
		Applied:   true,
		Endpoints: c.notifyEndpoints(ctx, ids),
	}, nil
}

func (c *Controllers) UpdateEndpointSettings(ctx context.Context, request UpdateEndpointSettingsRequestObject) (UpdateEndpointSettingsResponseObject, error) {
	if request.Body == nil {
		return UpdateEndpointSettings400JSONResponse{BadRequestJSONResponse(apiError("invalid_request", "a settings body is required"))}, nil
	}
	if _, err := c.backend.GetEndpoint(ctx, request.PoolId, request.ClusterId, request.BranchId, request.EndpointId); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return UpdateEndpointSettings404JSONResponse{NotFoundJSONResponse(apiError("not_found", "endpoint not found"))}, nil
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}
	// The same check `createEndpoint` makes, for the same reason: a key the
	// daemon does not know aborts its startup, so it has to be caught here
	// rather than at the pod. An update that could set what a create refuses
	// would just be the long way round to the same broken endpoint.
	for _, s := range []struct {
		daemon   string
		settings *DaemonSettings
	}{
		{daemonPageserver, request.Body.PageserverSettings},
		{daemonSafekeeper, request.Body.SafekeeperSettings},
	} {
		if s.settings == nil {
			continue
		}
		if err := validateDaemonSettings(s.daemon, *s.settings); err != nil {
			return UpdateEndpointSettings400JSONResponse{BadRequestJSONResponse(apiError("invalid_argument", err.Error()))}, nil
		}
	}

	if err := c.backend.UpdateEndpointSettings(ctx, request.EndpointId,
		pgSettingsOf(request.Body.PgSettings),
		daemonSettingsOf(request.Body.PageserverSettings),
		daemonSettingsOf(request.Body.SafekeeperSettings),
	); err != nil {
		return nil, fmt.Errorf("update endpoint settings: %w", err)
	}
	return UpdateEndpointSettings200JSONResponse{
		Applied:   true,
		Endpoints: c.notifyEndpoints(ctx, []uuid.UUID{request.EndpointId}),
	}, nil
}

// notifyEndpoints asks each endpoint to re-read the registry, and reports what
// each said. Never returns an error: the settings are already stored, and an
// endpoint that cannot be told is a fact to report rather than a failure.
func (c *Controllers) notifyEndpoints(ctx context.Context, ids []uuid.UUID) *[]EndpointSettingsOutcome {
	if len(ids) == 0 {
		return nil
	}
	out := make([]EndpointSettingsOutcome, 0, len(ids))
	for _, id := range ids {
		outcome := EndpointSettingsOutcome{EndpointId: id}
		switch reply, err := c.notifyEndpoint(ctx, id); {
		case err != nil:
			reason := err.Error()
			outcome.Unreachable = &reason
			c.logger.LogAttrs(ctx, slog.LevelInfo, "endpoint not told about a settings change",
				slog.String(applog.KeyEndpointID, id.String()), slog.String("error", reason))
		default:
			outcome.Restarted = &reply.Restarted
			outcome.Reloaded = &reply.Reloaded
			if len(reply.RestartForcedBy) > 0 {
				outcome.RestartForcedBy = &reply.RestartForcedBy
			}
			// Only when there is something to say: the caller stored these and
			// they are not in force, which is a warning rather than part of a
			// normal reply.
			if len(reply.NotApplied) > 0 {
				outcome.NotApplied = &reply.NotApplied
			}
		}
		out = append(out, outcome)
	}
	return &out
}

func (c *Controllers) notifyEndpoint(ctx context.Context, id uuid.UUID) (reloadReply, error) {
	if c.prov.Deployer == nil {
		return reloadReply{}, errors.New("this controller does not deploy endpoints")
	}
	base := c.prov.Deployer.EndpointSupervisorURL(endpointDeployName(id))
	if base == "" {
		return reloadReply{}, errors.New("this deployer cannot reach an endpoint's settings API")
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/settings/reload", nil)
	if err != nil {
		return reloadReply{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return reloadReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return reloadReply{}, fmt.Errorf("settings reload returned %s", resp.Status)
	}
	var reply reloadReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return reloadReply{}, fmt.Errorf("decoding the reload reply: %w", err)
	}
	if reply.Restarted == nil {
		reply.Restarted = []string{}
	}
	if reply.Reloaded == nil {
		reply.Reloaded = []string{}
	}
	return reply, nil
}

// pgSettingsOf and daemonSettingsOf convert a request field to what the backend
// takes. Both preserve the nil/empty distinction the whole API rests on: nil is
// "leave this surface alone", empty is "clear it". Collapsing them would make an
// omitted field silently destructive.
func pgSettingsOf(m *map[string]string) *map[string]string { return m }

func daemonSettingsOf[T ~map[string]any](m *T) *map[string]any {
	if m == nil {
		return nil
	}
	out := map[string]any(*m)
	return &out
}

// validateWriteTokenSettings rejects a lease that would fence the endpoint out
// on a timer.
//
// Checked here as well as in the supervisor because the two failures are not
// the same: the supervisor refusing means one endpoint keeps its old timings and
// logs why, which nobody is watching. Refusing here means the caller is told,
// before a row exists that every endpoint on the branch will read.
func validateWriteTokenSettings(m *WriteTokenSettings) error {
	if m == nil {
		return nil
	}
	ttlMS, okTTL, err := positiveInt(*m, "ttl_ms")
	if err != nil {
		return err
	}
	renewS, okRenew, err := positiveInt(*m, "renew_seconds")
	if err != nil {
		return err
	}
	for k := range *m {
		if k != "ttl_ms" && k != "renew_seconds" {
			return fmt.Errorf("write_token_settings: unknown key %q (want ttl_ms, renew_seconds)", k)
		}
	}
	// Only comparable when the caller sent both. Sending one against a stored
	// other is checked by the supervisor, which is the only place that knows
	// what the other currently is.
	if okTTL && okRenew && renewS*1000 >= ttlMS {
		return fmt.Errorf("write_token_settings: renew_seconds (%d) must be under ttl_ms (%d): the gap is how long a failing renewal may be retried before anything stops writing", renewS, ttlMS)
	}
	return nil
}

// positiveInt reads a JSON number that has to be a positive whole number.
// JSON has no integers, so a value that arrived as 1.5 or as a string is a
// caller error worth naming rather than truncating.
func positiveInt(m map[string]any, key string) (int64, bool, error) {
	v, ok := m[key]
	if !ok {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, fmt.Errorf("write_token_settings.%s must be a number, got %T", key, v)
	}
	if f != float64(int64(f)) || f <= 0 {
		return 0, false, fmt.Errorf("write_token_settings.%s must be a positive whole number, got %v", key, f)
	}
	return int64(f), true, nil
}
