package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// buildMekoDocDetails returns the JSON written to dist_rag.documents.document_details.
//
// Shape:
//
//		{"meko": {"user_id": "<uploader>", "size": <bytes>, "uploaded_at": "<RFC3339>"}}
//
//	  - user_id      authenticated uploader (NOT the datapack owner — multiple
//	    collaborators can upload to the same shared datapack, and we
//	    want per-file uploader provenance).
//	  - size         bytes from S3 Contents.Size; 0 means "unknown" rather
//	    than "empty file" (S3 occasionally omits Size on certain
//	    listing edge cases).
//	  - uploaded_at  S3 LastModified timestamp (RFC3339 UTC) — the moment
//	    the object was written to the bucket. This is the canonical
//	    "uploaded" time shown in the UI. Zero time is omitted so
//	    legacy rows without it degrade gracefully.
//
// Output is wire-ready JSONB. Returns nil only on json.Marshal failure
// — caller logs + falls back to "{}" so the upsert never fails because
// of details encoding.
func buildMekoDocDetails(uploaderUserID string, size int64, uploadedAt time.Time) []byte {
	meko := map[string]any{
		"user_id": uploaderUserID,
		"size":    size,
	}
	if !uploadedAt.IsZero() {
		meko["uploaded_at"] = uploadedAt.UTC().Format(time.RFC3339)
	}
	payload := map[string]any{
		"meko": meko,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Shouldn't happen for this fixed shape; log + fall through so
		// the document is still inserted with empty details rather
		// than the whole upload failing.
		slog.Warn("buildMekoDocDetails: marshal failed", "error", err)
		return nil
	}
	return b
}

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
	// ContentHash is the hex-encoded SHA-256 of the file bytes, computed by
	// the client before requesting the presigned URL. It is used as the S3
	// sub-directory so that (a) the same filename with different content gets
	// a unique path, and (b) re-uploading identical bytes is idempotent.
	ContentHash string `json:"content_hash"`
}

var allowedExtensions = map[string]bool{
	".pdf":  true,
	".txt":  true,
	".md":   true,
	".json": true,
	".mp4":  true,
}

// isHexSHA256 reports whether s is exactly 64 hexadecimal characters, i.e. a
// valid hex-encoded SHA-256 digest. Used to gate the client-supplied
// content_hash before it becomes an S3 key segment.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
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
	req.ContentHash = strings.ToLower(strings.TrimSpace(req.ContentHash))
	if req.ContentHash == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "content_hash is required")
	}
	// Must be a 64-char hex SHA-256. Reject anything else: the hash is used
	// verbatim as an S3 key segment, so a non-hex value (e.g. one containing
	// "../" or other path separators) could escape the per-user namespace.
	if !isHexSHA256(req.ContentHash) {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "content_hash must be a 64-character hex SHA-256")
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
		req.ContentHash,
	)
	if err != nil {
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

// downloadURLRequest is the body for POST /knowledge-bases/download-url.
//
// The caller passes the `document_uri` they got from
// GET /knowledge-bases/documents (full `s3://...` URI). We strip the
// bucket prefix to get the S3 key and verify it belongs to this
// datapack before signing — without that check, a caller could ask
// for a presigned URL on any key in the bucket they happen to know.
type downloadURLRequest struct {
	DocumentURI string `json:"document_uri"`
}

// RequestDownloadURL handles POST /api/v1/datapacks/:name/knowledge-bases/download-url.
//
// Returns a short-lived presigned S3 GET URL for the given document.
// The UI fetches this on click and opens it directly — no api_server
// in the file path. Mirrors the upload-URL flow.
//
//	@Summary	Presigned S3 GET URL for a knowledge-base document
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string				true	"Datapack name"
//	@Param		body	body		downloadURLRequest	true	"Document download request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	403		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/download-url [post]
func (h *KnowledgeBaseHandler) RequestDownloadURL(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file downloads are not configured")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	var req downloadURLRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}
	uri := strings.TrimSpace(req.DocumentURI)
	if uri == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "document_uri is required")
	}

	// Convert `s3://<bucket>/<key>` → `<key>`. Anything that isn't a
	// well-formed S3 URI for our bucket is rejected outright — better
	// than silently signing an arbitrary key.
	bucketPrefix := "s3://" + h.s3Svc.Bucket() + "/"
	if !strings.HasPrefix(uri, bucketPrefix) {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "document_uri must be an s3:// URI in this bucket")
	}
	key := strings.TrimPrefix(uri, bucketPrefix)

	// Ownership check. Files are stored under
	// `<owner_user_id>/<datapack_id>/<filename>` (see
	// S3Service.GenerateUploadURL). The datapack here is the resolved
	// row from RequireDatapackAccess middleware, so dp.UserID is the
	// owner regardless of who's calling — viewers/maintainers all
	// flow through the same path and the prefix matches. Anything
	// else means the caller is trying to fetch a file outside this
	// datapack's namespace.
	expectedPrefix := dp.UserID + "/" + dp.ID + "/"
	if !strings.HasPrefix(key, expectedPrefix) {
		return apiError(c, http.StatusForbidden,
			"FORBIDDEN", "document does not belong to this datapack")
	}

	result, err := h.s3Svc.GenerateDownloadURL(c.Request().Context(), key)
	if err != nil {
		slog.Error("failed to generate presigned download URL",
			"datapack_id", dp.ID, "key", key, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to generate download URL")
	}
	return ok(c, http.StatusOK, result)
}

// canDeleteKBDocument is the permission policy for the per-file DELETE
// endpoint. A caller can delete a KB document iff ANY of:
//
//   - they own the datapack (dp.Grant == "owner")
//   - they are a maintainer of the datapack (dp.Grant == "maintainer")
//   - they are the recorded uploader of THIS specific file (the
//     uploader_user_id stored in document_details->meko->user_id at
//     CreateKB time)
//
// Viewers and contributors fall through to 403 unless they uploaded
// the file themselves. Legacy files (NULL document_details, e.g.
// pre-dating the dual-write) have no recorded uploader; only
// owner/maintainer can delete those.
func canDeleteKBDocument(
	dpGrant string,
	callerUserID string,
	fileUploaderUserID *string,
) bool {
	g := strings.ToLower(strings.TrimSpace(dpGrant))
	if g == "owner" || g == string(models.GrantMaintainer) {
		return true
	}
	if fileUploaderUserID != nil &&
		strings.TrimSpace(*fileUploaderUserID) != "" &&
		strings.EqualFold(*fileUploaderUserID, callerUserID) {
		return true
	}
	return false
}

// DeleteDocument handles DELETE /api/v1/datapacks/:name/knowledge-bases/documents/:document_id.
//
// Removes a single knowledge-base file end-to-end:
//
//  1. Lookup the document (validates it belongs to this datapack and
//     returns its uploader_user_id + s3 URI in one query).
//
//  2. Permission check (see canDeleteKBDocument).
//
//  3. Delete chunks + dist_rag rows in a single transaction (repo).
//
//  4. Best-effort S3 delete — failures are logged but do not unwind
//     the DB delete (the orphan storage cost is small, a sweeper can
//     reconcile later).
//
//     @Summary	Delete one knowledge-base file
//     @Tags		knowledge-bases
//     @Produce	json
//     @Param		name		path	string	true	"Datapack name"
//     @Param		document_id	path	string	true	"dist_rag document UUID"
//     @Success	200		{object}	APIResponse
//     @Failure	400		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	403		{object}	APIResponse
//     @Failure	404		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{name}/knowledge-bases/documents/{document_id} [delete]
func (h *KnowledgeBaseHandler) DeleteDocument(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}
	acct, err := auth.GetAccount(c)
	if err != nil || acct == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "authentication required")
	}
	documentID := strings.TrimSpace(c.Param("document_id"))
	if documentID == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "document_id is required")
	}

	// Step 1: lookup. Single query returns uploader + s3 URI; also
	// validates the document actually belongs to this datapack (the
	// repo joins on the datapack's rag_source_ids).
	ref, err := h.knowledgeBaseSvc.GetDocumentRef(dp, documentID)
	if err != nil {
		if errors.Is(err, repository.ErrDocumentNotFound) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "document not found in this datapack")
		}
		slog.Error("failed to load document ref",
			"datapack_id", dp.ID, "document_id", documentID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to load document")
	}

	// Step 2: permission.
	if !canDeleteKBDocument(dp.Grant, acct.ID, ref.UploadedByUserID) {
		return apiError(c, http.StatusForbidden,
			"FORBIDDEN", "only datapack owners, maintainers, or the file's uploader can delete this file")
	}

	// Step 3: DB cleanup (chunks + dist_rag) in a single transaction.
	chunksDeleted, err := h.knowledgeBaseSvc.DeleteDocument(dp, documentID)
	if err != nil {
		if errors.Is(err, repository.ErrDocumentNotFound) {
			// Lost the race with a concurrent delete — treat as 404.
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", "document not found in this datapack")
		}
		slog.Error("failed to delete document",
			"datapack_id", dp.ID, "document_id", documentID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to delete document")
	}

	// Step 4: best-effort S3 delete. We've already removed the DB
	// pointers, so the user-visible state is consistent even if S3
	// errors out (orphan object that a sweeper can collect).
	s3Deleted := false
	if h.s3Svc != nil && h.s3Svc.Enabled() && ref.DocumentURI != nil {
		bucketPrefix := "s3://" + h.s3Svc.Bucket() + "/"
		if strings.HasPrefix(*ref.DocumentURI, bucketPrefix) {
			key := strings.TrimPrefix(*ref.DocumentURI, bucketPrefix)
			if delErr := h.s3Svc.DeleteObject(c.Request().Context(), key); delErr != nil {
				slog.Warn("S3 delete failed; DB rows already removed (orphan tolerated)",
					"datapack_id", dp.ID, "document_id", documentID,
					"key", key, "error", delErr)
			} else {
				s3Deleted = true
			}
		}
	}

	slog.Info("knowledge base document deleted",
		"datapack_id", dp.ID,
		"document_id", documentID,
		"chunks_deleted", chunksDeleted,
		"s3_deleted", s3Deleted,
		"by_user_id", acct.ID,
	)

	return ok(c, http.StatusOK, map[string]any{
		"document_id":    documentID,
		"chunks_deleted": chunksDeleted,
		"s3_deleted":     s3Deleted,
	})
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
	slog.Info("CreateKB request received",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"datapack_name", dp.Name,
	)

	if err := h.ragSvc.EnsureVectorIndex(); err != nil {
		slog.Error("failed to ensure vector index",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to initialize vector index")
	}

	sourceURI := h.knowledgeBaseSvc.BuildDatapackPrefix(dp.UserID, dp.ID)
	s3Prefix := fmt.Sprintf("%s/%s/", dp.UserID, dp.ID)

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
	slog.Info("CreateKB: source resolved",
		"datapack_id", dp.ID,
		"source_id", sourceID,
		"is_new_source", isNewSource,
	)

	newDocs := 0
	skippedDocs := 0
	shouldSkipAPIDocumentSync := isNewSource
	if !shouldSkipAPIDocumentSync {
		sourceStatus, statusErr := h.ragSvc.GetSourceStatus(sourceID)
		if statusErr != nil {
			slog.Warn("CreateKB: failed to read source status before API-side sync; proceeding with sync",
				"datapack_id", dp.ID,
				"source_id", sourceID,
				"error", statusErr,
			)
		} else {
			switch sourceStatus {
			case service.SourceStatusQueued, service.SourceStatusInProgress:
				shouldSkipAPIDocumentSync = true
				slog.Info("CreateKB: source is still being discovered/processed by dist_rag, skipping API-side document sync",
					"datapack_id", dp.ID,
					"source_id", sourceID,
					"source_status", sourceStatus,
				)
			}
		}
	}
	if shouldSkipAPIDocumentSync {
		// For a brand new source, dist_rag's CREATE_SOURCE worker discovers and
		// inserts documents. Inserting from api_server at the same time can create
		// duplicate document rows when no UNIQUE(source_id, document_uri) exists.
		slog.Info("CreateKB: skipping API-side document sync; relying on dist_rag source discovery",
			"datapack_id", dp.ID,
			"source_id", sourceID,
			"is_new_source", isNewSource,
		)
	} else {
		// Step 3: sync any new S3 objects into dist_rag.documents.
		objects, err := h.s3Svc.ListObjects(c.Request().Context(), s3Prefix)
		if err != nil {
			slog.Error("failed to list S3 objects",
				"prefix", s3Prefix, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"S3_ERROR", "failed to list uploaded files")
		}
		// set of existing document URIs
		existingURIs, err := h.ragSvc.GetExistingDocURIs(sourceID)
		if err != nil {
			slog.Error("failed to get existing doc URIs set", "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to check existing documents")
		}

		// Caller of CreateKB is recorded as the uploader for every
		// newly-inserted document. This is the best approximation we
		// have at sync time: ListObjects doesn't return per-object
		// uploader identity, and ConfirmUpload (which DOES know the
		// per-file uploader) is currently advisory.
		// TODO(uploader-precision): persist (s3_key -> uploader_user_id)
		// from ConfirmUpload into a small meko_system table so this
		// loop can attribute each file to its actual uploader instead
		// of the CreateKB caller.
		var uploaderUserID string
		if acct, acctErr := auth.GetAccount(c); acctErr == nil && acct != nil {
			uploaderUserID = acct.ID
		} else {
			uploaderUserID = dp.UserID // last-resort fallback
		}
		for _, obj := range objects {
			docURI := fmt.Sprintf("s3://%s/%s", h.s3Svc.Bucket(), obj.Key)
			if existingURIs[docURI] {
				skippedDocs++
				continue
			}
			// document_details carries per-file provenance: who
			// uploaded it (resolved above) + the byte size observed
			// in S3. The downstream documents-list endpoint resolves
			// user_id -> email at read time via meko_system.users.
			details := buildMekoDocDetails(uploaderUserID, obj.Size, obj.UploadedAt)
			if err := h.ragSvc.InsertDocument(
				sourceID, obj.Key, docURI, obj.ETag, obj.ContentType, details,
			); err != nil {
				slog.Warn("failed to insert document",
					"document", obj.Key, "document_uri", docURI,
					"error", err)
			} else {
				newDocs++
			}
		}

		slog.Info("document sync complete",
			"datapack_id", dp.ID,
			"total_s3_objects", len(objects),
			"already_tracked", len(existingURIs),
			"skipped_documents", skippedDocs,
			"new_documents", newDocs)
	}

	// Step 4: trigger index build.
	switch {
	case isNewSource:
		slog.Info("CreateKB: new source path, scheduling WaitAndBuild",
			"datapack_id", dp.ID,
			"source_id", sourceID,
		)
		go h.ragSvc.WaitAndBuild(sourceID)
	case newDocs > 0:
		slog.Info("CreateKB: source path already exists and new docs were added, triggering BuildIndex",
			"datapack_id", dp.ID,
			"source_id", sourceID,
			"new_docs", newDocs,
		)
		if err := h.ragSvc.BuildIndex(); err != nil {
			slog.Error("failed to build index",
				"datapack_id", dp.ID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to build index")
		}
	default:
		slog.Info("CreateKB: no new docs for existing source, skipping BuildIndex",
			"datapack_id", dp.ID,
			"source_id", sourceID,
		)
	}

	slog.Info("knowledge base created",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"source_id", sourceID,
		"is_new_source", isNewSource,
		"index_name", h.ragSvc.FullIndexName(),
	)

	return ok(c, http.StatusCreated, map[string]string{
		"datapack_id":   dp.ID,
		"rag_source_id": sourceID,
		"index_name":    h.ragSvc.FullIndexName(),
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
	slog.Info("KBStatus: loaded source references",
		"datapack_id", dp.ID,
		"source_ids_count", len(sourceIDs),
		"source_uris_count", len(sourceURIs),
		"source_ids", sourceIDs,
		"source_uris", sourceURIs,
	)

	if h.ragSvc == nil || len(sourceIDs) == 0 {
		indexName := ""
		if h.ragSvc != nil {
			indexName = h.ragSvc.FullIndexName()
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
// Returns a paginated envelope { items, total, page, page_size, has_more }
// of dist_rag documents for the datapack.
//
// Query params:
//
//   - page        int    (default 1)
//
//   - page_size   int    (default 50, max 200)
//
//   - q           string filename substring search (ILIKE)
//
//   - type_filter string file-extension suffix filter, e.g. ".md"
//
//   - indexed_only "true" restrict to COMPLETED documents
//
//     @Summary	List uploaded documents for a datapack (paginated)
//     @Tags		knowledge-bases
//     @Produce	json
//     @Param		name		path	string	true	"Datapack name"
//     @Param		page		query	int		false	"Page number (default 1)"
//     @Param		page_size	query	int		false	"Page size (default 50, max 200)"
//     @Param		q			query	string	false	"Filename substring search"
//     @Param		type_filter	query	string	false	"File extension suffix, e.g. .md"
//     @Param		indexed_only query	bool	false	"Restrict to fully indexed documents"
//     @Success	200			{object}	APIResponse
//     @Failure	401			{object}	APIResponse
//     @Failure	500			{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{name}/knowledge-bases/documents [get]
func (h *KnowledgeBaseHandler) Documents(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	const defaultPageSize = 50
	const maxPageSize = 200

	page := 1
	if v := strings.TrimSpace(c.QueryParam("page")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			page = n
		}
	}
	pageSize := defaultPageSize
	if v := strings.TrimSpace(c.QueryParam("page_size")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			if n > maxPageSize {
				n = maxPageSize
			}
			pageSize = n
		}
	}

	indexedOnly := strings.EqualFold(strings.TrimSpace(c.QueryParam("indexed_only")), "true")

	opts := repository.DocumentListOpts{
		Limit:       pageSize,
		Offset:      (page - 1) * pageSize,
		SearchQuery: strings.TrimSpace(c.QueryParam("q")),
		TypeFilter:  strings.TrimSpace(c.QueryParam("type_filter")),
		IndexedOnly: indexedOnly,
	}

	result, err := h.knowledgeBaseSvc.ListDistRagDocuments(dp, opts)
	if err != nil {
		slog.Error("failed to list knowledge base documents",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to list knowledge base documents")
	}
	return ok(c, http.StatusOK, result)
}

// Summary handles GET /api/v1/datapacks/:name/knowledge-bases/summary.
//
// Returns the header tile aggregates + the two deterministic description
// strings consumed by the SharedKnowledge page. One round trip from the
// UI's perspective; internally backed by three cheap repo queries.
//
//	@Summary	Header aggregates for the SharedKnowledge page
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/summary [get]
func (h *KnowledgeBaseHandler) Summary(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}
	summary, err := h.knowledgeBaseSvc.GetKnowledgeBaseSummary(dp)
	if err != nil {
		slog.Error("failed to load knowledge base summary",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to load knowledge base summary")
	}
	return ok(c, http.StatusOK, summary)
}

// ActivityLog handles GET /api/v1/datapacks/:name/knowledge-bases/activity-log.
//
// Merged feed of file uploads + memory promotions for this datapack,
// ordered by timestamp DESC. **Keyset pagination**: first call omits
// `cursor`; subsequent calls pass the `next_cursor` from the previous
// response verbatim. Cost is constant per page regardless of depth.
//
//	@Summary	Merged activity feed (file uploads + memory promotions)
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name		path	string	true	"Datapack name"
//	@Param		limit		query	int		false	"Page size (default 10, max 50)"
//	@Param		cursor		query	string	false	"Opaque next-page cursor from a previous call; omit for first page"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{name}/knowledge-bases/activity-log [get]
func (h *KnowledgeBaseHandler) ActivityLog(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	// Cap limit defensively so a malicious / misconfigured caller
	// can't ask for the entire feed in one shot. 50 is comfortably
	// above the UI's "Show more loads 10" cadence.
	limit := 10
	if v := strings.TrimSpace(c.QueryParam("limit")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = n
		}
	}
	cursor := strings.TrimSpace(c.QueryParam("cursor"))

	resp, err := h.knowledgeBaseSvc.ListActivityLog(dp, limit, cursor)
	if err != nil {
		slog.Error("failed to list activity log",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to list activity log")
	}
	return ok(c, http.StatusOK, resp)
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
