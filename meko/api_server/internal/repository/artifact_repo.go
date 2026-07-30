package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"meko/api_server/internal/models"
)

// ArtifactRepo handles persistence for datapacks.artifacts.
type ArtifactRepo struct {
	db *sql.DB
}

// NewArtifactRepo creates a new ArtifactRepo backed by the system DB.
func NewArtifactRepo(db *sql.DB) *ArtifactRepo {
	return &ArtifactRepo{db: db}
}

// FindByHash returns the artifact for (datapackID, contentHash), or nil if
// none exists. Returns an error only for unexpected DB failures.
func (r *ArtifactRepo) FindByHash(ctx context.Context, datapackID, contentHash string) (*models.Artifact, error) {
	const q = `
		SELECT id, datapack_id, content_hash, filename, content_type,
		       size_bytes, inline_content, s3_key,
		       created_at, last_accessed_at, expired_at
		FROM   datapacks.artifacts
		WHERE  datapack_id = $1
		AND    content_hash = $2`

	row := r.db.QueryRowContext(ctx, q, datapackID, contentHash)
	a, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// Insert writes a new artifact row. Callers must check FindByHash first to
// honour idempotency — Insert does not upsert.
func (r *ArtifactRepo) Insert(ctx context.Context, a *models.Artifact) (*models.Artifact, error) {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	const q = `
		INSERT INTO datapacks.artifacts
		    (id, datapack_id, content_hash, filename, content_type,
		     size_bytes, inline_content, s3_key, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
		ON CONFLICT (datapack_id, content_hash) DO NOTHING
		RETURNING id, datapack_id, content_hash, filename, content_type,
		          size_bytes, inline_content, s3_key,
		          created_at, last_accessed_at, expired_at`

	row := r.db.QueryRowContext(ctx, q,
		a.ID, a.DatapackID, a.ContentHash, a.Filename, a.ContentType,
		a.SizeBytes, a.InlineContent, nilIfEmpty(a.S3Key),
	)
	inserted, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent Insert won the race; fetch the existing row.
		return r.FindByHash(ctx, a.DatapackID, a.ContentHash)
	}
	return inserted, err
}

// TouchLastAccessed sets last_accessed_at = now() for the given artifact.
func (r *ArtifactRepo) TouchLastAccessed(ctx context.Context, datapackID, contentHash string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE datapacks.artifacts
		    SET last_accessed_at = now()
		  WHERE datapack_id = $1 AND content_hash = $2`,
		datapackID, contentHash,
	)
	return err
}

// Reactivate clears expired_at and refreshes last_accessed_at for an artifact
// that was previously soft-expired, making it live again.
func (r *ArtifactRepo) Reactivate(ctx context.Context, datapackID, contentHash string) (*models.Artifact, error) {
	const q = `
		UPDATE datapacks.artifacts
		   SET expired_at       = NULL,
		       last_accessed_at = now()
		 WHERE datapack_id  = $1
		   AND content_hash = $2
		RETURNING id, datapack_id, content_hash, filename, content_type,
		          size_bytes, inline_content, s3_key,
		          created_at, last_accessed_at, expired_at`

	row := r.db.QueryRowContext(ctx, q, datapackID, contentHash)
	a, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// scanArtifact reads one row into a models.Artifact.
func scanArtifact(row *sql.Row) (*models.Artifact, error) {
	var a models.Artifact
	var inlineContent []byte
	var s3Key sql.NullString
	var lastAccessed, expiredAt sql.NullTime

	err := row.Scan(
		&a.ID, &a.DatapackID, &a.ContentHash, &a.Filename, &a.ContentType,
		&a.SizeBytes, &inlineContent, &s3Key,
		&a.CreatedAt, &lastAccessed, &expiredAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	a.InlineContent = inlineContent
	if s3Key.Valid {
		a.S3Key = s3Key.String
	}
	if lastAccessed.Valid {
		t := lastAccessed.Time
		a.LastAccessedAt = &t
	}
	if expiredAt.Valid {
		t := expiredAt.Time
		a.ExpiredAt = &t
	}
	return &a, nil
}

// nilIfEmpty returns nil when s is empty (maps to SQL NULL for s3_key).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ensure time is used (it's in the model via *time.Time pointers).
var _ = time.Time{}
