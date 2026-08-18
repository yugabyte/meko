// Package admin: admin_feedback_service — thin wrapper around
// AdminRepo.ListFeedbackByDateRange. Kept as its own file so future
// feedback-workflow methods (status transitions, per-user drill-in)
// land here rather than swelling admin_metrics_service.
package admin

import (
	"context"
	"time"

	adminrepo "meko/api_server/internal/repository/admin"
)

// AdminFeedbackService exposes admin feedback reads. Owns nothing
// beyond an AdminRepo handle; state lives in the DB and the concrete
// filtering/paging shape lives on the handler.
type AdminFeedbackService struct {
	repo *adminrepo.AdminRepo
}

// NewAdminFeedbackService constructs the service.
func NewAdminFeedbackService(repo *adminrepo.AdminRepo) *AdminFeedbackService {
	return &AdminFeedbackService{repo: repo}
}

// AdminFeedbackItem is the outward-facing shape of one feedback row —
// what the handler serialises to JSON. Times are RFC3339 through the
// stdlib default; nullable columns come across as *string / *time so
// the JSON omits them cleanly when absent.
type AdminFeedbackItem struct {
	ID               string     `json:"id"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	UserID           *string    `json:"user_id,omitempty"`
	Source           string     `json:"source"`
	SourceIdentifier *string    `json:"source_identifier,omitempty"`
	DatapackID       *string    `json:"datapack_id,omitempty"`
	DatapackName     *string    `json:"datapack_name,omitempty"`
	ContextRoute     *string    `json:"context_route,omitempty"`
	Category         string     `json:"category"`
	Body             string     `json:"body"`
	MekoVersion      *string    `json:"meko_version,omitempty"`
	Status           string     `json:"status"`
}

// AdminFeedbackPagination echoes the caller's paging shape plus the
// total match count. Total drives the "Showing X-Y of N" strip and
// last-page detection on the FE.
type AdminFeedbackPagination struct {
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// AdminFeedbackResponse is the wrapping payload for GET
// /api/v1/admin/feedback.
type AdminFeedbackResponse struct {
	Feedback   []AdminFeedbackItem     `json:"feedback"`
	Pagination AdminFeedbackPagination `json:"pagination"`
}

// List returns paged feedback filtered by created_at ∈ [start, endExclusive).
// Bounds are optional; nil ⇒ unbounded on that side. The handler passes
// caller-clamped limit / offset (1..100 / >=0).
func (s *AdminFeedbackService) List(
	ctx context.Context,
	start, endExclusive *time.Time,
	limit, offset int,
) (AdminFeedbackResponse, error) {
	page, err := s.repo.ListFeedbackByDateRange(ctx, start, endExclusive, limit, offset)
	if err != nil {
		return AdminFeedbackResponse{}, err
	}
	items := make([]AdminFeedbackItem, len(page.Items))
	for i, r := range page.Items {
		items[i] = AdminFeedbackItem{
			ID:               r.ID,
			CreatedAt:        r.CreatedAt,
			UpdatedAt:        r.UpdatedAt,
			UserID:           r.UserID,
			Source:           r.Source,
			SourceIdentifier: r.SourceIdentifier,
			DatapackID:       r.DatapackID,
			DatapackName:     r.DatapackName,
			ContextRoute:     r.ContextRoute,
			Category:         r.Category,
			Body:             r.Body,
			MekoVersion:      r.MekoVersion,
			Status:           r.Status,
		}
	}
	return AdminFeedbackResponse{
		Feedback: items,
		Pagination: AdminFeedbackPagination{
			Total:  page.Total,
			Limit:  limit,
			Offset: offset,
		},
	}, nil
}
