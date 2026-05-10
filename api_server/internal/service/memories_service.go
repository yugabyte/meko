package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

type MemoriesService struct {
	memoriesRepo   *repository.MemoriesRepo
	skipVectorSort bool
}

func NewMemoriesService(memoriesRepo *repository.MemoriesRepo, skipVectorSort bool) *MemoriesService {
	return &MemoriesService{memoriesRepo: memoriesRepo, skipVectorSort: skipVectorSort}
}

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

func (svc *MemoriesService) List(
	ctx context.Context,
	filter models.MemoryListFilter,
) (models.MemoryListResponse, error) {
	if strings.TrimSpace(filter.DatapackID) == "" {
		return models.MemoryListResponse{}, fmt.Errorf("datapack_id is required")
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

	slog.Info("listing memories",
		"layer", "service",
		"datapack_id", filter.DatapackID,
		"user_id", filter.UserID,
		"user_type", filter.UserType,
		"page", filter.Page,
		"page_size", filter.PageSize,
	)

	memories, total, err := svc.memoriesRepo.ListByFilter(ctx, filter)
	if err != nil {
		return models.MemoryListResponse{}, err
	}

	topK := 5
	if !filter.IsPaidUserType() || svc.skipVectorSort {
		topK = 0
	}

	totalPages := (total + filter.PageSize - 1) / filter.PageSize
	if totalPages < 1 {
		totalPages = 1
	}

	return models.MemoryListResponse{
		Criteria:   filter,
		Memories:   memories,
		TopK:       topK,
		Total:      total,
		Page:       filter.Page,
		PageSize:   filter.PageSize,
		TotalPages: totalPages,
	}, nil
}

// Count returns the total number of memory rows for a datapack. This is a
// straight COUNT(*) on datapacks.meko_mem0_collection scoped by
// meko_datapack_id with no additional filtering.
func (svc *MemoriesService) Count(
	ctx context.Context,
	datapackID string,
) (models.MemoryCountResponse, error) {
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return models.MemoryCountResponse{}, fmt.Errorf("datapack_id is required")
	}

	slog.Info("counting memories",
		"layer", "service",
		"datapack_id", datapackID,
	)

	count, err := svc.memoriesRepo.CountByDatapack(ctx, datapackID)
	if err != nil {
		return models.MemoryCountResponse{}, err
	}

	return models.MemoryCountResponse{
		DatapackID: datapackID,
		Count:      count,
	}, nil
}
