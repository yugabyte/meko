package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// EnvironmentHandler serves the YCP fleet-registry API (control-plane mode):
// console/admin routes plus the agent enroll/status/desired-state surface.
type EnvironmentHandler struct {
	svc *service.EnvironmentService
}

// NewEnvironmentHandler constructs the handler.
func NewEnvironmentHandler(svc *service.EnvironmentService) *EnvironmentHandler {
	return &EnvironmentHandler{svc: svc}
}

// accountScope reads the account this fleet call acts for. Empty is unscoped, and on
// the fleet group only reaches here when --require-account-header is off; the group
// middleware rejects it otherwise. The admin routes never require one. See
// auth.AccountIDHeader.
func accountScope(c echo.Context) service.AccountScope {
	return service.AccountScope(strings.TrimSpace(c.Request().Header.Get(auth.AccountIDHeader)))
}

// envLookupError answers a failed environment lookup. An unscoped caller naming an
// id two accounts hold is the caller's to resolve, by sending one — not a 500.
func envLookupError(c echo.Context, err error) error {
	if errors.Is(err, service.ErrAmbiguousID) {
		return apiError(c, http.StatusConflict, "AMBIGUOUS_ID", err.Error())
	}
	return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
}

// ---- console / admin ----

// List returns the caller's environments, or the whole fleet when unscoped.
func (h *EnvironmentHandler) List(c echo.Context) error {
	envs, err := h.svc.List(accountScope(c))
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, map[string]any{"items": envs})
}

// Get returns one environment. Another account's environment is a 404, the same
// as one that does not exist.
//
// The install hint rides along: register's copy is gone once the operator navigates
// away, and nothing else can reproduce it. Rendered per request rather than stored,
// from the environment's own agent base URL — or, for an environment registered
// without one, from the address this call arrived on.
func (h *EnvironmentHandler) Get(c echo.Context) error {
	e, err := h.svc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	e.InstallHint = installHint(c, e, h.svc.BYOCMetricsURL())
	return ok(c, http.StatusOK, e)
}

// Register stores a new PENDING environment bound to the request's api_key_id. A
// name this account already holds answers 409 ENVIRONMENT_EXISTS.
func (h *EnvironmentHandler) Register(c echo.Context) error {
	var req service.RegisterEnvironmentRequest
	if !bindJSON(c, &req) {
		return nil
	}
	e, err := h.svc.Register(req, accountScope(c))
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		// A customer (scoped caller) may not register Yugabyte-operated MANAGED
		// infrastructure; only the unscoped operator path can.
		if errors.Is(err, service.ErrManagedRequiresOperator) {
			return apiError(c, http.StatusForbidden, "MANAGED_REQUIRES_OPERATOR", err.Error())
		}
		if errors.Is(err, service.ErrAPIKeyBound) {
			return apiError(c, http.StatusConflict, "API_KEY_BOUND",
				"this api key is already bound to another environment; one key drives one environment")
		}
		if errors.Is(err, service.ErrEnvironmentExists) {
			// err names the environment's id -- the slug that was stored, which is
			// what the API keys on and what an operator needs to find the row holding
			// the name. Not req.Name, which differs from it whenever slug() changed
			// anything.
			return apiError(c, http.StatusConflict, "ENVIRONMENT_EXISTS",
				err.Error()+"; the name stays claimed for the life of the row, deregistered or not")
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	body := map[string]any{
		"environment":  e,
		"install_hint": installHint(c, e, h.svc.BYOCMetricsURL()),
	}
	return ok(c, http.StatusCreated, body)
}

// SetDesired replaces the environment's desired resource set. The admin-only kinds
// are outside it: a body naming one is a 400, and a body omitting one leaves it.
//
// The set is authoritative and the agent garbage-collects what it omits, so emptying
// it deletes the environment's pool, cluster, branch and endpoint. That makes the
// destructive write the one a malformed body must not be able to reach: `resources`
// is required (a pointer, so an absent key is not an empty slice — the same
// distinction RegisterEnvironmentRequest.Desired already draws), unknown fields are
// refused rather than dropped, and an explicit empty set has to say so twice.
func (h *EnvironmentHandler) SetDesired(c echo.Context) error {
	var body struct {
		Resources *[]models.DesiredResource `json:"resources"`
		// AllowEmpty confirms an empty Resources. A body field rather than a query
		// parameter because YBM's pass-through forwards the JSON body and nothing
		// else — as a parameter it would never arrive, leaving no way to withdraw a
		// stack through YBM at all.
		AllowEmpty bool `json:"allow_empty"`
	}
	if !bindJSONStrict(c, &body) {
		return nil
	}
	if body.Resources == nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			`resources is required: desired state is replaced, not merged, so send the whole set. `+
				`To withdraw every resource, send {"resources": [], "allow_empty": true}.`)
	}
	if len(*body.Resources) == 0 && !body.AllowEmpty {
		return apiError(c, http.StatusBadRequest, "EMPTY_DESIRED_STATE",
			`an empty resources array withdraws every resource this environment declares, and the `+
				`agent deletes them on its next tick; resend with "allow_empty": true to confirm.`)
	}
	found, err := h.svc.SetDesired(c.Param("id"), accountScope(c), *body.Resources)
	if err != nil {
		// A kind this environment's agent does not serve, or one only the admin API
		// sets, is the caller's mistake, so it is a 400 here rather than the 500
		// envLookupError would give it.
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return envLookupError(c, err)
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return c.NoContent(http.StatusAccepted)
}

// GetDesired returns the environment's desired resource set, without the admin-only
// kinds: the admin routes read those.
//
// The agent-facing GET resolves the environment from a key id bound to it, which an
// operator has no way to hold — the console can send the headers, but any key id it
// mints resolves to nothing. Without this route desired state is write-only from the
// console: you could send a command and never read back what an environment was told
// to run.
func (h *EnvironmentHandler) GetDesired(c echo.Context) error {
	e, err := h.svc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	res, err := h.svc.Desired(e.UUID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if res == nil {
		res = []models.DesiredResource{}
	}
	return ok(c, http.StatusOK, map[string]any{"resources": res})
}

// Action applies an operator lifecycle action (suspend|resume|drain|deregister).
//
// A deregister answers like DELETE — 200 with the api_key_id binding — because the
// two are the same operation reached two ways, and a caller that deregisters through
// PATCH needs the same thing to revoke. The other actions keep their bare 202: they
// leave the binding in place.
func (h *EnvironmentHandler) Action(c echo.Context) error {
	var body struct {
		Action string `json:"action"`
	}
	if !bindJSON(c, &body) {
		return nil
	}
	e, err := h.svc.Action(c.Param("id"), accountScope(c), body.Action)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	if body.Action == "deregister" {
		return ok(c, http.StatusOK, map[string]any{"api_key_id": e.APIKeyID})
	}
	return c.NoContent(http.StatusAccepted)
}

// Deregister marks the environment DEREGISTERED. A state change, not a row delete:
// the fleet keeps the record (and its history) after an environment goes away. The
// response carries the environment's api_key_id so YBM can revoke the key nothing
// authenticates with any more.
func (h *EnvironmentHandler) Deregister(c echo.Context) error {
	e, err := h.svc.Action(c.Param("id"), accountScope(c), "deregister")
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return ok(c, http.StatusOK, map[string]any{"api_key_id": e.APIKeyID})
}

// Entitlements reports the plan and limits the console renders against.
func (h *EnvironmentHandler) Entitlements(c echo.Context) error {
	return ok(c, http.StatusOK, h.svc.Entitlement())
}

// ---- agent surface ----
//
// Every route here sits behind RequireAdminSecret + RequireAPIKeyID (YBM
// authenticates the agent's API key and forwards its id), and resolves the
// environment from that id alone: one key drives one environment.

// AgentBootstrapProgress advances an environment's state from agent-reported
// bootstrap phases.
func (h *EnvironmentHandler) AgentBootstrapProgress(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	var bp struct {
		Phase string `json:"phase"`
		Step  string `json:"step"`
	}
	if !bindJSON(c, &bp) {
		return nil
	}
	// No "empty phase is a quiet no-op" branch: phase is required by the spec, and
	// answering 204 to a report that moved nothing is the pattern this ticket is
	// about. The found bool is checked for the same reason, and answers 404 as
	// AgentStatus does -- the two agent-facing writes must not disagree about what
	// an unknown environment looks like.
	found, err := h.svc.SetState(e.UUID, bp.Phase)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "unknown environment")
	}
	return c.NoContent(http.StatusNoContent)
}

// AgentEnroll is the agent's first check-in: it binds the live agent to the
// environment its API key was registered with and hands back everything the agent
// needs to run.
func (h *EnvironmentHandler) AgentEnroll(c echo.Context) error {
	var req service.EnrollRequest
	if !bindJSON(c, &req) {
		return nil
	}
	e, err := h.svc.EnrollByKey(auth.GetAPIKeyID(c), req.Placement, req.Kind, req.ClusterFacts, req.Telemetry)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, service.ErrKindMismatch) {
			return apiError(c, http.StatusConflict, "KIND_MISMATCH", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if e == nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "no environment is bound to this api key")
	}
	// environment_uuid is the row key, globally unique where the id is a slug unique
	// only within an account. Nothing consumes it yet; it is handed over so an agent
	// records an unambiguous identity.
	body := map[string]any{
		"environment_id": e.ID, "environment_uuid": e.UUID, "mcp_base_url": e.MCPBaseURL,
	}
	return ok(c, http.StatusOK, body)
}

// AgentDesiredState returns the generic desired-state resources, the admin-only kinds
// included. With ?since=<change_token>&wait=<duration> it is a long-poll; see
// EnvironmentService.AgentDesired. Without both it answers at once, as it always has.
func (h *EnvironmentHandler) AgentDesiredState(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	since, wait, hold := longPollParams(c)
	ctx := c.Request().Context()
	token, res, err := h.svc.AgentDesired(ctx, e.UUID, since, wait, hold)
	if err != nil {
		if ctx.Err() != nil {
			return nil // the agent hung up; nobody is reading an answer
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if res == nil {
		res = []models.DesiredResource{}
	}
	// Revision tracks the desired set as a whole: the max per-resource generation,
	// so the agent's reported AppliedRevision advances whenever any spec changes.
	// A create or a delete does not move it, which is why the long-poll keys on
	// change_token instead.
	var revision int64 = 1
	for _, d := range res {
		if d.Generation > revision {
			revision = d.Generation
		}
	}
	return ok(c, http.StatusOK, map[string]any{
		"revision":        revision,
		"change_token":    token,
		"long_poll":       true, // tells the agent this server holds; an older one ignores since/wait
		"target_versions": map[string]string{"agent": "v1.4.2", "mcp": "v1.4.2", "rag": "v1.4.2"},
		"resources":       res,
	})
}

// longPollParams reads since and wait. wait is a Go duration ("25s") or a bare number
// of seconds. hold is false unless both are present and valid, so a malformed request
// degrades to an immediate answer rather than an error.
func longPollParams(c echo.Context) (since int64, wait time.Duration, hold bool) {
	s, w := c.QueryParam("since"), c.QueryParam("wait")
	if s == "" || w == "" {
		return 0, 0, false
	}
	since, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if n, nerr := strconv.Atoi(w); nerr == nil {
		wait = time.Duration(n) * time.Second
	} else if wait, err = time.ParseDuration(w); err != nil {
		return 0, 0, false
	}
	if wait <= 0 {
		return 0, 0, false
	}
	return since, wait, true
}

// AgentStatus ingests a status report.
func (h *EnvironmentHandler) AgentStatus(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	var rep service.StatusReport
	if !bindJSON(c, &rep) {
		return nil
	}
	res, err := h.svc.ApplyStatus(e.UUID, rep)
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !res.Found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "unknown environment")
	}
	// A task delta that could not be stored is NOT a failed heartbeat: the state and
	// inventory this report carried are durable, and answering 5xx would have the
	// agent re-send a snapshot that already landed. But it is also not a plain
	// success, because a delta is delivered once -- an agent told "accepted" retires
	// it, and the transition is then unrecoverable. So say so in the body and let the
	// agent hold that delta for the next tick.
	//
	// 200-with-a-body only in that case; the common path stays 204, so a report with
	// no tasks (every meko agent, and most amp ticks) is unchanged on the wire.
	if !res.TasksRecorded {
		return ok(c, http.StatusOK, map[string]any{"tasks_recorded": false})
	}
	return c.NoContent(http.StatusNoContent)
}

// AgentExtensions stores the environment's extension catalog, which the agent sends
// when it changes rather than on every heartbeat. A non-amp environment is a 400.
func (h *EnvironmentHandler) AgentExtensions(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	var rep service.ExtensionCatalogReport
	if !bindJSON(c, &rep) {
		return nil
	}
	if err := h.svc.ApplyExtensions(e.UUID, e.Kind, rep); err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// Extensions answers GET /environments/:id/extensions. 200 with extensions: null
// when no catalog has been relayed — an older agent, a meko environment — so a
// console can fall back instead of offering nothing.
func (h *EnvironmentHandler) Extensions(c echo.Context) error {
	e, err := h.svc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	cat, err := h.svc.Extensions(e.UUID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "EXTENSION_LIST_FAILED", err.Error())
	}
	return ok(c, http.StatusOK, cat)
}

// ImportExtension declares an extension for the environment's agent to register in
// its controller's catalog. 202: registration happens on the agent's next tick, and
// its outcome is the extension resource in the environment's reported inventory.
func (h *EnvironmentHandler) ImportExtension(c echo.Context) error {
	var spec service.ExtensionImport
	if !bindJSONStrict(c, &spec) {
		return nil
	}
	d, found, err := h.svc.ImportExtension(c.Param("id"), accountScope(c), c.Param("name"), spec)
	return h.extensionAnswer(c, found, err, d)
}

// RemoveExtension withdraws an extension's declaration, which the agent then
// deregisters. 202 for the same reason as ImportExtension.
func (h *EnvironmentHandler) RemoveExtension(c echo.Context) error {
	found, err := h.svc.RemoveExtension(c.Param("id"), accountScope(c), c.Param("name"))
	return h.extensionAnswer(c, found, err, nil)
}

func (h *EnvironmentHandler) extensionAnswer(c echo.Context, found bool, err error, d *models.DesiredResource) error {
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		return envLookupError(c, err)
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	if d == nil {
		return c.NoContent(http.StatusAccepted)
	}
	return ok(c, http.StatusAccepted, d)
}

// resolveAgent resolves the environment from the call's API key id, returning nil
// (having written the error) when the key is bound to no environment. A revoked
// key and a never-registered one are deliberately the same 401.
func (h *EnvironmentHandler) resolveAgent(c echo.Context) *models.Environment {
	e, err := h.svc.ResolveAgent(auth.GetAPIKeyID(c))
	if err != nil {
		_ = apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return nil
	}
	if e == nil {
		_ = apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "no environment is bound to this api key")
		return nil
	}
	return e
}
