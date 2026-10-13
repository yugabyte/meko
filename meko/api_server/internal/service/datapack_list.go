package service

import (
	"context"
	"errors"
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ErrInvalidDatapackCursor is returned by ListPage for a cursor that is
// malformed or was issued for a different sort or filter set.
var ErrInvalidDatapackCursor = repository.ErrInvalidDatapackCursor

// DatapackListRequest is one page request for GET /datapacks?limit=... The
// handler has already validated every field.
type DatapackListRequest struct {
	Cursor       string
	Sort         string
	Q            string
	Name         string
	Grant        string
	Limit        int
	IncludeTotal bool
}

// DatapackListPage is one page of datapacks plus the cursor for the next.
type DatapackListPage struct {
	Total      *int64
	NextCursor string
	Items      []models.Datapack
	Limit      int
	HasMore    bool
}

// DatapackCounts is the body of GET /datapacks/count.
type DatapackCounts struct {
	Owned  int64 `json:"owned"`
	Shared int64 `json:"shared"`
	Total  int64 `json:"total"`
}

// ListPage returns one page of the datapacks userID can see.
func (s *DatapackService) ListPage(
	ctx context.Context, userID string, req DatapackListRequest,
) (*DatapackListPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	filterHash := repository.DatapackFilterHash(req.Q, req.Name, req.Grant)
	opts := repository.DatapackListOpts{
		Limit:        req.Limit,
		Sort:         req.Sort,
		Q:            req.Q,
		Name:         req.Name,
		Grant:        req.Grant,
		IncludeTotal: req.IncludeTotal,
	}
	if req.Cursor != "" {
		key, err := repository.DecodeDatapackCursor(req.Cursor, req.Sort, filterHash)
		if err != nil {
			return nil, err
		}
		opts.After = key
	}
	page, err := s.repo.ListPage(ctx, userID, opts)
	if err != nil {
		return nil, err
	}
	out := &DatapackListPage{
		Items:   page.Items,
		HasMore: page.HasMore,
		Limit:   req.Limit,
		Total:   page.Total,
	}
	if page.Next != nil {
		out.NextCursor = repository.EncodeDatapackCursor(req.Sort, filterHash, *page.Next)
	}
	return out, nil
}

// StreamAll calls fn for every datapack userID can see, newest first.
func (s *DatapackService) StreamAll(ctx context.Context, userID string, fn func(models.Datapack) error) error {
	if userID == "" {
		return errors.New("user_id is required")
	}
	return s.repo.StreamAll(ctx, userID, fn)
}

// Counts returns how many datapacks userID owns and how many are shared with them.
func (s *DatapackService) Counts(ctx context.Context, userID string) (*DatapackCounts, error) {
	if userID == "" {
		return nil, errors.New("user_id is required")
	}
	owned, shared, err := s.repo.Counts(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &DatapackCounts{Owned: owned, Shared: shared, Total: owned + shared}, nil
}
