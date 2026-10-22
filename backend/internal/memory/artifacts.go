package memory

import (
	"context"
	"errors"

	"github.com/yugabyte/meko/backend/internal/store"
)

// MaxArtifactBytes is the largest file artifact_put keeps, as Meko Cloud's limit.
const MaxArtifactBytes = 5 << 20

// ErrTooLarge is a file over MaxArtifactBytes.
var ErrTooLarge = errors.New("the file is larger than 5 MiB")

// PutArtifact keeps a file in a datapack, once per content; the same bytes again return the file
// already kept.
func (s *Service) PutArtifact(ctx context.Context, datapack, agentID, filename, contentType string, b []byte) (
	store.Artifact, error) {
	if len(b) > MaxArtifactBytes {
		return store.Artifact{}, ErrTooLarge
	}
	a, _, err := s.st.PutArtifact(ctx, datapack, agentID, filename, contentType, b)
	if errors.Is(err, store.ErrNoDatapack) {
		return a, ErrNoDatapack
	}
	return a, err
}

// GetArtifact is a file of the datapack with its bytes, by its SHA-256, or ErrNotFound.
func (s *Service) GetArtifact(ctx context.Context, datapack, hash string) (store.Artifact, error) {
	a, err := s.st.GetArtifact(ctx, datapack, hash)
	if errors.Is(err, store.ErrNotFound) {
		return a, ErrNotFound
	}
	return a, err
}

// ListArtifacts is a page of the datapack's files, newest first, and how many match in all.
func (s *Service) ListArtifacts(ctx context.Context, datapack, query string, page, pageSize int) (
	[]store.Artifact, int, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, 0, err
	}
	return s.st.ListArtifacts(ctx, datapack, query, pageSize, (max(page, 1)-1)*pageSize)
}
