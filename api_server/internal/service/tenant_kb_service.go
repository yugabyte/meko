package service

import (
	"context"
	"fmt"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

type TenantKBService struct {
	repo *repository.TenantKBRepo
}

func NewTenantKBService(repo *repository.TenantKBRepo) *TenantKBService {
	return &TenantKBService{repo: repo}
}

func (svc *TenantKBService) Sync(
	ctx context.Context,
	req models.TenantKBSyncRequest,
) (models.TenantKBSyncResult, error) {
	if strings.TrimSpace(req.DatapackID) == "" {
		return models.TenantKBSyncResult{}, fmt.Errorf("datapack_id is required")
	}
	if len(req.MemoryIDs) == 0 {
		return models.TenantKBSyncResult{}, fmt.Errorf("memory_ids is required")
	}

	insertedIDs, updatedIDs, notFoundIDs, err := svc.repo.SyncMemories(ctx, req.UserID, req.AgentID, req.DatapackID, req.MemoryIDs)
	if err != nil {
		return models.TenantKBSyncResult{}, err
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

	return models.TenantKBSyncResult{
		InsertedIDs: insertedIDs,
		UpdatedIDs:  updatedIDs,
		NotFoundIDs: notFoundIDs,
	}, nil
}
