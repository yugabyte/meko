package service

import (
	"meko/api_server/internal/repository"
)

// YBAService manages YBA universe assignment for datapacks.
type YBAService struct {
	repo *repository.YBARepo
}

// NewYBAService creates a new YBAService.
func NewYBAService(repo *repository.YBARepo) *YBAService {
	return &YBAService{repo: repo}
}

// AssignUniverse selects an available universe with fewer than 100 datapacks.
func (s *YBAService) AssignUniverse() (*repository.UniverseAssignment, error) {
	return s.repo.AssignUniverse()
}

// GetAdminConnStr returns the admin connection string for a universe.
func (s *YBAService) GetAdminConnStr(universeID string) (string, error) {
	return s.repo.GetAdminConnStr(universeID)
}

// ReleaseUniverse decrements the datapack counter for a universe.
func (s *YBAService) ReleaseUniverse(universeID string) error {
	return s.repo.ReleaseUniverse(universeID)
}
