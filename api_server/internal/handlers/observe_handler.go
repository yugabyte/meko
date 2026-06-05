package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

// Defaults for GET /datapacks/:name/conversations pagination.
const (
	conversationsDefaultLimit = 20
	conversationsMaxLimit     = 200

	// Per-conversation thread page sizing. The thread can be very
	// long (the largest prod conversations have 30-40 messages and
	// growing); the UI scrolls within the right pane and asks for
	// older turns by bumping messagesOffset.
	threadDefaultLimit = 100
	threadMaxLimit     = 500
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

	q := strings.TrimSpace(c.QueryParam("q"))
	projectID, conversations, err := h.langfuse.ListConversations(dp.ID, limit, offset, q)
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

	// has_more is the cheap boundary signal for the UI's
	// infinite-scroll: when len(rows) == limit there's *probably*
	// another page (the next request returns 0 rows if not). The
	// UI keeps the existing /conversations/count endpoint for the
	// totals chip; we don't compute it here to keep this query hot.
	hasMore := len(conversations) == limit

	// langfuse_project_id is included so the UI can build
	// per-conversation deep links to the embedded Langfuse iframe
	// (/observe/project/<pid>/sessions/<sid>) without a second
	// roundtrip. Empty when no Langfuse mapping exists for the
	// datapack yet.
	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":         dp.ID,
		"datapack_name":       dp.Name,
		"langfuse_project_id": projectID,
		"limit":               limit,
		"offset":              offset,
		"has_more":            hasMore,
		"conversations":       conversations,
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

// ConversationDetail handles
// GET /api/v1/datapacks/:name/conversations/:conversation_id.
// Returns the header (same shape as a list row) plus a paginated
// thread of user/assistant turns with reasoning and per-turn counters.
//
// Datapack access is enforced upstream by RequireDatapackAccess.
//
// Query params:
//
//	messages_limit  — turns per page (default 100, max 500)
//	messages_offset — turns to skip (default 0)
func (h *ObserveHandler) ConversationDetail(c echo.Context) error {
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
	conversationID := c.Param("conversation_id")
	if conversationID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "conversation_id is required")
	}

	msgsLimit := threadDefaultLimit
	if v := c.QueryParam("messages_limit"); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			msgsLimit = n
		}
	}
	if msgsLimit > threadMaxLimit {
		slog.Warn("thread messages_limit exceeds max; clamping",
			"requested", msgsLimit, "max", threadMaxLimit)
		msgsLimit = threadMaxLimit
	}
	msgsOffset := 0
	if v := c.QueryParam("messages_offset"); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n >= 0 {
			msgsOffset = n
		}
	}

	projectID, detail, err := h.langfuse.GetConversation(
		dp.ID, conversationID, msgsLimit, msgsOffset,
	)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrLangfuseDBNotConfigured):
			return apiError(c, http.StatusServiceUnavailable,
				"SERVICE_UNAVAILABLE",
				"langfuse database is not configured on the api server")
		case errors.Is(err, service.ErrConversationNotFound):
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "conversation not found")
		}
		slog.Error("conversation detail failed",
			"datapack_id", dp.ID,
			"conversation_id", conversationID,
			"error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to fetch conversation detail")
	}

	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":         dp.ID,
		"datapack_name":       dp.Name,
		"langfuse_project_id": projectID,
		"conversation":        detail,
	})
}

// MessageTrace handles
// GET /api/v1/datapacks/:name/conversations/:conversation_id/messages/:message_id/trace.
// Returns the windowed observation tree for one turn — the message
// span plus all tool / generation observations between this message's
// start_time and the next message's start_time.
//
// Datapack access is enforced upstream by RequireDatapackAccess.
func (h *ObserveHandler) MessageTrace(c echo.Context) error {
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
	conversationID := c.Param("conversation_id")
	if conversationID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "conversation_id is required")
	}
	messageID := c.Param("message_id")
	if messageID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "message_id is required")
	}

	projectID, spans, err := h.langfuse.GetMessageTrace(
		dp.ID, conversationID, messageID,
	)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrLangfuseDBNotConfigured):
			return apiError(c, http.StatusServiceUnavailable,
				"SERVICE_UNAVAILABLE",
				"langfuse database is not configured on the api server")
		case errors.Is(err, service.ErrConversationNotFound):
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "message trace not found")
		}
		slog.Error("message trace failed",
			"datapack_id", dp.ID,
			"conversation_id", conversationID,
			"message_id", messageID,
			"error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to fetch message trace")
	}

	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":         dp.ID,
		"datapack_name":       dp.Name,
		"langfuse_project_id": projectID,
		"conversation_id":     conversationID,
		"message_id":          messageID,
		"spans":               spans,
	})
}

// MessageDecisionTrace handles
// GET /api/v1/datapacks/:name/conversations/:conversation_id/messages/:message_id/decision-trace.
//
// Returns the slim per-turn payload the chat UI's Decision Trace card
// and AssistantMetaRow chips read — model, latency, tokens, llm
// calls, memories extracted, reasoning, plan, memory hits, kb hits,
// processing steps. Drops input/output text (already in the bubble),
// started_at, observation_id, and memory_ids (unused by the UI).
//
// Reuses GetConversation under the hood and projects one ThreadMessage
// row to the slim shape — no new SQL path. Live-chat callers can poll
// this cheaply while Langfuse + mem0 catch up.
//
// Datapack access is enforced upstream by RequireDatapackAccess.
// Returns 404 while the bookmark hasn't been ingested yet, so clients
// can poll with backoff.
func (h *ObserveHandler) MessageDecisionTrace(c echo.Context) error {
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
	conversationID := c.Param("conversation_id")
	if conversationID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "conversation_id is required")
	}
	messageID := c.Param("message_id")
	if messageID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "message_id is required")
	}

	projectID, dt, err := h.langfuse.GetMessageDecisionTrace(
		dp.ID, conversationID, messageID,
	)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrLangfuseDBNotConfigured):
			return apiError(c, http.StatusServiceUnavailable,
				"SERVICE_UNAVAILABLE",
				"langfuse database is not configured on the api server")
		case errors.Is(err, service.ErrConversationNotFound):
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "message not found")
		}
		slog.Error("message decision-trace lookup failed",
			"datapack_id", dp.ID,
			"conversation_id", conversationID,
			"message_id", messageID,
			"error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to fetch message decision trace")
	}

	return ok(c, http.StatusOK, map[string]any{
		"datapack_id":         dp.ID,
		"datapack_name":       dp.Name,
		"langfuse_project_id": projectID,
		"conversation_id":     conversationID,
		"decision_trace":      dt,
	})
}
