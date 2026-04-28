package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// Defaults for GET /datapacks/:name/conversations pagination.
const (
	conversationsDefaultLimit = 20
	conversationsMaxLimit     = 200
)

// ObserveHandler handles lightweight Observe bootstrap.
type ObserveHandler struct {
	langfuse *service.LangfuseService
}

func NewObserveHandler(langfuse *service.LangfuseService) *ObserveHandler {
	return &ObserveHandler{langfuse: langfuse}
}

// BootstrapSession handles POST /api/v1/observe/session/bootstrap.
// It establishes a Langfuse web session by forwarding upstream Set-Cookie
// headers from CSRF + credentials login flow back to the browser.
func (h *ObserveHandler) BootstrapSession(c echo.Context) error {
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

	result, err := h.langfuse.BootstrapObserveSession(
		account.ID,
		account.Email,
		account.FirstName,
		account.LastName,
	)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	for _, rawCookie := range result.SetCookies {
		c.Response().Header().Add(echo.HeaderSetCookie, rawCookie)
	}
	return ok(c, http.StatusOK, map[string]string{
		"message": "observe bootstrap ready",
		"org_id":  result.OrgID,
	})
}

// ConversationList handles GET /api/v1/datapacks/:name/conversations.
// Returns one page of Langfuse sessions (= conversations) for the
// datapack's project, newest first, with offset pagination. Datapack
// ownership is enforced upstream by RequireDatapackOwnership.
//
// Query params:
//
//	limit  — page size (default 20, max 200)
//	offset — rows to skip (default 0)
func (h *ObserveHandler) ConversationList(c echo.Context) error {
	if h.langfuse == nil {
		return apiError(c, http.StatusServiceUnavailable,
			"SERVICE_UNAVAILABLE",
			"langfuse service is not configured")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	limit := conversationsDefaultLimit
	if v := c.QueryParam("limit"); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			limit = n
		}
	}
	if limit > conversationsMaxLimit {
		slog.Warn("conversation limit exceeds max; clamping",
			"requested", limit, "max", conversationsMaxLimit)
		limit = conversationsMaxLimit
	}
	offset := 0
	if v := c.QueryParam("offset"); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n >= 0 {
			offset = n
		}
	}

	conversations, err := h.langfuse.ListConversations(dp.ID, limit, offset)
	if err != nil {
		if errors.Is(err, service.ErrLangfuseDBNotConfigured) {
			return apiError(c, http.StatusServiceUnavailable,
				"SERVICE_UNAVAILABLE",
				"langfuse database is not configured on the api server")
		}
		slog.Error("conversation list failed",
			"datapack_id", dp.ID,
			"limit", limit,
			"offset", offset,
			"error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to fetch conversations")
	}

	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":   dp.ID,
		"datapack_name": dp.Name,
		"limit":         limit,
		"offset":        offset,
		"conversations": conversations,
	})
}

// ConversationCount handles GET /api/v1/datapacks/:name/conversations/count.
// Returns the total number of distinct Langfuse sessions (= conversations)
// for the datapack's project. Ownership is enforced upstream by
// RequireDatapackOwnership.
func (h *ObserveHandler) ConversationCount(c echo.Context) error {
	if h.langfuse == nil {
		return apiError(c, http.StatusServiceUnavailable,
			"SERVICE_UNAVAILABLE",
			"langfuse service is not configured")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	n, err := h.langfuse.CountConversations(dp.ID)
	if err != nil {
		if errors.Is(err, service.ErrLangfuseDBNotConfigured) {
			return apiError(c, http.StatusServiceUnavailable,
				"SERVICE_UNAVAILABLE",
				"langfuse database is not configured on the api server")
		}
		slog.Error("conversation count failed",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to fetch conversation count")
	}

	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":   dp.ID,
		"datapack_name": dp.Name,
		"count":         n,
	})
}
