package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ExtensionCatalogReport is the body of PUT /agent/extensions. A pointer, so a body
// that omits the field or sends null is told so rather than stored as empty.
type ExtensionCatalogReport struct {
	Extensions *[]models.EnvironmentExtension `json:"extensions"`
}

// ExtensionCatalog is GET /environments/:id/extensions. Extensions is nil — null on
// the wire — when no catalog has ever been relayed, which a console must read as
// "unknown, fall back" and never as "this environment has no extensions".
type ExtensionCatalog struct {
	ReportedAt *time.Time                    `json:"reported_at"`
	Extensions []models.EnvironmentExtension `json:"extensions"`
}

// SetExtensionRepo wires the catalog store. Unset, GET answers unknown and the
// agent's PUT fails.
func (s *EnvironmentService) SetExtensionRepo(r *repository.EnvironmentExtensionRepo) {
	s.extRepo = r
}

// ApplyExtensions replaces an environment's relayed extension catalog. The caller
// resolves the environment first. Only an amp environment has a catalog; refusing
// the rest keeps "meko reads null" a guarantee rather than an accident of what
// today's meko agent happens not to send.
func (s *EnvironmentService) ApplyExtensions(envUUID, kind string, r ExtensionCatalogReport) error {
	if kind != KindAmp {
		return fmt.Errorf("%w: only an amp environment has an extension catalog", ErrValidation)
	}
	if r.Extensions == nil {
		return fmt.Errorf("%w: extensions is required; send [] for an empty catalog", ErrValidation)
	}
	for i, e := range *r.Extensions {
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("%w: extensions[%d].name is required", ErrValidation, i)
		}
	}
	if s.extRepo == nil {
		return errors.New("extension catalog store not configured")
	}
	return s.extRepo.PutCatalog(envUUID, *r.Extensions)
}

// Extensions returns an environment's relayed catalog. The caller resolves the
// environment, and with it the account check, first.
func (s *EnvironmentService) Extensions(envUUID string) (ExtensionCatalog, error) {
	if s.extRepo == nil {
		return ExtensionCatalog{}, nil
	}
	exts, at, err := s.extRepo.GetCatalog(envUUID)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	return ExtensionCatalog{Extensions: exts, ReportedAt: at}, nil
}
