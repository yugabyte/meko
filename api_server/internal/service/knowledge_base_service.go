package service

import (
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// KnowledgeBaseService provides business logic for knowledge base operations.
type KnowledgeBaseService struct {
	repo *repository.KnowledgeBaseRepo
}

// NewKnowledgeBaseService creates a new KnowledgeBaseService.
func NewKnowledgeBaseService(
	repo *repository.KnowledgeBaseRepo,
) *KnowledgeBaseService {
	return &KnowledgeBaseService{repo: repo}
}

// AddRequest represents the payload for adding a knowledge base.
type AddRequest struct {
	URL string `json:"url"`
}

// Add validates and adds a knowledge base to a datapack.
func (s *KnowledgeBaseService) Add(
	datapackName string, req AddRequest,
) (string, error) {
	if req.URL == "" {
		return "", fmt.Errorf("url is required")
	}
	return s.repo.Add(datapackName, req.URL)
}

// List returns all knowledge bases for a datapack.
func (s *KnowledgeBaseService) List(
	datapackName string,
) ([]models.KnowledgeBase, error) {
	return s.repo.ListByDatapack(datapackName)
}

// DeleteRequest represents the payload for deleting a knowledge base.
type DeleteRequest struct {
	URL string `json:"url"`
}

// Delete validates and removes a knowledge base from a datapack.
func (s *KnowledgeBaseService) Delete(
	datapackName string, req DeleteRequest,
) error {
	if req.URL == "" {
		return fmt.Errorf("url is required")
	}
	return s.repo.Delete(datapackName, req.URL)
}
