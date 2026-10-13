package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
	"meko/api_server/internal/telemetry"
)

// DatapackHandler holds Echo handlers for datapack endpoints.
type DatapackHandler struct {
	svc         *service.DatapackService
	langfuse    *service.LangfuseService
	memoriesSvc *service.MemoriesService
	kbSvc       *service.KnowledgeBaseService
	// projectSvc: nil unless the Projects feature is enabled. When set, Create
	// requires the new datapack to name a folder and files it there.
	projectSvc *service.ProjectService
}

// SetProjects enables the "new datapacks live in a folder" rule on Create.
// Unset (the default) Create behaves exactly as it did before Projects.
func (h *DatapackHandler) SetProjects(p *service.ProjectService) {
	h.projectSvc = p
}

// NewDatapackHandler creates a new DatapackHandler.
func NewDatapackHandler(
	svc *service.DatapackService,
	langfuse *service.LangfuseService,
	memoriesSvc *service.MemoriesService,
	kbSvc *service.KnowledgeBaseService,
) *DatapackHandler {
	return &DatapackHandler{
		svc:         svc,
		langfuse:    langfuse,
		memoriesSvc: memoriesSvc,
		kbSvc:       kbSvc,
	}
}

// Create handles POST /api/v1/datapacks.
//
//	@Summary	Create a datapack
//	@Tags		datapacks
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.CreateRequest	true	"Create request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks [post]
func (h *DatapackHandler) Create(c echo.Context) error {
	var req service.CreateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	if account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	req.UserID = account.ID
	req.UserType = account.AccountType
	req.Email = account.Email
	// POST /datapacks is on the PAT-accepting dpDirect group AND on the
	// Cognito-authed group, so this handler serves the UI, the CLI, and
	// meko-mcp-server's proxy calls. Derive `source` from the auth
	// method (JWT → ui, PAT → mcp/api depending on the X-Meko-Client
	// hint); the hardcoded UI tag would otherwise mis-slice every non-
	// browser create in the funnel.
	authMethod, _ := c.Get(auth.ContextKeyAuthMethod).(string)
	req.Source = telemetry.SourceFromAuthMethod(authMethod, c.Request().Header.Get(clientHintHeader))

	// Validated BEFORE the datapack exists: a bad folder_id must fail the
	// request outright rather than leave an unplaceable datapack behind.
	// Omitting folder_id is not an error — the datapack is created unfiled.
	if h.projectSvc != nil {
		if fErr := h.projectSvc.ValidateNewDatapackFolder(account.ID, req.FolderID); fErr != nil {
			status, code := projectStatus(fErr)
			return apiError(c, status, code, fErr.Error())
		}
	}

	resp, err := h.svc.CreateProvisioned(c.Request().Context(), req, account)
	if err != nil {
		// Checked first: the wrapped upstream error names internal Langfuse
		// hosts and must only be logged (CreateProvisioned already did).
		if errors.Is(err, service.ErrCreateRollbackFailed) {
			return apiError(c, http.StatusServiceUnavailable, "LANGFUSE_UNAVAILABLE",
				"Could not finish setting up the datapack. Please retry.")
		}
		if errors.Is(err, service.ErrLangfuseUnavailable) {
			return apiError(c, http.StatusServiceUnavailable, "LANGFUSE_UNAVAILABLE",
				"Could not finish setting up the datapack; nothing was created. Please retry.")
		}
		// Tier datapack cap → 402. DatapackLimitResponse dispatches on the tier
		// (Free → Standard, Standard → Pro) — the cap fires for both gated tiers
		// (datapack_service.go), so a Standard user must not be told to upgrade
		// to Standard.
		if errors.Is(err, service.ErrFreeTierDatapackLimit) {
			return respondTierLimit(c, service.DatapackLimitResponse(account.AccountType))
		}
		if isDatapackValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isConflictErr(err) {
			return apiError(c, http.StatusConflict,
				"CONFLICT", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return internalError(c, err, "create the datapack")
	}

	// File it. Best-effort: the folder was already validated above, so a
	// failure here is an outage rather than bad input, and the datapack now
	// exists — failing the response would tell the caller their datapack
	// wasn't created when it was. It lands unfiled instead, which the
	// Datapacks page offers a one-click fix for.
	if h.projectSvc != nil && strings.TrimSpace(req.FolderID) != "" {
		if pErr := h.projectSvc.Place(account.ID, resp.DatapackID, req.FolderID); pErr != nil {
			slog.Warn("datapack created but filing failed (non-fatal; stays unfiled)",
				"datapack_id", resp.DatapackID, "folder_id", req.FolderID, "error", pErr)
		}
	}

	redactCreateResponse(resp)
	return ok(c, http.StatusCreated, resp)
}

// Describe handles GET /api/v1/datapacks/:datapack_id.
// The datapack is resolved by RequireDatapackAccess middleware which
// supports both owners and shared users. The Grant field is already
// populated by the repository layer.
//
//	@Summary	Describe a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id} [get]
func (h *DatapackHandler) Describe(c echo.Context) error {
	dp, found := c.Get(auth.ContextKeyDatapack).(*models.Datapack)
	if !found || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	if h.memoriesSvc != nil {
		if resp, err := h.memoriesSvc.Count(c.Request().Context(), dp.ID); err != nil {
			slog.Warn("failed to count memories", "datapack_id", dp.ID, "error", err)
		} else {
			dp.MemoryCount = resp.Count
		}
		// Sibling count: only memories still awaiting a decision
		// (meko_promotion_user_action IS NULL). Failure is non-fatal —
		// learnings_count stays zero and the UI degrades gracefully.
		if resp, err := h.memoriesSvc.CountLearnings(c.Request().Context(), dp.ID); err != nil {
			slog.Warn("failed to count learnings", "datapack_id", dp.ID, "error", err)
		} else {
			dp.LearningsCount = resp.Count
		}
	}

	if h.kbSvc != nil {
		if kbCounts, err := h.kbSvc.Counts(dp); err != nil {
			slog.Warn("failed to count knowledge base items", "datapack_id", dp.ID, "error", err)
		} else {
			dp.KnowledgeCount = int(kbCounts.TotalChunks)
			dp.CollectiveMemoryCount = int(kbCounts.PromotedMemories)
		}
	}

	if optedOut, err := h.svc.ConversationSearchOptedOut(dp.ID); err != nil {
		slog.Warn("failed to read conversation search opt-out", "datapack_id", dp.ID, "error", err)
	} else {
		dp.ConversationSearchOptOut = optedOut
	}

	redactDatapack(dp)
	return ok(c, http.StatusOK, dp)
}

// setConversationSearchOptOutRequest is the body for
// PATCH /datapacks/:datapack_id/conversation-search-opt-out.
type setConversationSearchOptOutRequest struct {
	OptedOut bool `json:"opted_out"`
}

// SetConversationSearchOptOut handles PATCH /datapacks/:datapack_id/conversation-search-opt-out.
// Restricted to the datapack's owner or a maintainer (see
// auth.RequireDatapackOwnerOrMaintainerGrant on this route) — deliberately
// NOT folded into Update, which is owner-only.
//
//	@Summary	Set a datapack's conversation-search opt-out
//	@Tags		datapacks
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string								true	"Datapack ID"
//	@Param		body		body		setConversationSearchOptOutRequest	true	"Opt-out state"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	403		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/conversation-search-opt-out [patch]
func (h *DatapackHandler) SetConversationSearchOptOut(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	var req setConversationSearchOptOutRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.SetConversationSearchOptOut(account.ID, dp.ID, req.OptedOut); err != nil {
		return internalError(c, err, "update the datapack", "datapack_id", dp.ID)
	}

	return ok(c, http.StatusOK, map[string]bool{"conversation_search_opt_out": req.OptedOut})
}

// Update handles PATCH /api/v1/datapacks/:datapack_id.
//
//	@Summary	Update a datapack
//	@Tags		datapacks
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string					true	"Datapack ID"
//	@Param		body		body		service.UpdateRequest	true	"Update fields"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id} [patch]
func (h *DatapackHandler) Update(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	var req service.UpdateRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.svc.Update(account.ID, dp.ID, req); err != nil {
		if isDatapackValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isConflictErr(err) {
			return apiError(c, http.StatusConflict, "CONFLICT", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return internalError(c, err, "update the datapack", "datapack_id", dp.ID)
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "datapack updated",
	})
}

// Delete handles DELETE /api/v1/datapacks/:datapack_id.
//
//	@Summary	Delete a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id} [delete]
func (h *DatapackHandler) Delete(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	if err := h.svc.Delete(account.ID, dp.ID); err != nil {
		if isNotFoundErr(err) {
			// A clean message, not err.Error(): defense in depth, so no future
			// not-found error from a downstream call can leak its detail here.
			return apiError(c, http.StatusNotFound, "NOT_FOUND", "datapack not found")
		}
		// Defense in depth against the MEKO-812 class: the delete path talks to
		// Langfuse and the DB, and such errors can embed internal URLs/SQL. Today
		// the Langfuse cleanup is best-effort (logged, not returned — see
		// https://github.com/yugabyte/meko/pull/1665), so the error here is a repo/DB
		// error; log it and return a generic message rather than err.Error(), which
		// is also where the pre-1665 URL leak came from.
		return internalError(c, err, "delete the datapack", "datapack_id", dp.ID)
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "datapack deleted",
	})
}

// Status handles GET /api/v1/datapacks/:datapack_id/status.
//
//	@Summary	Get datapack status
//	@Tags		datapacks
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/status [get]
func (h *DatapackHandler) Status(c echo.Context) error {
	if _, err := auth.GetAccount(c); err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack not resolved by middleware")
	}
	// dp.UserID is the owner's id (populated by RequireDatapackAccess even on
	// the shared-access path); the status lookup is keyed by owner, so passing
	// the caller's id would 404 for shared maintainers.
	st, err := h.svc.Status(dp.UserID, dp.ID)
	if err != nil {
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return internalError(c, err, "get the datapack status", "datapack_id", dp.ID)
	}

	redactDatapackStatus(st)
	return ok(c, http.StatusOK, st)
}

// GenerateLangfuseProjectKeys handles
// POST /api/v1/datapacks/:datapack_id/langfuse/project-keys.
//
//	@Summary	Generate Langfuse project keys for a datapack
//	@Tags		datapacks
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/langfuse/project-keys [post]
func (h *DatapackHandler) GenerateLangfuseProjectKeys(c echo.Context) error {
	if h.langfuse == nil || !h.langfuse.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"SERVICE_UNAVAILABLE",
			"langfuse is not configured")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil || dp == nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack not resolved by middleware")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), service.LangfuseProvisionBudget)
	defer cancel()
	keys, err := h.langfuse.EnsureProjectForDatapack(ctx, account, dp.ID, dp.Name)
	if err != nil {
		slog.Error("langfuse project provisioning failed",
			"user_id", account.ID, "datapack_id", dp.ID, "error", err)
		// Same status and code as before; only the message is generic, since
		// the error can name internal Langfuse hosts.
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR",
			"Could not set up the datapack's Langfuse project. Please retry.")
	}

	return ok(c, http.StatusOK, keys)
}

func isDatapackValidationErr(err error) bool {
	return errors.Is(err, service.ErrInvalidDatapackName)
}

func isValidationErr(err error) bool {
	return strings.Contains(err.Error(), "is required")
}

func isNotFoundErr(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

func isConflictErr(err error) bool {
	return strings.Contains(err.Error(), "already exists")
}
