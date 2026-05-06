package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

type PromoteMemoriesService struct {
	repo *repository.PromoteMemoriesRepo
}

func NewPromoteMemoriesService(repo *repository.PromoteMemoriesRepo) *PromoteMemoriesService {
	return &PromoteMemoriesService{repo: repo}
}

func (svc *PromoteMemoriesService) Promote(
	ctx context.Context,
	req models.PromoteMemoriesRequest,
) (models.PromoteMemoriesResult, error) {
	if strings.TrimSpace(req.DatapackID) == "" {
		return models.PromoteMemoriesResult{}, fmt.Errorf("datapack_id is required")
	}
	if len(req.MemoryIDs) == 0 {
		return models.PromoteMemoriesResult{}, fmt.Errorf("memory_ids is required")
	}

	insertedIDs, updatedIDs, notFoundIDs, err := svc.repo.Promote(ctx, req.UserID, req.AgentID, req.DatapackID, req.MemoryIDs)
	if err != nil {
		return models.PromoteMemoriesResult{}, err
	}

	if insertedIDs == nil {
		insertedIDs = []string{}
	}
	if updatedIDs == nil {
		updatedIDs = []string{}
	}
	if notFoundIDs == nil {
		notFoundIDs = []string{}
	}

	return models.PromoteMemoriesResult{
		InsertedIDs: insertedIDs,
		UpdatedIDs:  updatedIDs,
		NotFoundIDs: notFoundIDs,
	}, nil
}

// List returns paginated promoted memories from datapacks.promoted_collection.
func (svc *PromoteMemoriesService) List(
	ctx context.Context,
	filter models.PromotedMemoryListFilter,
) (models.PromotedMemoryListResponse, error) {
	if strings.TrimSpace(filter.DatapackID) == "" {
		return models.PromotedMemoryListResponse{}, fmt.Errorf("datapack_id is required")
	}

	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = defaultPageSize
	}
	if filter.PageSize > maxPageSize {
		filter.PageSize = maxPageSize
	}

	slog.Info("listing promoted memories",
		"layer", "service",
		"datapack_id", filter.DatapackID,
		"page", filter.Page,
		"page_size", filter.PageSize,
	)

	items, total, err := svc.repo.ListPromoted(ctx, filter.DatapackID, filter.Page, filter.PageSize)
	if err != nil {
		return models.PromotedMemoryListResponse{}, err
	}

	totalPages := (total + filter.PageSize - 1) / filter.PageSize
	if totalPages < 1 {
		totalPages = 1
	}

	if items == nil {
		items = []models.PromotedMemory{}
	}

	return models.PromotedMemoryListResponse{
		Criteria:         filter,
		PromotedMemories: items,
		Total:            total,
		Page:             filter.Page,
		PageSize:         filter.PageSize,
		TotalPages:       totalPages,
	}, nil
}
