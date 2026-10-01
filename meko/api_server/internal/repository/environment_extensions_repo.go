package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"meko/api_server/internal/models"
)

// EnvironmentExtensionRepo persists the extension catalog each amp environment's
// agent relays (meko_system.environment_extensions). Control-plane mode only.
//
// Keyed by env_uuid, and like EnvironmentTaskRepo it never resolves an environment:
// the caller has done that, and with it the account check.
type EnvironmentExtensionRepo struct {
	db *sql.DB
}

// NewEnvironmentExtensionRepo constructs the repo over the system DB.
func NewEnvironmentExtensionRepo(db *sql.DB) *EnvironmentExtensionRepo {
	return &EnvironmentExtensionRepo{db: db}
}

// PutCatalog replaces the environment's catalog. Callers must not pass nil — the
// absence of a row is what "never reported" means.
func (r *EnvironmentExtensionRepo) PutCatalog(envUUID string, exts []models.EnvironmentExtension) error {
	if exts == nil {
		return errors.New("extension catalog: nil is not a catalog")
	}
	raw, err := json.Marshal(exts)
	if err != nil {
		return fmt.Errorf("encode extension catalog: %w", err)
	}
	_, err = r.db.Exec(`
		INSERT INTO meko_system.environment_extensions (env_uuid, catalog, reported_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (env_uuid) DO UPDATE
		   SET catalog = EXCLUDED.catalog, reported_at = EXCLUDED.reported_at`,
		envUUID, raw)
	if err != nil {
		return fmt.Errorf("upsert extension catalog: %w", err)
	}
	return nil
}

// GetCatalog returns the environment's catalog and when it was relayed, or nil and
// a nil time when none ever was.
func (r *EnvironmentExtensionRepo) GetCatalog(envUUID string) ([]models.EnvironmentExtension, *time.Time, error) {
	var raw []byte
	var at time.Time
	err := r.db.QueryRow(`
		SELECT catalog, reported_at FROM meko_system.environment_extensions
		 WHERE env_uuid = $1`, envUUID).Scan(&raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get extension catalog: %w", err)
	}
	exts := []models.EnvironmentExtension{}
	if err := json.Unmarshal(raw, &exts); err != nil {
		return nil, nil, fmt.Errorf("decode extension catalog: %w", err)
	}
	return exts, &at, nil
}
