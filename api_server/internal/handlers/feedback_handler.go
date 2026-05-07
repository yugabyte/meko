package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// FeedbackHandler handles POST /api/v1/feedback.
type FeedbackHandler struct {
	repo *repository.FeedbackRepo
}

// NewFeedbackHandler constructs a FeedbackHandler.
func NewFeedbackHandler(repo *repository.FeedbackRepo) *FeedbackHandler {
	return &FeedbackHandler{repo: repo}
}

// Create handles POST /api/v1/feedback.
//
//	@Summary	Submit product feedback
//	@Tags		feedback
//	@Accept		json
//	@Produce	json
//	@Param		body	body		models.CreateFeedbackRequest	true	"Feedback payload"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/feedback [post]
func (h *FeedbackHandler) Create(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	}

	var req models.CreateFeedbackRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}

	if err := validateCreateFeedback(&req, account); err != nil {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	}

	params := repository.CreateFeedbackParams{
		CreatedAt:        req.CreatedAt,
		UserID:           account.ID,
		Source:           strings.TrimSpace(req.Source),
		SourceIdentifier: req.SourceIdentifier,
		DatapackID:       req.DatapackID,
		DatapackName:     req.DatapackName,
		ContextRoute:     req.UserPath,
		Category:         strings.TrimSpace(req.Category),
		Body:             strings.TrimSpace(req.FeedbackText),
		MekoVersion:      req.MekoVersion,
	}

	sidEmpty := req.SourceIdentifier == nil || strings.TrimSpace(*req.SourceIdentifier) == ""
	if sidEmpty && strings.TrimSpace(account.Email) != "" {
		email := strings.TrimSpace(account.Email)
		params.SourceIdentifier = &email
	}

	id, err := h.repo.Create(params)
	if err != nil {
		if strings.Contains(err.Error(), "invalid input value for enum") {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid category or source value")
		}
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	resp := models.CreateFeedbackResponse{
		FeedbackID: strconv.FormatInt(id, 10),
		Status:     "received",
		Message:    "Thanks for your feedback.",
	}
	return ok(c, http.StatusOK, resp)
}

// ListGET handles GET /api/v1/feedback.
// Query params: optional user_id (UUID) and optional status (feedback_status enum).
// At least one parameter must be set. When both are set, filters use AND semantics.
//
//	@Summary	List feedback rows
//	@Tags		feedback
//	@Produce	json
//	@Param		user_id	query	string	false	"Filter by submitting user UUID"
//	@Param		status	query	string	false	"Filter by status: new, triaged, in_progress, resolved, wontfix"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/feedback [get]
func (h *FeedbackHandler) ListGET(c echo.Context) error {
	if _, err := auth.GetAccount(c); err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	}

	userQP := strings.TrimSpace(c.QueryParam("user_id"))
	statusQP := strings.TrimSpace(c.QueryParam("status"))
	if userQP == "" && statusQP == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"at least one of user_id or status query parameters is required")
	}

	var userPtr *string
	if userQP != "" {
		if _, err := uuid.Parse(userQP); err != nil {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "user_id must be a valid UUID")
		}
		userPtr = &userQP
	}

	var statusPtr *string
	if statusQP != "" {
		if !models.ValidFeedbackStatus(statusQP) {
			return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR",
				"status must be one of: new, triaged, in_progress, resolved, wontfix")
		}
		statusPtr = &statusQP
	}

	items, err := h.repo.ListFeedback(c.Request().Context(), userPtr, statusPtr)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]any{
		"feedback": items,
	})
}

func validateCreateFeedback(req *models.CreateFeedbackRequest, account *models.Account) error {
	if strings.TrimSpace(req.FeedbackText) == "" {
		return fmt.Errorf("feedback_text is required")
	}
	if strings.TrimSpace(req.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if !models.ValidFeedbackSource(strings.TrimSpace(req.Source)) {
		return fmt.Errorf("source must be one of: web_app, docs, discord, email, cli, mcp_server")
	}
	if strings.TrimSpace(req.Category) == "" {
		return fmt.Errorf("category is required")
	}
	if !models.ValidFeedbackCategory(strings.TrimSpace(req.Category)) {
		return fmt.Errorf("category must be one of: bug, confusion, feature_request, other")
	}

	if strings.TrimSpace(req.UserID) == "" {
		return fmt.Errorf("user_id is required")
	}
	if strings.TrimSpace(req.UserID) != account.ID {
		return fmt.Errorf("user_id must match the authenticated account")
	}

	if req.DatapackID != nil && strings.TrimSpace(*req.DatapackID) != "" {
		if _, err := uuid.Parse(strings.TrimSpace(*req.DatapackID)); err != nil {
			return fmt.Errorf("datapack_id must be a valid UUID when provided")
		}
	}

	return nil
}
