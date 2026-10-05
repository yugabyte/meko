package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ExtensionCatalogReport is the body of POST /agent/extensions. A pointer, so a body
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
// agent's POST fails.
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

// KindExtension is the desired-state kind an amp agent converges into its
// controller's extension catalog (registerExtension / deregisterExtension).
const KindExtension = "extension"

// ExtensionImport is the body of PUT /environments/:id/extensions/:name: the amp
// controller's registerExtension body, which is what the agent sends verbatim. Typed
// rather than passed through so that, with strict binding, a misspelled field is a
// 400 here and not a registration that silently lacks it.
type ExtensionImport struct {
	Version        *string                `json:"version,omitempty"`
	Description    *string                `json:"description,omitempty"`
	Library        *string                `json:"library,omitempty"`
	Preload        *bool                  `json:"preload,omitempty"`
	Layout         *ExtensionImportLayout `json:"layout,omitempty"`
	AbiExtraExempt *[]string              `json:"abi_extra_exempt,omitempty"`
	// Image is pinned to a tag or digest; the controller refuses :latest.
	Image string `json:"image"`
}

// ExtensionImportLayout is where the image keeps its pieces, relative to its mount.
type ExtensionImportLayout struct {
	SharePath   *string            `json:"share_path,omitempty"`
	LibPath     *string            `json:"lib_path,omitempty"`
	LibraryPath *[]string          `json:"library_path,omitempty"`
	EnvPaths    *map[string]string `json:"env_paths,omitempty"`
}

// ImportExtension declares an extension in an amp environment's desired state; its
// agent registers it in the controller's catalog on the next tick. A full replace,
// like the controller's verb: re-importing the same body keeps the generation, a
// changed one bumps it. found=false ⇒ unknown environment, or one the scope does not
// own.
//
// Only the name and image are checked here, including that neither carries
// whitespace the controller would trim. Everything else — the name's shape, the
// image reference, the layout paths — is the controller's rule, enforced on
// registration and reported back on the extension resource as rejected; a copy here
// would drift from it.
func (s *EnvironmentService) ImportExtension(
	envID string, scope AccountScope, name string, spec ExtensionImport,
) (*models.DesiredResource, bool, error) {
	e, err := s.Get(envID, scope)
	if err != nil || e == nil {
		return nil, false, err
	}
	if err = checkExtensionTarget(e.Kind, name); err != nil {
		return nil, true, err
	}
	if strings.TrimSpace(spec.Image) == "" {
		return nil, true, fmt.Errorf("%w: image is required", ErrValidation)
	}
	if strings.TrimSpace(spec.Image) != spec.Image {
		return nil, true, fmt.Errorf("%w: image must not carry surrounding whitespace", ErrValidation)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, true, fmt.Errorf("encode extension spec: %w", err)
	}
	d, err := s.repo.UpsertDesired(e.UUID, models.DesiredResource{Kind: KindExtension, Name: name, Spec: raw})
	if err != nil {
		return nil, true, err
	}
	return &d, true, nil
}

// RemoveExtension withdraws an extension's declaration; the agent deregisters it on
// the next tick, unless a branch still enables it. Idempotent: withdrawing one that
// was never declared is not an error.
func (s *EnvironmentService) RemoveExtension(envID string, scope AccountScope, name string) (bool, error) {
	e, err := s.Get(envID, scope)
	if err != nil || e == nil {
		return false, err
	}
	if err = checkExtensionTarget(e.Kind, name); err != nil {
		return true, err
	}
	return true, s.repo.DeleteDesired(e.UUID, KindExtension, name)
}

func checkExtensionTarget(envKind, name string) error {
	if envKind != KindAmp {
		return fmt.Errorf("%w: only an amp environment has an extension catalog", ErrValidation)
	}
	// The controller trims a name and registers the trimmed one, which the agent
	// would then collect as undeclared, so a padded name never converges.
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name || strings.Contains(name, "/") {
		return fmt.Errorf("%w: %q is not an extension name", ErrValidation, name)
	}
	return nil
}
