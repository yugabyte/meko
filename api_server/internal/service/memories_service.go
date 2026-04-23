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
	memoriesRepo *repository.MemoriesRepo
}

func NewMemoriesService(memoriesRepo *repository.MemoriesRepo) *MemoriesService {
	return &MemoriesService{memoriesRepo: memoriesRepo}
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
		"user_id", filter.UserID,
		"agent_id", filter.AgentID,
		"datapack_id", filter.DatapackID,
		"free_user", filter.FreeUser,
		"page", filter.Page,
		"page_size", filter.PageSize,
	)

	memories, total, err := svc.memoriesRepo.ListByFilter(ctx, filter)
	if err != nil {
		return models.MemoryListResponse{}, err
	}

	topK := 5
	if filter.FreeUser {
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
