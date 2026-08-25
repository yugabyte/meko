package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

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

// accountScope reads the account this fleet call acts for. Empty is unscoped, and
// only reaches here when --require-account-header is off; the group middleware
// rejects it otherwise. See auth.AccountIDHeader.
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
func (h *EnvironmentHandler) Get(c echo.Context) error {
	e, err := h.svc.Get(c.Param("id"), accountScope(c))
	if err != nil {
		return envLookupError(c, err)
	}
	if e == nil {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return ok(c, http.StatusOK, e)
}

// Register stores a PENDING environment bound to the request's api_key_id. When a
// re-registration rebound an existing row to a different key, the response carries
// the old id as replaced_api_key_id so YBM can revoke it.
func (h *EnvironmentHandler) Register(c echo.Context) error {
	var req service.RegisterEnvironmentRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	e, replacedKeyID, err := h.svc.Register(req, accountScope(c))
	if err != nil {
		if errors.Is(err, service.ErrValidation) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		if errors.Is(err, service.ErrAPIKeyBound) {
			return apiError(c, http.StatusConflict, "API_KEY_BOUND",
				"this api key is already bound to another environment; one key drives one environment")
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	body := map[string]any{
		"environment":  e,
		"install_hint": installHint(c, e, req.AgentBaseURL),
	}
	if replacedKeyID != "" {
		body["replaced_api_key_id"] = replacedKeyID
	}
	return ok(c, http.StatusCreated, body)
}

// SetDesired replaces the environment's desired resource set.
func (h *EnvironmentHandler) SetDesired(c echo.Context) error {
	var body struct {
		Resources []models.DesiredResource `json:"resources"`
	}
	if err := c.Bind(&body); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	found, err := h.svc.SetDesired(c.Param("id"), accountScope(c), body.Resources)
	if err != nil {
		return envLookupError(c, err)
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "no such environment")
	}
	return c.NoContent(http.StatusAccepted)
}

// GetDesired returns the environment's desired resource set.
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
	if err := c.Bind(&body); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
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
	if err := c.Bind(&bp); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if bp.Phase != "" {
		if _, err := h.svc.SetState(e.UUID, bp.Phase); err != nil {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// AgentEnroll is the agent's first check-in: it binds the live agent to the
// environment its API key was registered with and hands back everything the agent
// needs to run.
func (h *EnvironmentHandler) AgentEnroll(c echo.Context) error {
	var req service.EnrollRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	e, err := h.svc.EnrollByKey(auth.GetAPIKeyID(c), req.Placement, req.Kind, req.ClusterFacts)
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
	// The stack comes back with enrollment, so an agent that enrolls can deploy the
	// whole data plane from this one response — no second call, and nothing for an
	// operator to command. A failure to read it is logged and omitted rather than
	// failing the enrollment: the agent retries the read on every desired-state tick
	// anyway.
	if stack, err := h.svc.EnrolledStack(e.UUID); err != nil {
		slog.Warn("could not read the stack to hand the enrolling agent",
			"environment", e.ID, "error", err)
	} else if len(stack) > 0 {
		body["stack"] = stack
	}
	return ok(c, http.StatusOK, body)
}

// AgentDesiredState returns the generic desired-state resources.
func (h *EnvironmentHandler) AgentDesiredState(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	res, err := h.svc.Desired(e.UUID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if res == nil {
		res = []models.DesiredResource{}
	}
	// Revision tracks the desired set as a whole: the max per-resource generation,
	// so the agent's reported AppliedRevision advances whenever any spec changes.
	var revision int64 = 1
	for _, d := range res {
		if d.Generation > revision {
			revision = d.Generation
		}
	}
	return ok(c, http.StatusOK, map[string]any{
		"revision":        revision,
		"target_versions": map[string]string{"agent": "v1.4.2", "mcp": "v1.4.2", "rag": "v1.4.2"},
		"resources":       res,
	})
}

// AgentStatus ingests a status report.
func (h *EnvironmentHandler) AgentStatus(c echo.Context) error {
	e := h.resolveAgent(c)
	if e == nil {
		return nil
	}
	var rep service.StatusReport
	if err := c.Bind(&rep); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	found, err := h.svc.ApplyStatus(e.UUID, rep)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	if !found {
		return apiError(c, http.StatusNotFound, "NOT_FOUND", "unknown environment")
	}
	return c.NoContent(http.StatusNoContent)
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
