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

// ArtifactListOpts controls pagination + filename filtering for
// ListByDatapack. Zero-values are safe: Limit ≤ 0 falls back to the
// caller-supplied default, Offset ≤ 0 starts from the top, and an
// empty SearchQuery disables filename filtering.
type ArtifactListOpts struct {
	Limit       int
	Offset      int
	SearchQuery string // filename ILIKE %<q>%
}

// ListByDatapackResult mirrors the KB documents-list envelope so the
// UI has one shape to render everywhere paginated tables live.
type ListByDatapackResult struct {
	Items   []*models.Artifact
	Total   int
	HasMore bool
}

// ListByDatapack returns a paginated slice of artifacts for the datapack,
// newest first, honouring the SearchQuery (case-insensitive filename
// substring). Expired rows are excluded — the UI has no "resurrect"
// affordance today, so surfacing them would be misleading.
//
// One extra COUNT(*) query runs alongside the SELECT so the caller can
// render "total" without over-fetching. On very large datapacks the
// count could get expensive; keep an eye on it and swap to a keyset
// paginator if it becomes a hot path.
func (r *ArtifactRepo) ListByDatapack(
	ctx context.Context, datapackID string, opts ArtifactListOpts,
) (*ListByDatapackResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 25
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	q := opts.SearchQuery

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM datapacks.artifacts
		  WHERE datapack_id = $1
		    AND expired_at IS NULL
		    AND ($2 = '' OR filename ILIKE '%' || $2 || '%')`,
		datapackID, q,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("count artifacts: %w", err)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, datapack_id, content_hash, filename, content_type,
		        size_bytes, inline_content, s3_key,
		        created_at, last_accessed_at, expired_at
		   FROM datapacks.artifacts
		  WHERE datapack_id = $1
		    AND expired_at IS NULL
		    AND ($2 = '' OR filename ILIKE '%' || $2 || '%')
		  ORDER BY created_at DESC, id DESC
		  LIMIT $3 OFFSET $4`,
		datapackID, q, opts.Limit, opts.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	items := make([]*models.Artifact, 0, opts.Limit)
	for rows.Next() {
		var a models.Artifact
		var inlineContent []byte
		var s3Key sql.NullString
		var lastAccessed, expiredAt sql.NullTime
		if err := rows.Scan(
			&a.ID, &a.DatapackID, &a.ContentHash, &a.Filename, &a.ContentType,
			&a.SizeBytes, &inlineContent, &s3Key,
			&a.CreatedAt, &lastAccessed, &expiredAt,
		); err != nil {
			return nil, fmt.Errorf("scan artifact row: %w", err)
		}
		// Deliberately drop inline_content from the list result — this
		// endpoint is a manifest, not a bulk download. Callers use
		// GET .../artifacts/:hash to fetch bytes.
		a.InlineContent = nil
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
		items = append(items, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}

	return &ListByDatapackResult{
		Items:   items,
		Total:   total,
		HasMore: opts.Offset+len(items) < total,
	}, nil
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
