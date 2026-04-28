package service

import (
	"context"
	"fmt"
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
