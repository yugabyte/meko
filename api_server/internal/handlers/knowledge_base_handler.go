package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// KnowledgeBaseHandler holds Echo handlers for knowledge-base endpoints.
//
// In the multi-tenant model the handler operates entirely against the
// shared meko_system.knowledge_bases table and the shared dist_rag
// extension on the api_server's primary database connection: there is
// no per-datapack database to open and no ownership transfer to fix.
type KnowledgeBaseHandler struct {
	knowledgeBaseSvc *service.KnowledgeBaseService
	ragSvc           *service.RagService
	s3Svc            *service.S3Service
}

// NewKnowledgeBaseHandler creates a new KnowledgeBaseHandler.
func NewKnowledgeBaseHandler(
	knowledgeBaseSvc *service.KnowledgeBaseService,
	ragSvc *service.RagService,
	s3Svc *service.S3Service,
) *KnowledgeBaseHandler {
	return &KnowledgeBaseHandler{
		knowledgeBaseSvc: knowledgeBaseSvc,
		ragSvc:           ragSvc,
		s3Svc:            s3Svc,
	}
}

// Add handles POST /api/v1/datapacks/:name/knowledge-bases.
//
// TODO(external-kb): not on the canonical create path. The per-datapack
// KB row is created/refreshed by CreateKB (POST /knowledge-bases/create)
// using the dist_rag source UUID. Add() is reserved for a future
// "register an external KB URL" flow where the caller supplies a URL
// that is not the s3://<bucket>/<user_id>/<datapack_id>/ prefix and is
// not backed by a dist_rag source.
//
//	@Summary	Add a knowledge base URL to the datapack
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string				true	"Datapack name"
//	@Param		body	body		service.AddRequest	true	"Knowledge base URL"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases [post]
func (h *KnowledgeBaseHandler) Add(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	var req service.AddRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	result, err := h.knowledgeBaseSvc.Add(dp, req)
	if err != nil {
		return mapKBError(c, err)
	}

	return ok(c, http.StatusCreated, result)
}

// List handles GET /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	List knowledge bases for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases [get]
func (h *KnowledgeBaseHandler) List(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	kbs, err := h.knowledgeBaseSvc.List(dp)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if kbs == nil {
		kbs = []models.KnowledgeBase{}
	}
	return ok(c, http.StatusOK, kbs)
}

// Delete handles DELETE /api/v1/datapacks/:name/knowledge-bases/:rag_source_id.
//
//	@Summary	Delete a knowledge base
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name			path		string	true	"Datapack name"
//	@Param		rag_source_id	path		string	true	"rag_source_id (UUID)"
//	@Success	200				{object}	APIResponse
//	@Failure	400				{object}	APIResponse
//	@Failure	401				{object}	APIResponse
//	@Failure	404				{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/{rag_source_id} [delete]
func (h *KnowledgeBaseHandler) Delete(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	ragSourceID := c.Param("rag_source_id")
	if ragSourceID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "rag_source_id is required")
	}

	if err := h.knowledgeBaseSvc.Delete(dp, ragSourceID); err != nil {
		return mapKBError(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{
		"message":       "knowledge base deleted",
		"rag_source_id": ragSourceID,
	})
}

type uploadURLRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

var allowedExtensions = map[string]bool{
	".pdf":  true,
	".txt":  true,
	".md":   true,
	".json": true,
	".mp4":  true,
}

const maxFileSizeBytes = 5 * 1024 * 1024 // 5 MB

// RequestUploadURL handles POST /api/v1/datapacks/:name/knowledge-bases/upload-url.
//
//	@Summary	Generate a presigned S3 upload URL
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string				true	"Datapack name"
//	@Param		body	body		uploadURLRequest	true	"Upload request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/upload-url [post]
func (h *KnowledgeBaseHandler) RequestUploadURL(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	var req uploadURLRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if req.Filename == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "filename is required")
	}
	ext := strings.ToLower(filepath.Ext(req.Filename))
	if !allowedExtensions[ext] {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			"unsupported file type: "+ext+
				"; allowed: .pdf, .txt, .md, .json, .mp4")
	}
	if req.Size > maxFileSizeBytes {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "file too large; maximum 5 MB")
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	result, err := h.s3Svc.GenerateUploadURL(
		c.Request().Context(),
		dp.UserID,
		dp.ID,
		req.Filename,
		req.ContentType,
	)
	if err != nil {
		if errors.Is(err, service.ErrDuplicateFile) {
			return apiError(c, http.StatusConflict,
				"DUPLICATE_FILE", err.Error())
		}
		slog.Error("failed to generate presigned upload URL",
			"user_id", dp.UserID,
			"datapack_id", dp.ID,
			"filename", req.Filename,
			"error", err,
		)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to generate upload URL")
	}

	slog.Info("presigned upload URL generated",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"filename", req.Filename,
		"size", req.Size,
		"key", result.Key,
	)

	return ok(c, http.StatusOK, result)
}

type uploadCompleteRequest struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// ConfirmUpload handles POST /api/v1/datapacks/:name/knowledge-bases/upload-complete.
//
// This endpoint is currently advisory: the actual KB row is created by
// the subsequent /knowledge-bases/create call which lists the S3 prefix
// and registers all uploaded files with dist_rag.
//
//	@Summary	Confirm a completed file upload
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string					true	"Datapack name"
//	@Param		body	body		uploadCompleteRequest	true	"Upload confirmation"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/upload-complete [post]
func (h *KnowledgeBaseHandler) ConfirmUpload(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	var req uploadCompleteRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	if req.Key == "" || req.Filename == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "key and filename are required")
	}

	slog.Info("file upload confirmed",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"key", req.Key,
		"filename", req.Filename,
		"size", req.Size,
	)

	return ok(c, http.StatusOK, map[string]string{
		"message": "upload confirmed",
		"key":     req.Key,
	})
}

// CreateKB handles POST /api/v1/datapacks/:name/knowledge-bases/create.
//
// Workflow:
//
//  1. Build the S3 source URI: s3://<bucket>/<user_id>/<datapack_id>/.
//
//  2. Ensure the dist_rag vector index exists.
//
//  3. Look up or create the dist_rag source for that URI.
//
//  4. Sync new S3 objects into dist_rag.documents.
//
//  5. Upsert a meko_system.knowledge_bases row keyed by
//     (datapack_id, rag_source_id) so subsequent lookups can map
//     this datapack back to its dist_rag source.
//
//  6. Trigger build_index (asynchronously for newly-created sources,
//     synchronously when only new documents were added).
//
//     @Summary	Create or refresh a knowledge base from uploaded files
//     @Tags		knowledge-bases
//     @Produce	json
//     @Param		name	path		string	true	"Datapack name"
//     @Success	201		{object}	APIResponse
//     @Failure	400		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Failure	503		{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{name}/knowledge-bases/create [post]
func (h *KnowledgeBaseHandler) CreateKB(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}
	if h.ragSvc == nil {
		return apiError(c, http.StatusServiceUnavailable,
			"RAG_DISABLED", "rag service is not configured")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	if err := h.ragSvc.EnsureVectorIndex(); err != nil {
		slog.Error("failed to ensure vector index",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to initialize vector index")
	}

	sourceURI := h.knowledgeBaseSvc.BuildDatapackPrefix(dp.UserID, dp.ID)
	s3Prefix := fmt.Sprintf("%s/%s/", dp.UserID, dp.ID)

	slog.Info("creating knowledge base",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"source_uri", sourceURI,
	)

	// Step 1: try to reuse a previously-cached rag_source_id from
	// meko_system.knowledge_bases.
	cachedSourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dp)
	if err != nil {
		slog.Error("failed to load cached source IDs", "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to check cached sources")
	}

	var sourceID string
	isNewSource := false

	if len(cachedSourceIDs) > 0 {
		sourceID = cachedSourceIDs[0]

		exists, err := h.ragSvc.SourceExists(sourceID)
		if err != nil {
			slog.Error("failed to validate cached source_id",
				"source_id", sourceID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to validate cached source")
		}
		if !exists {
			slog.Warn("cached source_id is stale, clearing cache",
				"source_id", sourceID, "datapack_id", dp.ID)
			if err := h.knowledgeBaseSvc.DeleteByDatapack(dp); err != nil {
				slog.Error("failed to clear stale KB cache", "error", err)
			}
			cachedSourceIDs = nil
			sourceID = ""
		}
	}

	if sourceID == "" {
		// Step 2: not cached or cache was stale; check dist_rag.sources.
		sourceID, err = h.ragSvc.LookupSourceByURI(sourceURI)
		if err != nil {
			slog.Error("failed to lookup source by URI", "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to lookup source")
		}
		if sourceID == "" {
			isNewSource = true
			sourceID, err = h.ragSvc.CreateSource(sourceURI, dp.ID)
			if err != nil {
				slog.Error("failed to create dist_rag source", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to create source")
			}
			if err := h.ragSvc.AddSourceToIndex(sourceID); err != nil {
				slog.Error("failed to add source to index", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to add source to index")
			}
		}

		// Persist the rag_source_id so subsequent calls reuse it.
		if err := h.knowledgeBaseSvc.InsertWithID(
			dp.ID, sourceID, sourceURI, nil,
		); err != nil {
			slog.Error("failed to save knowledge base", "error", err)
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to save knowledge base")
		}
	}

	// Step 3: sync any new S3 objects into dist_rag.documents.
	objects, err := h.s3Svc.ListObjects(c.Request().Context(), s3Prefix)
	if err != nil {
		slog.Error("failed to list S3 objects",
			"prefix", s3Prefix, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"S3_ERROR", "failed to list uploaded files")
	}

	existingURIs, err := h.ragSvc.GetExistingDocURIs(sourceID)
	if err != nil {
		slog.Error("failed to get existing doc URIs", "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to check existing documents")
	}

	newDocs := 0
	for _, obj := range objects {
		docURI := fmt.Sprintf("s3://%s/%s", h.s3Svc.Bucket(), obj.Key)
		if existingURIs[docURI] {
			continue
		}
		if err := h.ragSvc.InsertDocument(
			sourceID, obj.Key, docURI, obj.ETag, obj.ContentType,
		); err != nil {
			slog.Warn("failed to insert document",
				"document", obj.Key, "error", err)
		} else {
			newDocs++
		}
	}

	slog.Info("document sync complete",
		"datapack_id", dp.ID,
		"total_s3_objects", len(objects),
		"already_tracked", len(existingURIs),
		"new_documents", newDocs)

	// Step 4: trigger index build.
	switch {
	case isNewSource:
		go h.ragSvc.WaitAndBuild(sourceID)
	case newDocs > 0:
		if err := h.ragSvc.BuildIndex(); err != nil {
			slog.Error("failed to build index",
				"datapack_id", dp.ID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to build index")
		}
	}

	slog.Info("knowledge base created",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"source_id", sourceID,
		"is_new_source", isNewSource,
		"index_name", h.ragSvc.IndexName(),
	)

	return ok(c, http.StatusCreated, map[string]string{
		"datapack_id":   dp.ID,
		"rag_source_id": sourceID,
		"index_name":    h.ragSvc.IndexName(),
		"source_uri":    sourceURI,
		"status":        "build_triggered",
	})
}

// KBStatus handles GET /api/v1/datapacks/:name/knowledge-bases/status.
//
//	@Summary	Get knowledge base pipeline status
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/status [get]
func (h *KnowledgeBaseHandler) KBStatus(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	sourceURIs, err := h.knowledgeBaseSvc.GetSourceURIs(dp)
	if err != nil {
		slog.Error("failed to get source URIs",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to get source URIs")
	}
	sourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dp)
	if err != nil {
		slog.Error("failed to get source IDs",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to get source IDs")
	}

	if h.ragSvc == nil || len(sourceIDs) == 0 {
		indexName := ""
		if h.ragSvc != nil {
			indexName = h.ragSvc.IndexName()
		}
		return ok(c, http.StatusOK, &repository.PipelineStatus{
			IndexName: indexName,
		})
	}

	status, err := h.ragSvc.GetPipelineStatus(sourceURIs)
	if err != nil {
		slog.Error("failed to get pipeline status",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to get pipeline status")
	}
	srcStatuses, err := h.ragSvc.GetSourceStatuses(sourceIDs)
	if err != nil {
		slog.Error("failed to get source statuses",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to get source statuses")
	}
	status.Sources = srcStatuses

	slog.Info("KB status response",
		"datapack_id", dp.ID,
		"sources", len(srcStatuses),
		"pipeline_details", len(status.Details),
	)

	return ok(c, http.StatusOK, status)
}

// Counts handles GET /api/v1/datapacks/:name/knowledge-bases/counts.
//
// Returns the per-datapack breakdown of total uploaded documents,
// total stored chunks, and total promoted memories.
//
//	@Summary	Knowledge base counts breakdown for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/counts [get]
func (h *KnowledgeBaseHandler) Counts(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	counts, err := h.knowledgeBaseSvc.Counts(dp)
	if err != nil {
		slog.Error("failed to load knowledge base counts",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to load knowledge base counts")
	}
	return ok(c, http.StatusOK, counts)
}

// Documents handles GET /api/v1/datapacks/:name/knowledge-bases/documents.
//
// Returns (document_name, status) for every dist_rag.documents row
// whose source belongs to the datapack.
//
//	@Summary	List uploaded documents for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/documents [get]
func (h *KnowledgeBaseHandler) Documents(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	docs, err := h.knowledgeBaseSvc.ListDistRagDocuments(dp)
	if err != nil {
		slog.Error("failed to list knowledge base documents",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to list knowledge base documents")
	}
	return ok(c, http.StatusOK, docs)
}

// mapKBError converts a KB-related error into the matching HTTP
// envelope: 400 for validation, 404 for ErrKnowledgeBaseNotFound,
// 409 for ErrKnowledgeBaseExists, and 500 otherwise.
func mapKBError(c echo.Context, err error) error {
	switch {
	case isValidationErr(err):
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", err.Error())
	case errors.Is(err, repository.ErrKnowledgeBaseNotFound):
		return apiError(c, http.StatusNotFound,
			"NOT_FOUND", err.Error())
	case errors.Is(err, repository.ErrKnowledgeBaseExists):
		return apiError(c, http.StatusConflict,
			"CONFLICT", err.Error())
	default:
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
}
