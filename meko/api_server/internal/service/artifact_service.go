package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ArtifactRepo is the minimal persistence interface used by ArtifactService.
// The concrete implementation is repository.ArtifactRepo; the interface exists
// so tests can substitute a stub without a real database.
type ArtifactRepo interface {
	FindByHash(ctx context.Context, datapackID, hash string) (*models.Artifact, error)
	Insert(ctx context.Context, a *models.Artifact) (*models.Artifact, error)
	TouchLastAccessed(ctx context.Context, datapackID, hash string) error
	Reactivate(ctx context.Context, datapackID, hash string) (*models.Artifact, error)
}

// ArtifactS3 is the S3 subset used by ArtifactService, extracted so tests can
// substitute a stub without a real S3 bucket.
type ArtifactS3 interface {
	Enabled() bool
	UploadObject(ctx context.Context, userID, datapackID, hash, filename, contentType string, data []byte) (string, error)
	DownloadObject(ctx context.Context, key string) ([]byte, error)
}

// ArtifactService handles business logic for artifact upload and retrieval.
type ArtifactService struct {
	repo  ArtifactRepo
	s3Svc ArtifactS3
}

// NewArtifactService creates a new ArtifactService backed by the concrete repo and S3 service.
func NewArtifactService(repo *repository.ArtifactRepo, s3Svc *S3Service) *ArtifactService {
	return &ArtifactService{repo: repo, s3Svc: s3Svc}
}

// NewArtifactServiceWithIface creates an ArtifactService from interface values (used in tests).
func NewArtifactServiceWithIface(repo ArtifactRepo, s3Svc ArtifactS3) *ArtifactService {
	return &ArtifactService{repo: repo, s3Svc: s3Svc}
}

// PutRequest carries the inputs for uploading an artifact.
type ArtifactPutRequest struct {
	UserID      string
	DatapackID  string
	Filename    string
	ContentType string
	Data        []byte // decoded file bytes
}

// ArtifactGetResponse is the retrieval result.
type ArtifactGetResponse struct {
	*models.Artifact
	// ContentBase64 is set for inline (db) artifacts.
	ContentBase64 string `json:"content_base64,omitempty"`
	// LocalPath is set for S3 artifacts after the caller writes the file to disk.
	// The service returns raw Content; the HTTP/MCP layer writes it and sets this.
	Content []byte `json:"-"`
}

// Put stores an artifact for the given datapack. Uploading the same bytes
// (same SHA-256) twice returns the existing row without re-writing.
func (s *ArtifactService) Put(ctx context.Context, req ArtifactPutRequest) (*models.Artifact, error) {
	if len(req.Data) == 0 {
		return nil, fmt.Errorf("artifact data must not be empty")
	}

	sum := sha256.Sum256(req.Data)
	contentHash := hex.EncodeToString(sum[:])

	// Idempotency: return existing artifact if the same bytes were already uploaded.
	existing, err := s.repo.FindByHash(ctx, req.DatapackID, contentHash)
	if err != nil {
		return nil, fmt.Errorf("check existing artifact: %w", err)
	}
	if existing != nil {
		if existing.ExpiredAt != nil {
			// Re-upload of a soft-expired artifact: reactivate it.
			reactivated, err := s.repo.Reactivate(ctx, req.DatapackID, contentHash)
			if err != nil {
				return nil, fmt.Errorf("reactivate artifact: %w", err)
			}
			return reactivated, nil
		}
		return existing, nil
	}

	a := &models.Artifact{
		DatapackID:  req.DatapackID,
		ContentHash: contentHash,
		Filename:    path.Base(strings.ReplaceAll(req.Filename, `\`, "/")),
		ContentType: req.ContentType,
		SizeBytes:   int64(len(req.Data)),
	}

	if int64(len(req.Data)) < models.MaxInlineBytes {
		a.InlineContent = req.Data
	} else {
		if s.s3Svc == nil || !s.s3Svc.Enabled() { //nolint:staticcheck
			return nil, fmt.Errorf("S3 not configured; cannot store artifact of size %d bytes", len(req.Data))
		}
		key, err := s.s3Svc.UploadObject(ctx, req.UserID, req.DatapackID, contentHash, req.Filename, req.ContentType, req.Data)
		if err != nil {
			return nil, fmt.Errorf("upload artifact to S3: %w", err)
		}
		a.S3Key = key
	}

	return s.repo.Insert(ctx, a)
}

// Get retrieves an artifact by (datapackID, contentHash). Returns the artifact
// plus the file bytes. Callers check Artifact.StoredIn() to decide how to
// surface the content.
func (s *ArtifactService) Get(ctx context.Context, datapackID, contentHash string) (*ArtifactGetResponse, error) {
	a, err := s.repo.FindByHash(ctx, datapackID, contentHash)
	if err != nil {
		return nil, fmt.Errorf("find artifact: %w", err)
	}
	if a == nil {
		return nil, nil // caller maps to 404
	}
	if a.ExpiredAt != nil {
		return nil, fmt.Errorf("artifact expired") // caller maps to 410
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.repo.TouchLastAccessed(ctx, datapackID, contentHash); err != nil {
			slog.Warn("failed to touch artifact last_accessed_at",
				"datapack_id", datapackID, "content_hash", contentHash, "error", err)
		}
	}()

	resp := &ArtifactGetResponse{Artifact: a}

	if len(a.InlineContent) > 0 {
		resp.ContentBase64 = base64.StdEncoding.EncodeToString(a.InlineContent)
		return resp, nil
	}

	// Large file: fetch from S3 and return raw bytes; HTTP/MCP layer writes to disk.
	if s.s3Svc == nil || !s.s3Svc.Enabled() {
		return nil, fmt.Errorf("S3 not configured; cannot retrieve artifact stored in S3")
	}
	data, err := s.s3Svc.DownloadObject(ctx, a.S3Key)
	if err != nil {
		return nil, fmt.Errorf("download artifact from S3: %w", err)
	}
	resp.Content = data
	return resp, nil
}
