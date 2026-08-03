package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gabriel-vasile/mimetype"
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

// Add handles POST /api/v1/datapacks/:datapack_id/knowledge-bases.
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
//	@Param		datapack_id	path		string				true	"Datapack ID"
//	@Param		body	body		service.AddRequest	true	"Knowledge base URL"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases [post]
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

// List handles GET /api/v1/datapacks/:datapack_id/knowledge-bases.
//
//	@Summary	List knowledge bases for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases [get]
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

// Delete handles DELETE /api/v1/datapacks/:datapack_id/knowledge-bases/:rag_source_id.
//
//	@Summary	Delete a knowledge base
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id			path		string	true	"Datapack ID"
//	@Param		rag_source_id	path		string	true	"rag_source_id (UUID)"
//	@Success	200				{object}	APIResponse
//	@Failure	400				{object}	APIResponse
//	@Failure	401				{object}	APIResponse
//	@Failure	404				{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/{rag_source_id} [delete]
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

// RequestUploadURL handles POST /api/v1/datapacks/:datapack_id/knowledge-bases/upload-url.
//
//	@Summary	Generate a presigned S3 upload URL
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string				true	"Datapack ID"
//	@Param		body	body		uploadURLRequest	true	"Upload request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/upload-url [post]
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
	if uploadMimeByExt[ext] == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			"unsupported file type: "+ext+"; allowed: "+allowedExtensionsMsg)
	}
	// `size <= 0` is rejected explicitly: otherwise a negative payload
	// could wrap the int64 free-tier-cap arithmetic below and let an
	// uncapped upload through, and a zero size is a clear client bug.
	if req.Size <= 0 {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "size must be greater than 0")
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	// Per-file size cap + total-storage cap. Both derived from
	// LimitsFor(user_type):
	//   free      -> 5 MB   / 100 MB total
	//   standard  -> 50 MB  / 5 GB total   (Marketplace-paid)
	//   pro/etc.  -> TierUnlimited on both
	// Gated on the REQUESTER's tier (not the owner's) to stay
	// consistent with the FE — a Pro user uploading into a shared
	// Free-owner datapack sees no cap on either side. Falls back to
	// the legacy 5 MB per-file cap when there's no account context.
	account, _ := auth.GetAccount(c)
	perFileCap := int64(maxFileSizeBytes)
	if account != nil {
		if l := service.LimitsFor(account.AccountType).MaxKBFileBytes; l > 0 {
			perFileCap = l
		}
	}
	if req.Size > perFileCap {
		userType := ""
		if account != nil {
			userType = account.AccountType
		}
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", service.UpgradeMessageFileSize(userType))
	}

	if account != nil && service.IsGatedUserType(account.AccountType) {
		limits := service.LimitsFor(account.AccountType)
		if currentBytes, err := h.knowledgeBaseSvc.SumDocumentSizesByOwner(dp.UserID); err != nil {
			slog.Error("kb upload cap: sum sizes",
				"user_id", dp.UserID, "datapack_id", dp.ID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to read tier storage usage")
		} else if currentBytes+req.Size > limits.MaxKBStorageBytes {
			slog.Info("kb upload blocked by tier cap",
				"user_id", dp.UserID,
				"user_type", account.AccountType,
				"current_bytes", currentBytes,
				"incoming_bytes", req.Size,
				"cap", limits.MaxKBStorageBytes,
			)
			return apiError(c, http.StatusPaymentRequired,
				"FREE_TIER_KB_STORAGE_LIMIT", service.FreeTierUpgradeMessageKBUpload)
		}
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

// RequestDownloadURL handles POST /api/v1/datapacks/:datapack_id/knowledge-bases/download-url.
//
// Returns a short-lived presigned S3 GET URL for the given document.
// The UI fetches this on click and opens it directly — no api_server
// in the file path. Mirrors the upload-URL flow.
//
//	@Summary	Presigned S3 GET URL for a knowledge-base document
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string				true	"Datapack ID"
//	@Param		body	body		downloadURLRequest	true	"Document download request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	403		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/download-url [post]
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
	if models.IsOwnerOrMaintainerGrant(dpGrant) {
		return true
	}
	if fileUploaderUserID != nil &&
		strings.TrimSpace(*fileUploaderUserID) != "" &&
		strings.EqualFold(*fileUploaderUserID, callerUserID) {
		return true
	}
	return false
}

// DeleteDocument handles DELETE /api/v1/datapacks/:datapack_id/knowledge-bases/documents/:document_id.
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
//     @Param		datapack_id		path	string	true	"Datapack ID"
//     @Param		document_id	path	string	true	"dist_rag document UUID"
//     @Success	200		{object}	APIResponse
//     @Failure	400		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	403		{object}	APIResponse
//     @Failure	404		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{datapack_id}/knowledge-bases/documents/{document_id} [delete]
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

// ConfirmUpload handles POST /api/v1/datapacks/:datapack_id/knowledge-bases/upload-complete.
//
// This endpoint is currently advisory: the actual KB row is created by
// the subsequent /knowledge-bases/create call which lists the S3 prefix
// and registers all uploaded files with dist_rag.
//
//	@Summary	Confirm a completed file upload
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string					true	"Datapack ID"
//	@Param		body	body		uploadCompleteRequest	true	"Upload confirmation"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/upload-complete [post]
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

// resolveOrCreateSource returns the dist_rag source_id bound to a datapack's
// knowledge base, creating and indexing a new source when none exists.
// isNewSource is true only when the source was created in this call, which
// lets callers pick WaitAndBuild (new) over BuildIndex (already discoverable).
func (h *KnowledgeBaseHandler) resolveOrCreateSource(
	dp *models.Datapack, sourceURI string,
) (sourceID string, isNewSource bool, err error) {
	if dp == nil {
		return "", false, fmt.Errorf("datapack is required")
	}
	if h.ragSvc == nil {
		return "", false, fmt.Errorf("rag service is not configured")
	}

	// A cached mapping whose dist_rag source has since been deleted is stale;
	// drop it and fall through to recreate.
	cachedSourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dp)
	if err != nil {
		return "", false, fmt.Errorf("check cached sources: %w", err)
	}
	if len(cachedSourceIDs) > 0 {
		sourceID = cachedSourceIDs[0]
		exists, existsErr := h.ragSvc.SourceExists(sourceID)
		if existsErr != nil {
			return "", false, fmt.Errorf("validate cached source: %w", existsErr)
		}
		if !exists {
			slog.Warn("stale KB source cache, clearing",
				"source_id", sourceID, "datapack_id", dp.ID)
			if derr := h.knowledgeBaseSvc.DeleteByDatapack(dp); derr != nil {
				slog.Error("failed to clear stale KB source cache",
					"datapack_id", dp.ID, "error", derr)
			}
			sourceID = ""
		}
	}
	if sourceID != "" {
		return sourceID, false, nil
	}

	// The source may already exist in dist_rag without a cached mapping (e.g.
	// created out of band); adopt it and persist the mapping.
	sourceID, err = h.ragSvc.LookupSourceByURI(sourceURI)
	if err != nil {
		return "", false, fmt.Errorf("lookup source: %w", err)
	}
	if sourceID != "" {
		if err := h.knowledgeBaseSvc.InsertWithID(dp.ID, sourceID, sourceURI, nil); err != nil {
			return "", false, fmt.Errorf("save knowledge base: %w", err)
		}
		return sourceID, false, nil
	}

	sourceID, err = h.ragSvc.CreateSource(sourceURI, dp.ID)
	if err != nil {
		return "", false, fmt.Errorf("create source: %w", err)
	}
	if err := h.ragSvc.AddSourceToIndex(sourceID); err != nil {
		return "", false, fmt.Errorf("add source to index: %w", err)
	}
	if err := h.knowledgeBaseSvc.InsertWithID(dp.ID, sourceID, sourceURI, nil); err != nil {
		return "", false, fmt.Errorf("save knowledge base: %w", err)
	}
	return sourceID, true, nil
}

// CreateKB handles POST /api/v1/datapacks/:datapack_id/knowledge-bases/create.
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
//     @Param		datapack_id	path		string	true	"Datapack ID"
//     @Success	201		{object}	APIResponse
//     @Failure	400		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Failure	503		{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{datapack_id}/knowledge-bases/create [post]
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

	sourceID, isNewSource, err := h.resolveOrCreateSource(dp, sourceURI)
	if err != nil {
		slog.Error("failed to resolve knowledge base source",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to resolve knowledge base source")
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

// uploadFormFileField is the multipart/form-data field name the file is
// sent under. Matches the convention used by OpenAI's and Anthropic's
// `POST /v1/files` endpoints (`-F file=@...`) so agent/CLI callers can use
// the same muscle memory.
const uploadFormFileField = "file"

// uploadUserLocks serializes the check-then-act section of UploadFile (dedup
// check -> free-tier cap check -> S3 upload -> InsertDocument) per user_id, so
// concurrent uploads can't both read a stale "current usage" and jointly
// exceed FreeTierMaxKBStorageBytes, or both pass the dedup check for the same
// content. E.g. at 99 MiB used against a 100 MiB cap, two concurrent 512KB and
// 513KB uploads each individually check out fine against the same 99 MiB
// snapshot, landing ~1KB over cap. Per-user rather than global so unrelated
// users aren't serialized against each other.
var (
	uploadUserLocksMu sync.Mutex
	uploadUserLocks   = map[string]*sync.Mutex{}
)

// lockForUploadUser returns the per-user mutex used to serialize UploadFile's
// dedup/cap-check/upload/insert critical section, creating one on first use.
func lockForUploadUser(userID string) *sync.Mutex {
	uploadUserLocksMu.Lock()
	defer uploadUserLocksMu.Unlock()
	userLevelIndividualMutex, ok := uploadUserLocks[userID]
	if !ok {
		userLevelIndividualMutex = &sync.Mutex{}
		uploadUserLocks[userID] = userLevelIndividualMutex
	}
	return userLevelIndividualMutex
}

// uploadFormSourceField is the multipart/form-data field name that selects
// where the file's bytes come from. It's a forward-looking discriminator:
// today only "local" (client-pushed multipart bytes) is supported, but the
// contract is stable so we can later add out-of-band fetch modes ("url",
// "gdrive", …) without changing the endpoint shape. Keeping fetch modes
// behind an explicit value also isolates them as a distinct security surface
// (server-side URL fetch would introduce SSRF risk that a client-pushed
// upload does not).
const uploadFormSourceField = "source"

// UploadSource enumerates the accepted values of the `source` form field.
type UploadSource string

// UploadSourceLocal is a client-pushed file: the bytes arrive inline in the
// multipart request (local filesystem, generated artifact, etc.). It is the
// only supported source today, and the default when `source` is omitted.
// In the future, we can add support for fetching files from URLs.
const UploadSourceLocal UploadSource = "local"

// multipartOverheadBytes is slack added over the raw file-size cap when
// bounding the multipart request body, to cover the boundary markers and
// per-part headers that wrap the file bytes.
const multipartOverheadBytes = 8 * 1024

// uploadMimeByExt maps accepted upload extensions to their MIME type. It is the
// single source of truth for what the KB upload paths accept, mirroring the
// pipeline's SUPPORTED_DOCUMENT_TYPES (yugabyte-db/python/ai/.../rag_pipeline/
// document_types.py); unsupported types (e.g. video/mp4) are omitted so a file
// can't sit QUEUED forever. Everything below derives from this map.
var uploadMimeByExt = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".text": "text/plain",
	".md":   "text/markdown",
	".json": "application/json",
	".csv":  "text/csv",
	".xml":  "text/xml",
	".html": "text/html",
	".htm":  "text/html",
}

// allowedExtensionsMsg is the sorted, comma-joined accept-list shown in
// "unsupported file" errors.
var allowedExtensionsMsg = func() string {
	exts := make([]string, 0, len(uploadMimeByExt))
	for ext := range uploadMimeByExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return strings.Join(exts, ", ")
}()

// resolveUploadMIME verifies the file's bytes match its (already-accepted)
// extension and returns the MIME type to store, or ok=false to reject a renamed
// / spoofed file. The extension gives the expected type; the content must
// corroborate it. mtype comes from mimetype.DetectReader, which only reads the
// leading header (its default limit), not the whole file — see
// https://pkg.go.dev/github.com/gabriel-vasile/mimetype#DetectReader
//
// When mimetype positively identifies the content (PDF, HTML, XML, JSON), it
// must equal what the extension claims — e.g. HTML saved as .txt, or a video
// saved as .pdf, is a mismatch and is rejected. The textual fallback below
// applies when the extension itself claims a type with no reliable signature
// (text/plain, text/markdown, or text/csv, i.e. .txt/.text/.md/.csv) — those
// can't be verified any other way. For a signature-bearing extension
// (.pdf/.json/.xml/.html/.htm), content that isn't positively identified as
// that type is a mismatch and is rejected, even if it happens to be textual
// (e.g. plain text renamed to report.pdf) — otherwise the fallback would
// defeat the corroboration this function exists to perform for exactly those
// extensions.
//
// text/csv is deliberately NOT in the strict/distinctive bucket even though
// mimetype can positively identify it: its detector is a fragile RFC4180
// heuristic (see https://github.com/gabriel-vasile/mimetype/issues/138), not
// a real signature — ordinary multi-line prose where every line happens to
// contain the same number of commas (e.g. "Hi Krishna, thanks.\nBye now, Krishna.")
// gets misidentified as text/csv and would otherwise be hard-rejected as
// "spoofed". Falling through to the textual check still blocks a genuinely
// binary payload (image, PDF, ...) renamed to .csv, which is all the
// corroboration a delimited-text format can reliably offer.
func resolveUploadMIME(mtype *mimetype.MIME, ext string) (contentType, detected string, ok bool) {
	expected := uploadMimeByExt[ext]
	detected = mtype.String()
	if expected == "" {
		return "", detected, false
	}
	if identifiedType, identified := distinctiveMIME(mtype); identified {
		if identifiedType != expected {
			return "", detected, false
		}
		return expected, detected, true
	}
	if expected != "text/plain" && expected != "text/markdown" && expected != "text/csv" {
		return "", detected, false
	}
	if !mimeIsTextual(mtype) {
		return "", detected, false
	}
	return expected, detected, true
}

// distinctiveMIME returns the supported MIME type mimetype positively detected
// from the content, if any. text/plain, text/markdown, and text/csv are
// intentionally excluded: text/plain and text/markdown carry no signature at
// all (markdown is indistinguishable from plain text), and text/csv's
// "signature" is a fragile line-shape heuristic prone to false positives on
// ordinary prose (see resolveUploadMIME) — none of the three can be verified
// against the extension this strictly, so they fall through to the textual
// check instead.
func distinctiveMIME(m *mimetype.MIME) (string, bool) {
	switch {
	case m.Is("application/pdf"):
		return "application/pdf", true
	case m.Is("text/html"):
		return "text/html", true
	case m.Is("text/xml"):
		return "text/xml", true
	case m.Is("application/json"):
		return "application/json", true
	}
	return "", false
}

// mimeIsTextual reports whether m sits under text/plain in mimetype's type
// hierarchy (i.e. the content is genuinely text, not binary).
func mimeIsTextual(m *mimetype.MIME) bool {
	for ; m != nil; m = m.Parent() {
		if m.Is("text/plain") {
			return true
		}
	}
	return false
}

// UploadFile handles POST /api/v1/datapacks/:datapack_id/knowledge-bases/upload.
//
// Single-call alternative to the presigned upload-url -> PUT -> upload-complete
// -> create browser flow: the caller sends the file as multipart/form-data
// (field name "file", capped at maxFileSizeBytes = 5MB) and this handler
// uploads to S3, registers the document with dist_rag, and triggers indexing
// — all in one round trip. Built for programmatic / agentic callers (PAT
// bearer tokens) that shouldn't have to orchestrate the multi-step browser
// flow or reach S3 directly. The multipart contract mirrors OpenAI's and
// Anthropic's `POST /v1/files` (`-F file=@...`).
//
// The optional `source` form field selects where the bytes come from:
// "local" (default) is a client-pushed multipart file. It's a forward-looking
// discriminator — future values like "url" will let the server fetch from a
// public link (Google Drive, HTTPS, …) without changing the endpoint shape.
//
//	@Summary	Upload a file directly to a datapack's knowledge base
//	@Tags		knowledge-bases
//	@Accept		multipart/form-data
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Param		source		formData	string	false	"Byte source: 'local' (default). Reserved: 'url'."
//	@Param		file		formData	file	true	"File to upload (pdf, txt, md, json, csv, xml, html) — required when source=local"
//	@Success	202		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	402		{object}	APIResponse
//	@Failure	413		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/upload [post]
func (h *KnowledgeBaseHandler) UploadFile(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}
	if h.ragSvc == nil {
		return apiError(c, http.StatusServiceUnavailable,
			"RAG_DISABLED", "rag service is not configured")
	}

	// Reject oversized uploads on the declared Content-Length before any
	// multipart parsing, so the 5 MB cap holds for every caller regardless of
	// whether an ingress/proxy enforces a body-size limit. With
	// Expect: 100-continue (curl sends it for larger bodies) the client never
	// transmits the payload. The MaxBytesReader below still enforces the cap
	// when Content-Length is absent or under-reports.
	if c.Request().ContentLength > maxFileSizeBytes+multipartOverheadBytes {
		return apiError(c, http.StatusRequestEntityTooLarge,
			"VALIDATION_ERROR", service.FreeTierUpgradeMessageFileSize)
	}

	// Cap the raw body before anything can trigger multipart parsing. Both
	// FormValue and FormFile call ParseMultipartForm, which drains the entire
	// body and spills whatever exceeds its in-memory budget to temp files — so
	// the cap has to precede the first form access, not just the file read. A
	// chunked upload declares no Content-Length (ContentLength == -1), which
	// slips past the check above, leaving this as its only bound. Small slack
	// over the file cap covers the multipart framing (boundary + part headers).
	c.Request().Body = http.MaxBytesReader(
		c.Response().Writer, c.Request().Body, maxFileSizeBytes+multipartOverheadBytes,
	)

	// Resolve the byte source up front. Empty defaults to "local"; anything
	// other than "local" is rejected. Future fetch modes ("url", …) will add
	// their own cases here without changing the endpoint shape.
	source := UploadSource(strings.ToLower(strings.TrimSpace(c.FormValue(uploadFormSourceField))))
	if source == "" {
		source = UploadSourceLocal
	}
	if source != UploadSourceLocal {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			`unsupported source: "`+string(source)+`"; allowed: local`)
	}

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

	fileHeader, err := c.FormFile(uploadFormFileField)
	if err != nil {
		// FormValue above discards parse errors, so a body that tripped the cap
		// first surfaces here; report it as 413 rather than a missing-field 400.
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return apiError(c, http.StatusRequestEntityTooLarge,
				"VALIDATION_ERROR", service.FreeTierUpgradeMessageFileSize)
		}
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", `multipart form field "file" is required`)
	}

	filename := filepath.Base(strings.TrimSpace(fileHeader.Filename))
	if filename == "" || filename == "." {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "filename is required")
	}
	// Gate on the extension first (cheap reject before reading the body). The
	// bytes are re-checked against it below (resolveUploadMIME) so a renamed
	// file — content that doesn't match its extension — is rejected too.
	ext := strings.ToLower(filepath.Ext(filename))
	if uploadMimeByExt[ext] == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			"unsupported file type: "+ext+"; allowed: "+allowedExtensionsMsg)
	}
	// Reject on the declared part size before reading any bytes; the decoded
	// length is re-checked below in case the header under-reports.
	if fileHeader.Size > maxFileSizeBytes {
		return apiError(c, http.StatusRequestEntityTooLarge,
			"VALIDATION_ERROR", service.FreeTierUpgradeMessageFileSize)
	}

	src, err := fileHeader.Open()
	if err != nil {
		slog.Error("UploadFile: failed to open multipart file",
			"datapack_id", dp.ID, "filename", filename, "error", err)
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "failed to read uploaded file")
	}
	defer src.Close()

	// Validate the content type before buffering the whole body. DetectReader
	// reads only the header (not the whole file); TeeReader captures those bytes
	// so io.MultiReader can hand them back for the full read below.
	// https://pkg.go.dev/github.com/gabriel-vasile/mimetype#DetectReader
	header := &bytes.Buffer{}
	mtype, err := mimetype.DetectReader(io.TeeReader(src, header))
	if err != nil {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "failed to read uploaded file")
	}
	if header.Len() == 0 {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "uploaded file is empty")
	}

	// The extension is already accepted (gated above); the bytes must
	// corroborate it, so a renamed / spoofed file is rejected here.
	contentType, detected, supported := resolveUploadMIME(mtype, ext)
	if !supported {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR",
			fmt.Sprintf("file content does not match its %s extension: "+
				"expected %s but the content is actually %s",
				ext, uploadMimeByExt[ext], detected))
	}

	// Read one byte past the cap so a header that under-reports Size can't
	// smuggle an oversized body through. header holds the bytes DetectReader
	// already consumed, so prepend it to the remaining stream.
	body := io.MultiReader(header, src)
	data, err := io.ReadAll(io.LimitReader(body, maxFileSizeBytes+1))
	if err != nil {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "failed to read uploaded file")
	}
	if int64(len(data)) > maxFileSizeBytes {
		return apiError(c, http.StatusRequestEntityTooLarge,
			"VALIDATION_ERROR", service.FreeTierUpgradeMessageFileSize)
	}

	contentHash := sha256.Sum256(data)
	contentHashHex := hex.EncodeToString(contentHash[:])
	ctx := c.Request().Context()

	// Serialize per-user from here through InsertDocument: this whole section
	// is a check-then-act sequence (dedup check, free-tier cap check, S3
	// upload, document registration) that must be atomic per user, or
	// concurrent requests can both pass the dedup check for the same content,
	// or jointly blow through the free-tier storage cap. See
	// lockForUploadUser's doc comment.
	userLock := lockForUploadUser(dp.UserID)
	userLock.Lock()
	defer userLock.Unlock()

	// Content-hash dedup (identity is the bytes, not the filename), same rule as
	// the presigned flow. If the same content already exists we don't re-upload
	// or re-charge storage: report whether it is already processed or still
	// being processed. An S3 object that was never registered in dist_rag
	// (orphan), or one whose pipeline run FAILED, falls through and gets
	// re-registered/re-indexed below, reusing its key — InsertDocument is an
	// upsert and the pipeline only skips PROCESSING/COMPLETED rows, so FAILED
	// is retried rather than reported as still-processing.
	existingKey, err := h.s3Svc.ExistingUploadKeyByHash(ctx, dp.UserID, dp.ID, contentHashHex)
	if err != nil {
		slog.Error("UploadFile: dedup check failed",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"S3_ERROR", "failed to check existing upload")
	}
	if existingKey != "" {
		if detail, found := h.documentDetail(dp, existingKey); found && detail.DocumentStatus != "FAILED" {
			status, message := "already_uploaded",
				"This file was already uploaded and is being processed."
			if detail.DocumentStatus == "COMPLETED" {
				status, message = "already_processed",
					"This file is already processed in the knowledge base."
			}
			statusURL := fmt.Sprintf(
				"%s/api/v1/datapacks/%s/knowledge-bases/document-status?document_key=%s",
				requestOrigin(c), dp.ID, url.QueryEscape(existingKey))
			return ok(c, http.StatusOK, map[string]any{
				"datapack_id":  dp.ID,
				"document_key": existingKey,
				"filename":     path.Base(existingKey),
				"status":       status,
				"message":      message,
				"status_url":   statusURL,
			})
		}
	}

	key := existingKey
	if key == "" {
		// New content: enforce the free-tier storage cap (same as the presigned
		// path, gated on the REQUESTER's tier), then upload.
		if service.IsFreeUserType(acct.AccountType) {
			currentBytes, sumErr := h.knowledgeBaseSvc.SumDocumentSizesByOwner(dp.UserID)
			if sumErr != nil {
				slog.Error("UploadFile: free-tier cap check failed",
					"user_id", dp.UserID, "datapack_id", dp.ID, "error", sumErr)
				return apiError(c, http.StatusInternalServerError,
					"INTERNAL_ERROR", "failed to read free-tier storage usage")
			}
			if currentBytes+int64(len(data)) > service.FreeTierMaxKBStorageBytes {
				slog.Info("UploadFile: blocked by free-tier cap",
					"user_id", dp.UserID,
					"current_bytes", currentBytes,
					"incoming_bytes", len(data),
					"cap", service.FreeTierMaxKBStorageBytes,
				)
				return apiError(c, http.StatusPaymentRequired,
					"FREE_TIER_KB_STORAGE_LIMIT", service.FreeTierUpgradeMessageKBUpload)
			}
		}
		key, err = h.s3Svc.UploadObject(ctx, dp.UserID, dp.ID, contentHashHex, filename, contentType, data)
		if err != nil {
			slog.Error("UploadFile: S3 upload failed",
				"datapack_id", dp.ID, "filename", filename, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"S3_ERROR", "failed to upload file")
		}
	}

	if err := h.ragSvc.EnsureVectorIndex(); err != nil {
		slog.Error("UploadFile: failed to ensure vector index",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to initialize vector index")
	}

	sourceURI := h.knowledgeBaseSvc.BuildDatapackPrefix(dp.UserID, dp.ID)
	sourceID, isNewSource, err := h.resolveOrCreateSource(dp, sourceURI)
	if err != nil {
		slog.Error("UploadFile: failed to resolve knowledge base source",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to resolve knowledge base source")
	}

	docURI := fmt.Sprintf("s3://%s/%s", h.s3Svc.Bucket(), key)
	details := buildMekoDocDetails(acct.ID, int64(len(data)), time.Now())
	if err := h.ragSvc.InsertDocument(
		sourceID, key, docURI, contentHashHex, contentType, details,
	); err != nil {
		slog.Error("UploadFile: failed to insert document",
			"datapack_id", dp.ID, "document_uri", docURI, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to register document")
	}

	if isNewSource {
		go h.ragSvc.WaitAndBuild(sourceID)
	} else if err := h.ragSvc.BuildIndex(); err != nil {
		slog.Error("UploadFile: failed to trigger build_index",
			"datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to build index")
	}

	// The s3:// URI and dist_rag source id stay in the logs for
	// support/debugging rather than in the API response.
	slog.Info("knowledge base file uploaded via direct API",
		"user_id", dp.UserID,
		"datapack_id", dp.ID,
		"filename", filename,
		"key", key,
		"document_uri", docURI,
		"size", len(data),
		"rag_source_id", sourceID,
		"is_new_source", isNewSource,
		"by_user_id", acct.ID,
	)

	// Absolute status URL (also in the Location header) for the 202 response,
	// scoped to this file via ?document_key= so the caller polls just their
	// upload. requestOrigin honors X-Forwarded-Proto/Host so it's correct behind
	// the ingress.
	statusURL := fmt.Sprintf("%s/api/v1/datapacks/%s/knowledge-bases/document-status?document_key=%s",
		requestOrigin(c), dp.ID, url.QueryEscape(key))
	c.Response().Header().Set("Location", statusURL)

	return ok(c, http.StatusAccepted, map[string]any{
		"datapack_id":  dp.ID,
		"document_key": key,
		"filename":     filename,
		"size":         len(data),
		"status":       "processing",
		"status_url":   statusURL,
	})
}

// KBStatus handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/status.
//
//	@Summary	Get knowledge base pipeline status
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Failure	503		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/status [get]
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

// KBDocumentStatus handles
// GET /api/v1/datapacks/:datapack_id/knowledge-bases/document-status?key=<key>.
//
// Returns the pipeline detail for a single uploaded document, identified by the
// S3 key returned from the direct upload response, instead of the whole
// datapack's pipeline. Returns 404 until the document appears in dist_rag.
//
//	@Summary	Get pipeline status for a single uploaded document
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id		path		string	true	"Datapack ID"
//	@Param		document_key	query		string	true	"Document key (from the upload response)"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/document-status [get]
func (h *KnowledgeBaseHandler) KBDocumentStatus(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}
	documentKey := c.QueryParam("document_key")
	if documentKey == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "document_key query parameter is required")
	}
	if detail, found := h.documentDetail(dp, documentKey); found {
		return ok(c, http.StatusOK, detail)
	}
	return apiError(c, http.StatusNotFound, "NOT_FOUND", "document not found for document_key")
}

// documentDetail returns the dist_rag pipeline detail for a single document key
// within a datapack, if it is registered. Reused by KBDocumentStatus and the
// direct upload dedup check.
func (h *KnowledgeBaseHandler) documentDetail(
	dp *models.Datapack, documentKey string,
) (repository.PipelineDetail, bool) {
	if h.ragSvc == nil {
		return repository.PipelineDetail{}, false
	}
	sourceURIs, err := h.knowledgeBaseSvc.GetSourceURIs(dp)
	if err != nil || len(sourceURIs) == 0 {
		return repository.PipelineDetail{}, false
	}
	detail, err := h.ragSvc.GetDocumentStatus(sourceURIs, documentKey)
	if err != nil {
		slog.Error("documentDetail: failed to get document status",
			"datapack_id", dp.ID, "document_key", documentKey, "error", err)
		return repository.PipelineDetail{}, false
	}
	if detail == nil {
		return repository.PipelineDetail{}, false
	}
	return *detail, true
}

// Counts handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/counts.
//
// Returns the per-datapack breakdown of total uploaded documents,
// total stored chunks, and total promoted memories.
//
//	@Summary	Knowledge base counts breakdown for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/counts [get]
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

// Documents handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/documents.
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
//     @Param		datapack_id		path	string	true	"Datapack ID"
//     @Param		page		query	int		false	"Page number (default 1)"
//     @Param		page_size	query	int		false	"Page size (default 50, max 200)"
//     @Param		q			query	string	false	"Filename substring search"
//     @Param		type_filter	query	string	false	"File extension suffix, e.g. .md"
//     @Param		indexed_only query	bool	false	"Restrict to fully indexed documents"
//     @Success	200			{object}	APIResponse
//     @Failure	401			{object}	APIResponse
//     @Failure	500			{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{datapack_id}/knowledge-bases/documents [get]
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

// Summary handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/summary.
//
// Returns the header tile aggregates + the two deterministic description
// strings consumed by the SharedKnowledge page. One round trip from the
// UI's perspective; internally backed by three cheap repo queries.
//
//	@Summary	Header aggregates for the SharedKnowledge page
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/summary [get]
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

// PendingUploadItem describes one S3 object that was uploaded via the
// presigned-URL flow but has never been registered with dist_rag (no
// row exists in dist_rag.documents for it, in any status). Surfacing
// these lets the UI recover from the case where a user PUT a file to
// S3 but never clicked "Create / Add to Knowledge Base".
type PendingUploadItem struct {
	// Key is the full S3 key, e.g. "<user_id>/<datapack_id>/<sha>/<filename>".
	Key string `json:"key"`
	// Filename is the basename shown to the user.
	Filename string `json:"filename"`
	// Size is the S3 object size in bytes (0 = unknown).
	Size int64 `json:"size"`
	// UploadedAt is the S3 LastModified timestamp, RFC3339 UTC.
	UploadedAt string `json:"uploaded_at,omitempty"`
}

// PendingUploadsResponse is the envelope returned by /pending-uploads.
type PendingUploadsResponse struct {
	Items []PendingUploadItem `json:"items"`
	Count int                 `json:"count"`
}

// PendingUploads handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/pending-uploads.
//
// Returns S3 objects under the datapack's prefix that are NOT yet
// tracked in dist_rag.documents. These are files the user uploaded
// (via a presigned PUT) but never followed up with a Create/Add-to-KB
// call, so the ingestion pipeline has never seen them.
//
// Logic:
//
//  1. Resolve the datapack's rag_source_id values (may be zero — the
//     source is created lazily on first CreateKB).
//
//  2. List every S3 object under "<user_id>/<datapack_id>/".
//
//  3. If a source exists, load the set of URIs already known to
//     dist_rag.documents (any status).
//
//  4. Return the diff: S3 keys whose s3://bucket/key is not in that
//     set. When no source exists yet, every S3 object is pending.
//
//     @Summary	List S3 uploads not yet registered with dist_rag
//     @Tags		knowledge-bases
//     @Produce	json
//     @Param		datapack_id	path		string	true	"Datapack ID"
//     @Success	200		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Failure	503		{object}	APIResponse
//     @Security	BearerAuth
//     @Router		/datapacks/{datapack_id}/knowledge-bases/pending-uploads [get]
func (h *KnowledgeBaseHandler) PendingUploads(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	s3Prefix := fmt.Sprintf("%s/%s/", dp.UserID, dp.ID)
	objects, err := h.s3Svc.ListObjects(c.Request().Context(), s3Prefix)
	if err != nil {
		slog.Error("pending-uploads: failed to list S3 objects",
			"datapack_id", dp.ID, "prefix", s3Prefix, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"S3_ERROR", "failed to list uploaded files")
	}
	if len(objects) == 0 {
		return ok(c, http.StatusOK, PendingUploadsResponse{Items: []PendingUploadItem{}, Count: 0})
	}

	// existingURIs is the set of documents already known to dist_rag
	// (any status). Missing from this set = the pipeline has never
	// seen this file, i.e. it's an orphan awaiting a Create/Add-to-KB.
	// Union across ALL source_ids for the datapack — historically
	// each CreateKB call could produce a new source row, so orphans
	// must be tested against the full union, not just the first.
	existingURIs := make(map[string]bool)
	if h.ragSvc != nil {
		sourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dp)
		if err != nil {
			slog.Error("pending-uploads: failed to load source ids",
				"datapack_id", dp.ID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to check existing documents")
		}
		for _, sourceID := range sourceIDs {
			uris, err := h.ragSvc.GetExistingDocURIs(sourceID)
			if err != nil {
				slog.Error("pending-uploads: failed to load dist_rag URIs",
					"datapack_id", dp.ID, "source_id", sourceID, "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to check existing documents")
			}
			for uri := range uris {
				existingURIs[uri] = true
			}
		}
	}

	items := make([]PendingUploadItem, 0)
	for _, obj := range objects {
		docURI := fmt.Sprintf("s3://%s/%s", h.s3Svc.Bucket(), obj.Key)
		if existingURIs[docURI] {
			continue
		}
		var uploadedAt string
		if !obj.UploadedAt.IsZero() {
			uploadedAt = obj.UploadedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, PendingUploadItem{
			Key:        obj.Key,
			Filename:   filepath.Base(obj.Key),
			Size:       obj.Size,
			UploadedAt: uploadedAt,
		})
	}

	return ok(c, http.StatusOK, PendingUploadsResponse{Items: items, Count: len(items)})
}

// DeletePendingUpload handles DELETE /api/v1/datapacks/:datapack_id/knowledge-bases/pending-uploads?key=<encoded>.
//
// Removes a single S3 orphan — nothing else exists for it (no dist_rag
// row, no chunks), so this is an S3 DeleteObject and nothing more. The
// prefix check is the only real safety bit: it stops a caller from
// reaching outside their own datapack's key space.
func (h *KnowledgeBaseHandler) DeletePendingUpload(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}
	key := strings.TrimSpace(c.QueryParam("key"))
	if key == "" {
		return apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "key is required")
	}
	if !strings.HasPrefix(key, fmt.Sprintf("%s/%s/", dp.UserID, dp.ID)) {
		return apiError(c, http.StatusForbidden, "FORBIDDEN", "key does not belong to this datapack")
	}
	if err := h.s3Svc.DeleteObject(c.Request().Context(), key); err != nil {
		slog.Error("pending upload delete: S3 failed",
			"datapack_id", dp.ID, "key", key, "error", err)
		return apiError(c, http.StatusInternalServerError, "S3_ERROR", "failed to delete file")
	}
	slog.Info("pending upload deleted", "datapack_id", dp.ID, "key", key)
	return ok(c, http.StatusOK, map[string]string{"key": key})
}

// ActivityLog handles GET /api/v1/datapacks/:datapack_id/knowledge-bases/activity-log.
//
// Merged feed of file uploads + memory promotions for this datapack,
// ordered by timestamp DESC. **Keyset pagination**: first call omits
// `cursor`; subsequent calls pass the `next_cursor` from the previous
// response verbatim. Cost is constant per page regardless of depth.
//
//	@Summary	Merged activity feed (file uploads + memory promotions)
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		datapack_id		path	string	true	"Datapack ID"
//	@Param		limit		query	int		false	"Page size (default 10, max 50)"
//	@Param		cursor		query	string	false	"Opaque next-page cursor from a previous call; omit for first page"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/knowledge-bases/activity-log [get]
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
