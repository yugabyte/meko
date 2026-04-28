package service

import (
	"encoding/json"
	"fmt"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// KnowledgeBaseService provides business logic for knowledge base
// operations. All methods take an already-resolved *models.Datapack
// (loaded by RequireDatapackOwnership middleware) so the service does
// not have to re-look-up the datapack by name.
type KnowledgeBaseService struct {
	repo   *repository.KnowledgeBaseRepo
	bucket string
}

// NewKnowledgeBaseService creates a new KnowledgeBaseService.
//
// bucket is the configured S3 bucket name used to construct
// knowledge_base_url values for the CreateKB flow. It may be empty
// when the S3 service is disabled; in that case CreateKB is unusable
// but Add/List/Delete still work against caller-supplied URLs.
func NewKnowledgeBaseService(
	repo *repository.KnowledgeBaseRepo,
	bucket string,
) *KnowledgeBaseService {
	return &KnowledgeBaseService{repo: repo, bucket: bucket}
}

// Bucket returns the configured S3 bucket name.
func (s *KnowledgeBaseService) Bucket() string {
	return s.bucket
}

// BuildDatapackPrefix returns the canonical S3 prefix for the given
// (user_id, datapack_id) pair: "s3://<bucket>/<user_id>/<datapack_id>/".
// Used both by the upload flow and CreateKB to construct a single
// per-datapack source URI.
func (s *KnowledgeBaseService) BuildDatapackPrefix(
	userID, datapackID string,
) string {
	return fmt.Sprintf("s3://%s/%s/%s/", s.bucket, userID, datapackID)
}

// AddRequest is the payload accepted by KnowledgeBaseHandler.Add.
//
// TODO(external-kb): currently unused on the canonical create path —
// CreateKB owns the per-datapack KB row keyed by the dist_rag source
// UUID. AddRequest is reserved for a future "register an external KB
// URL" flow where the URL is not the per-datapack S3 prefix and there
// is no dist_rag source backing it.
type AddRequest struct {
	URL     string          `json:"url"`
	Details json.RawMessage `json:"details,omitempty"`
}

// AddResult summarizes the row inserted by KnowledgeBaseService.Add.
//
// TODO(external-kb): see AddRequest — this type is reserved for the
// future external-KB registration flow.
type AddResult struct {
	DatapackID  string `json:"datapack_id"`
	RagSourceID string `json:"rag_source_id"`
	URL         string `json:"knowledge_base_url"`
}

// Add validates the request and inserts a knowledge_base row for the
// supplied datapack. The repo generates rag_source_id server-side via
// gen_random_uuid() and returns it.
//
// TODO(external-kb): not on the canonical create path; CreateKB persists
// the per-datapack KB row using the dist_rag source UUID via
// InsertWithID. Add() is kept wired up so the future external-KB
// registration flow can reuse it without re-introducing the route.
func (s *KnowledgeBaseService) Add(
	dp *models.Datapack, req AddRequest,
) (*AddResult, error) {
	if dp == nil {
		return nil, fmt.Errorf("datapack is required")
	}
	if req.URL == "" {
		return nil, fmt.Errorf("url is required")
	}

	ragSourceID, err := s.repo.Insert(dp.ID, req.URL, req.Details)
	if err != nil {
		return nil, err
	}
	return &AddResult{
		DatapackID:  dp.ID,
		RagSourceID: ragSourceID,
		URL:         req.URL,
	}, nil
}

// InsertWithID inserts a knowledge_base row using a caller-supplied
// rag_source_id (typically the UUID returned by dist_rag.create_source).
func (s *KnowledgeBaseService) InsertWithID(
	datapackID, ragSourceID, url string, details json.RawMessage,
) error {
	return s.repo.InsertWithID(datapackID, ragSourceID, url, details)
}

// List returns every KB row for the given datapack.
func (s *KnowledgeBaseService) List(
	dp *models.Datapack,
) ([]models.KnowledgeBase, error) {
	if dp == nil {
		return nil, fmt.Errorf("datapack is required")
	}
	return s.repo.ListByDatapack(dp.ID)
}

// Delete removes a single KB row identified by rag_source_id within
// the given datapack. Surfaces ErrKnowledgeBaseNotFound when no such
// row exists.
func (s *KnowledgeBaseService) Delete(
	dp *models.Datapack, ragSourceID string,
) error {
	if dp == nil {
		return fmt.Errorf("datapack is required")
	}
	if ragSourceID == "" {
		return fmt.Errorf("rag_source_id is required")
	}
	return s.repo.Delete(dp.ID, ragSourceID)
}

// Upsert inserts or updates a knowledge_base row keyed by
// (datapack_id, rag_source_id). Used by the CreateKB flow which
// needs idempotency on retried calls.
func (s *KnowledgeBaseService) Upsert(
	datapackID, ragSourceID, url string, details json.RawMessage,
) error {
	return s.repo.Upsert(datapackID, ragSourceID, url, details)
}

// GetSourceIDs returns the rag_source_id values stored for a datapack.
func (s *KnowledgeBaseService) GetSourceIDs(
	dp *models.Datapack,
) ([]string, error) {
	if dp == nil {
		return nil, fmt.Errorf("datapack is required")
	}
	return s.repo.GetSourceIDs(dp.ID)
}

// GetSourceURIs returns the knowledge_base_url values for a datapack.
func (s *KnowledgeBaseService) GetSourceURIs(
	dp *models.Datapack,
) ([]string, error) {
	if dp == nil {
		return nil, fmt.Errorf("datapack is required")
	}
	return s.repo.GetSourceURIs(dp.ID)
}

// DeleteByDatapack removes all KB rows for a datapack. Used to clear
// stale RAG cache when the dist_rag source no longer exists.
func (s *KnowledgeBaseService) DeleteByDatapack(dp *models.Datapack) error {
	if dp == nil {
		return fmt.Errorf("datapack is required")
	}
	return s.repo.DeleteByDatapack(dp.ID)
}

// Counts returns the per-datapack chunk / doc / promoted-memory
// breakdown that backs the "Knowledge Base" tile.
func (s *KnowledgeBaseService) Counts(
	dp *models.Datapack,
) (repository.KnowledgeBaseCounts, error) {
	if dp == nil {
		return repository.KnowledgeBaseCounts{},
			fmt.Errorf("datapack is required")
	}
	return s.repo.Counts(dp.ID)
}

// ListDistRagDocuments returns the (document_name, status) pairs for
// every dist_rag document tied to the datapack via dist_rag.sources.
// Used by the "uploaded documents" tab.
func (s *KnowledgeBaseService) ListDistRagDocuments(
	dp *models.Datapack,
) ([]repository.KnowledgeBaseDocument, error) {
	if dp == nil {
		return nil, fmt.Errorf("datapack is required")
	}
	return s.repo.ListDistRagDocuments(dp.ID)
}
