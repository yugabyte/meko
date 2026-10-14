package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// S3KeyIsSafeToDelete reports whether a key is not still referenced by a
// dist_rag document (DocumentExistsForURI, spanning every source_id, unlike
// a single source's existingURIs). Artifacts no longer need checking here —
// UploadArtifactObject's "artifacts/" prefix is disjoint from this space —
// assuming meko/scripts/migrate_artifacts_prefix.py has already run.
// Errors count as "not safe": this guards an unrecoverable DELETE.
func (s *KnowledgeBaseService) S3KeyIsSafeToDelete(dp *models.Datapack, key string) (bool, error) {
	docURI := fmt.Sprintf("s3://%s/%s", s.bucket, key)
	if tracked, err := s.DocumentExistsForURI(dp, docURI); err != nil {
		return false, fmt.Errorf("check document registration: %w", err)
	} else if tracked {
		return false, nil
	}
	return true, nil
}

// Bucket returns the configured S3 bucket name.
func (s *KnowledgeBaseService) Bucket() string {
	return s.bucket
}

// SumDocumentSizesByOwner returns total KB document bytes for `userID`,
// used by the upload-URL handler for free-tier cap enforcement.
func (s *KnowledgeBaseService) SumDocumentSizesByOwner(userID string) (int64, error) {
	return s.repo.SumDocumentSizesByOwner(userID)
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

// GetDocumentRef returns per-file metadata needed by the DELETE flow
// (the uploader's user_id for the permission check, the in-flight
// status for the delete guard, and the S3 URI for the bucket-side
// cleanup). Thin pass-through to the repo so the handler doesn't import
// the repository package directly. Passes the same schema/index
// constants ListDistRagDocuments uses so the returned Status is read
// from the same source as the listing's — see the repo-level comment.
func (s *KnowledgeBaseService) GetDocumentRef(
	dp *models.Datapack, documentID string,
) (repository.DocumentRef, error) {
	if dp == nil {
		return repository.DocumentRef{}, fmt.Errorf("datapack is required")
	}
	if strings.TrimSpace(documentID) == "" {
		return repository.DocumentRef{}, fmt.Errorf("document_id is required")
	}
	return s.repo.GetDocumentRef(dp.ID, documentID, DatapackIndexSchema, DatapackIndexName)
}

// DeleteDocument removes a file's chunks + dist_rag rows. The S3
// object delete is the handler's job (it owns the S3Service).
// Returns the chunk count removed for logging / response.
func (s *KnowledgeBaseService) DeleteDocument(
	dp *models.Datapack, documentID string,
) (int64, error) {
	if dp == nil {
		return 0, fmt.Errorf("datapack is required")
	}
	if strings.TrimSpace(documentID) == "" {
		return 0, fmt.Errorf("document_id is required")
	}
	return s.repo.DeleteDocument(dp.ID, documentID)
}

// ErrDocumentInFlight means DeleteDocumentByURI found a document row but its
// pipeline hasn't reached a terminal status yet, so the delete was refused.
var ErrDocumentInFlight = errors.New("document is still being processed")

// DeleteDocumentByURI removes a document's chunks + dist_rag rows by its S3
// URI, for callers that only have the URI. Returns found=false, no error,
// if no row exists there. Returns ErrDocumentInFlight for the same reason
// DeleteDocument's handler-level guard exists: a mid-chunking delete
// orphans what the rag_agent worker writes next.
func (s *KnowledgeBaseService) DeleteDocumentByURI(
	dp *models.Datapack, documentURI string,
) (found bool, err error) {
	if dp == nil {
		return false, fmt.Errorf("datapack is required")
	}
	documentID, ok, err := s.repo.GetDocumentIDForURI(dp.ID, documentURI)
	if err != nil {
		return false, fmt.Errorf("resolve document id for uri: %w", err)
	}
	if !ok {
		return false, nil
	}
	ref, err := s.repo.GetDocumentRef(dp.ID, documentID, DatapackIndexSchema, DatapackIndexName)
	if err != nil {
		return false, fmt.Errorf("get document ref %s: %w", documentID, err)
	}
	// Same allow-list as DeleteDocument's handler-level guard — see its
	// comment for why only COMPLETED/FAILED are treated as terminal.
	switch strings.ToUpper(ref.Status) {
	case "NOT_STARTED", "QUEUED", "PROCESSING", "RETRY":
		return false, ErrDocumentInFlight
	}
	if _, err := s.repo.DeleteDocument(dp.ID, documentID); err != nil {
		return false, fmt.Errorf("delete document %s: %w", documentID, err)
	}
	return true, nil
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

// DocumentExistsForURI reports whether the datapack already has a document
// registered for an S3 URI, under any of its sources.
func (s *KnowledgeBaseService) DocumentExistsForURI(
	dp *models.Datapack, documentURI string,
) (bool, error) {
	if dp == nil {
		return false, fmt.Errorf("datapack is required")
	}
	return s.repo.DocumentExistsForURI(dp.ID, documentURI)
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

// DocumentsPage is the paginated envelope returned by /documents.
type DocumentsPage struct {
	Items    []repository.KnowledgeBaseDocument `json:"items"`
	Total    int64                              `json:"total"`
	Page     int                                `json:"page"`
	PageSize int                                `json:"page_size"`
	HasMore  bool                               `json:"has_more"`
}

// ListDistRagDocuments returns a paginated, optionally-filtered page of
// uploaded files (id, name, uri, type, MD5, chunks_processed, completed_at,
// status) for every dist_rag document tied to the datapack.
// Used by the SharedKnowledge Files tab.
//
// Pulls the dist_rag schema + index name from the same constants
// `RagService` is wired with at boot, so a future per-datapack
// schema/index split touches the constants here, not callers.
//
// A viewer caller isn't told who uploaded a file, except their own.
func (s *KnowledgeBaseService) ListDistRagDocuments(
	dp *models.Datapack,
	opts repository.DocumentListOpts,
	caller *models.Account,
) (DocumentsPage, error) {
	if dp == nil {
		return DocumentsPage{}, fmt.Errorf("datapack is required")
	}
	docs, total, err := s.repo.ListDistRagDocuments(dp.ID, DatapackIndexSchema, DatapackIndexName, opts)
	if err != nil {
		return DocumentsPage{}, err
	}
	redactDocUploaders(docs, dp.Grant, caller)
	page := 1
	if opts.Limit > 0 {
		page = opts.Offset/opts.Limit + 1
	}
	return DocumentsPage{
		Items:    docs,
		Total:    total,
		Page:     page,
		PageSize: opts.Limit,
		HasMore:  int64(opts.Offset+opts.Limit) < total,
	}, nil
}

// redactDocUploaders hides who uploaded each file from a viewer, except on the
// viewer's own uploads: the UI offers delete from uploaded_by_user_id, and an
// uploader can still delete their own file.
func redactDocUploaders(docs []repository.KnowledgeBaseDocument, grant string, caller *models.Account) {
	if !models.IsViewerGrant(grant) {
		return
	}
	for i := range docs {
		if caller == nil || docs[i].UploadedByUserID == nil || *docs[i].UploadedByUserID != caller.ID {
			docs[i].UploadedByUserID = nil
			docs[i].UploadedByEmail = nil
		}
	}
}

// KnowledgeBaseSummaryResponse decorates the raw repo aggregate with
// two deterministic, UI-ready strings (Description, FilesSummary) and
// the promoted-memories count, so the SharedKnowledge page renders the
// whole header from one round trip.
//
// Both strings are templates filled in from the same numbers the rest
// of the response carries; the UI never has to compose copy itself.
// When we later swap in an LLM-generated description, only the
// `Description` field's source changes — the wire shape stays.
type KnowledgeBaseSummaryResponse struct {
	repository.KnowledgeBaseSummary
	PromotedMemoriesCount int64  `json:"promoted_memories_count"`
	Description           string `json:"description"`
	FilesSummary          string `json:"files_summary"`
}

// GetKnowledgeBaseSummary returns the aggregate + description strings
// for the header tile on the SharedKnowledge page.
//
// Three repo calls, all cheap:
//
//  1. doc/chunk/heaviest aggregates + embedding metadata
//     (`repo.GetKnowledgeBaseSummary` — two SQL statements internally)
//  2. promoted-memories count (`repo.Counts` reuses an existing query)
//
// The description and files-summary strings are computed from the
// resulting numbers via small deterministic templates — no LLM, no
// cache table. Swap the generator function (`renderDescription`,
// `renderFilesSummary`) to switch to LLM later without touching
// callers.
func (s *KnowledgeBaseService) GetKnowledgeBaseSummary(
	dp *models.Datapack,
) (KnowledgeBaseSummaryResponse, error) {
	if dp == nil {
		return KnowledgeBaseSummaryResponse{},
			fmt.Errorf("datapack is required")
	}
	agg, err := s.repo.GetKnowledgeBaseSummary(dp.ID, DatapackIndexSchema, DatapackIndexName)
	if err != nil {
		return KnowledgeBaseSummaryResponse{}, err
	}
	// Reuse existing Counts query for promoted memories — it already
	// hits `datapacks.promoted_collection`.
	counts, err := s.repo.Counts(dp.ID)
	if err != nil {
		return KnowledgeBaseSummaryResponse{}, err
	}
	resp := KnowledgeBaseSummaryResponse{
		KnowledgeBaseSummary:  agg,
		PromotedMemoriesCount: counts.PromotedMemories,
	}
	resp.Description = renderDescription(dp.Name, agg, counts.PromotedMemories)
	resp.FilesSummary = renderFilesSummary(agg)
	return resp, nil
}

// renderDescription produces the top auto-generated description for the
// SharedKnowledge page. Deterministic template; the wire shape stays the
// same when this is swapped for an LLM call.
func renderDescription(datapackName string, s repository.KnowledgeBaseSummary, promoted int64) string {
	heaviest := ""
	if s.HeaviestSourceName != nil {
		heaviest = fmt.Sprintf(" Heaviest source: %s.", *s.HeaviestSourceName)
	}
	return fmt.Sprintf(
		"Knowledge base for %s — %d files totaling %d chunks, reinforced by %d promoted memories.%s",
		datapackName, s.TotalFiles, s.TotalChunks, promoted, heaviest,
	)
}

// ActivityLogResponse pairs the merged feed rows with the opaque
// next-page cursor the repo derives. `has_more` is true iff the cursor
// would advance — i.e. the server believes there's at least one more
// row. UI's "Show more" button reads `next_cursor` straight back into
// the next request; client never needs to interpret it.
type ActivityLogResponse struct {
	Items      []repository.ActivityLogItem `json:"items"`
	HasMore    bool                         `json:"has_more"`
	NextCursor string                       `json:"next_cursor"`
	Limit      int                          `json:"limit"`
}

// ListActivityLog returns the file-upload + memory-promotion feed for
// the SharedKnowledge "Activity log" panel. Thin pass-through to the
// repo (with schema/index constants) plus cursor encode/decode at the
// wire boundary so the repo can stay strongly-typed. A viewer caller isn't
// told who uploaded a file, except their own.
func (s *KnowledgeBaseService) ListActivityLog(
	dp *models.Datapack,
	limit int, cursor string,
	caller *models.Account,
) (ActivityLogResponse, error) {
	if dp == nil {
		return ActivityLogResponse{}, fmt.Errorf("datapack is required")
	}
	// nil structural: a single datapack's feed has no folder or placement
	// events, those describe the project around it.
	resp, err := s.ListActivityLogForDatapacks([]string{dp.ID}, limit, cursor, nil)
	if err != nil || !models.IsViewerGrant(dp.Grant) {
		return resp, err
	}
	callerEmail := ""
	if caller != nil {
		callerEmail = caller.Email
	}
	for i := range resp.Items {
		redactOthersUploaderEmail(&resp.Items[i], callerEmail)
	}
	return resp, nil
}

// redactOthersUploaderEmail hides who uploaded a file from a viewer, unless the
// viewer did. The feed carries only the uploader's email, so it matches on that.
func redactOthersUploaderEmail(item *repository.ActivityLogItem, callerEmail string) {
	if e := item.UploadedByEmail; e != nil && (callerEmail == "" || !strings.EqualFold(*e, callerEmail)) {
		item.UploadedByEmail = nil
	}
}

// ListActivityLogForDatapacks is the same feed over a SET of datapacks, for
// the project-wide Activity tab.
//
// The caller decides the set, and that is the access boundary: this method
// checks nothing, so it must only ever be handed datapacks the caller can
// already see.
//
// Cost note. Both sides read every row in scope and sort, per page: the
// promotions side because promoted_collection is keyed
// (meko_datapack_id HASH, id) with nothing on the promotion timestamp, and
// the uploads side because dist_rag.vector_index_pipeline_details is a VIEW
// over a four-way join that the DISTINCT ON materialises before the keyset
// filter applies. So a project's feed costs O(project contents) per page,
// not O(page). Fine at the sizes a project reaches; the fix is an event
// table, not a better query.
// structural carries the project-only sides (folders created, datapacks
// filed). Nil from the single-datapack feed, which is what keeps those rows
// off the Knowledge page.
func (s *KnowledgeBaseService) ListActivityLogForDatapacks(
	datapackIDs []string,
	limit int, cursor string,
	structural *repository.StructuralActivity,
) (ActivityLogResponse, error) {
	c := repository.DecodeActivityLogCursor(cursor)
	items, nextCursor, err := s.repo.ListActivityLog(
		datapackIDs, DatapackIndexSchema, DatapackIndexName, limit, c, structural,
	)
	if err != nil {
		return ActivityLogResponse{}, err
	}
	encoded := repository.EncodeActivityLogCursor(nextCursor)
	return ActivityLogResponse{
		Items:      items,
		HasMore:    encoded != "",
		NextCursor: encoded,
		Limit:      limit,
	}, nil
}

// renderFilesSummary is the lower "FILES SUMMARY · AUTO" line. Same
// numbers as the description, condensed.
func renderFilesSummary(s repository.KnowledgeBaseSummary) string {
	if s.TotalFiles == 0 {
		return "No files uploaded yet."
	}
	if s.HeaviestSourceName == nil {
		return fmt.Sprintf("%d files totaling %d chunks.", s.TotalFiles, s.TotalChunks)
	}
	return fmt.Sprintf(
		"%d files totaling %d chunks. Heaviest source: %s.",
		s.TotalFiles, s.TotalChunks, *s.HeaviestSourceName,
	)
}
