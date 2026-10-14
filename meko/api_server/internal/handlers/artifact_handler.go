package handlers

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

const defaultMaxArtifactSizeBytes int64 = 5 * 1024 * 1024 // 5 MiB

// ArtifactHandler serves the two artifact endpoints:
//
//	POST /api/v1/datapacks/:datapack_id/artifacts
//	GET  /api/v1/datapacks/:datapack_id/artifacts/:content_hash
type ArtifactHandler struct {
	svc                  *service.ArtifactService
	maxArtifactSizeBytes int64
}

// NewArtifactHandler creates a new ArtifactHandler. maxSizeBytes sets the
// decoded file size limit for PUT; 0 falls back to defaultMaxArtifactSizeBytes.
func NewArtifactHandler(svc *service.ArtifactService, maxSizeBytes int64) *ArtifactHandler {
	if maxSizeBytes <= 0 {
		maxSizeBytes = defaultMaxArtifactSizeBytes
	}
	return &ArtifactHandler{svc: svc, maxArtifactSizeBytes: maxSizeBytes}
}

// artifactPutRequest is the JSON body for PUT.
type artifactPutRequest struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"` // base64-encoded file bytes
}

// artifactPutResponse is the JSON body returned on success.
type artifactPutResponse struct {
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	StoredIn    string `json:"stored_in"` // "db" or "s3"
}

// artifactGetResponse is the JSON body returned on GET.
type artifactGetResponse struct {
	ArtifactID         string `json:"artifact_id"`
	ContentHash        string `json:"content_hash"`
	Filename           string `json:"filename"`
	ContentType        string `json:"content_type"`
	SizeBytes          int64  `json:"size_bytes"`
	StoredIn           string `json:"stored_in"`
	ContentBase64      string `json:"content_base64,omitempty"`       // inline (db) artifacts
	ContentBase64Large string `json:"content_base64_large,omitempty"` // S3 artifacts returned as base64
}

// Put handles POST /api/v1/datapacks/:datapack_id/artifacts.
//
//	@Summary	Upload a file artifact to a datapack
//	@Tags		artifacts
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string				true	"Datapack ID"
//	@Param		body		body		artifactPutRequest	true	"Artifact upload"
//	@Success	201			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	403			{object}	APIResponse	"Viewers can't upload artifacts"
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/artifacts [post]
func (h *ArtifactHandler) Put(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}
	// An upload writes to the shared datapack, and a viewer is read-only;
	// the only writes a viewer keeps are deletes of its own data.
	if models.IsViewerGrant(dp.Grant) {
		return apiError(c, http.StatusForbidden, "FORBIDDEN", "viewers have read-only access to this datapack")
	}
	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "account context missing")
	}

	// Limit body reads to 2× maxArtifactSizeBytes (base64 overhead) to reject
	// oversized requests before unmarshalling.
	maxBodyBytes := h.maxArtifactSizeBytes * 2
	c.Request().Body = http.MaxBytesReader(c.Response().Writer, c.Request().Body, maxBodyBytes)

	var req artifactPutRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if err := validateArtifactPutRequest(req); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	}

	data, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_BASE64", "content_base64 is not valid base64")
	}
	if int64(len(data)) > h.maxArtifactSizeBytes {
		return apiError(c, http.StatusRequestEntityTooLarge, "ARTIFACT_TOO_LARGE",
			fmt.Sprintf("file size %d bytes exceeds limit of %d bytes", len(data), h.maxArtifactSizeBytes))
	}

	artifact, err := h.svc.Put(c.Request().Context(), service.ArtifactPutRequest{
		UserID:      account.ID,
		DatapackID:  dp.ID,
		Filename:    req.Filename,
		ContentType: req.ContentType,
		Data:        data,
	})
	if err != nil {
		// Can be a raw S3 SDK error (AccessDenied, throttling, etc.) —
		// never echo err.Error() to the client.
		slog.Error("artifact put failed",
			"datapack_id", dp.ID, "filename", req.Filename, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"ARTIFACT_PUT_FAILED", "failed to store artifact, please try again or contact support")
	}

	return ok(c, http.StatusCreated, artifactPutResponse{
		ArtifactID:  artifact.ID,
		ContentHash: artifact.ContentHash,
		Filename:    artifact.Filename,
		ContentType: artifact.ContentType,
		SizeBytes:   artifact.SizeBytes,
		StoredIn:    artifact.StoredIn(),
	})
}

// Get handles GET /api/v1/datapacks/:datapack_id/artifacts/:content_hash.
//
//	@Summary	Retrieve a file artifact from a datapack
//	@Tags		artifacts
//	@Produce	json
//	@Param		datapack_id		path		string	true	"Datapack ID"
//	@Param		content_hash	path		string	true	"SHA-256 content hash"
//	@Success	200				{object}	APIResponse
//	@Failure	401				{object}	APIResponse
//	@Failure	404				{object}	APIResponse
//	@Failure	410				{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/artifacts/{content_hash} [get]
func (h *ArtifactHandler) Get(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}

	contentHash := strings.ToLower(strings.TrimSpace(c.Param("content_hash")))
	if contentHash == "" {
		return apiError(c, http.StatusBadRequest, "MISSING_PARAM", "content_hash path param required")
	}
	if len(contentHash) != 64 {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "content_hash must be a 64-character SHA-256 hex string")
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "content_hash must be a valid hex string")
	}

	resp, err := h.svc.Get(c.Request().Context(), dp.ID, contentHash)
	if err != nil {
		if err.Error() == "artifact expired" {
			return apiError(c, http.StatusGone, "ARTIFACT_EXPIRED", "artifact has expired")
		}
		// Can be a raw S3 SDK error — never echo err.Error() to the client.
		slog.Error("artifact get failed",
			"datapack_id", dp.ID, "content_hash", contentHash, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"ARTIFACT_GET_FAILED", "failed to retrieve artifact, please try again or contact support")
	}
	if resp == nil {
		return apiError(c, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "no artifact with that hash in this datapack")
	}

	gr := artifactGetResponse{
		ArtifactID:  resp.ID,
		ContentHash: resp.ContentHash,
		Filename:    resp.Filename,
		ContentType: resp.ContentType,
		SizeBytes:   resp.SizeBytes,
		StoredIn:    resp.StoredIn(),
	}

	if resp.ContentBase64 != "" {
		gr.ContentBase64 = resp.ContentBase64
	} else if len(resp.Content) > 0 {
		// S3 artifact: return as base64 to the API caller.
		// The MCP layer writes it to ~/.meko/artifacts/ and returns local_path.
		gr.ContentBase64Large = base64.StdEncoding.EncodeToString(resp.Content)
	}

	return ok(c, http.StatusOK, gr)
}

func validateArtifactPutRequest(req artifactPutRequest) error {
	if strings.TrimSpace(req.Filename) == "" {
		return fmt.Errorf("filename is required")
	}
	if strings.TrimSpace(req.ContentType) == "" {
		return fmt.Errorf("content_type is required")
	}
	if strings.TrimSpace(req.ContentBase64) == "" {
		return fmt.Errorf("content_base64 is required")
	}
	return nil
}

// artifactListItem is one row of the paginated list envelope. Deliberately
// omits the raw bytes — this endpoint is a manifest, not a bulk download.
// `stored_in` tells the client whether to expect a presigned URL from
// /download-url or to fall back to the base64 GET.
type artifactListItem struct {
	ArtifactID     string `json:"artifact_id"`
	ContentHash    string `json:"content_hash"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	SizeBytes      int64  `json:"size_bytes"`
	StoredIn       string `json:"stored_in"` // "db" or "s3"
	CreatedAt      string `json:"created_at"`
	LastAccessedAt string `json:"last_accessed_at,omitempty"`
}

// artifactListResponse mirrors the KB documents-list envelope so paginated
// FE code paths stay uniform across surfaces.
type artifactListResponse struct {
	Items    []artifactListItem `json:"items"`
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
	HasMore  bool               `json:"has_more"`
}

// List handles GET /api/v1/datapacks/:datapack_id/artifacts.
//
// Paginated list of artifacts (id / hash / filename / size / storage tier /
// timestamps). Any datapack grant can read this — the endpoint is registered
// under RequireDatapackAccess only.
//
//	@Summary	List artifacts for a datapack (paginated)
//	@Tags		artifacts
//	@Produce	json
//	@Param		datapack_id	path	string	true	"Datapack ID"
//	@Param		page		query	int		false	"Page number (default 1)"
//	@Param		page_size	query	int		false	"Page size (default 25, max 200)"
//	@Param		q			query	string	false	"Filename substring filter (ILIKE)"
//	@Success	200			{object}	APIResponse
//	@Failure	401			{object}	APIResponse
//	@Failure	500			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/artifacts [get]
func (h *ArtifactHandler) List(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}
	const defaultPageSize = 25
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
	opts := repository.ArtifactListOpts{
		Limit:       pageSize,
		Offset:      (page - 1) * pageSize,
		SearchQuery: strings.TrimSpace(c.QueryParam("q")),
	}

	result, err := h.svc.List(c.Request().Context(), dp.ID, opts)
	if err != nil {
		slog.Error("artifact list failed", "datapack_id", dp.ID, "error", err)
		return apiError(c, http.StatusInternalServerError, "ARTIFACT_LIST_FAILED", "failed to list artifacts")
	}

	items := make([]artifactListItem, 0, len(result.Items))
	for _, a := range result.Items {
		row := artifactListItem{
			ArtifactID:  a.ID,
			ContentHash: a.ContentHash,
			Filename:    a.Filename,
			ContentType: a.ContentType,
			SizeBytes:   a.SizeBytes,
			StoredIn:    a.StoredIn(),
			CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
		}
		if a.LastAccessedAt != nil {
			row.LastAccessedAt = a.LastAccessedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, row)
	}
	return ok(c, http.StatusOK, artifactListResponse{
		Items:    items,
		Total:    result.Total,
		Page:     page,
		PageSize: pageSize,
		HasMore:  result.HasMore,
	})
}

// artifactDownloadURLResponse tells the client either how to grab the file
// from S3 (URL) or that it needs to fall back to the base64 GET path (empty
// URL, storage tier = "db"). Mirrors KB's /knowledge-bases/download-url so
// the client can reuse the same "presigned-or-base64" branch pattern.
type artifactDownloadURLResponse struct {
	URL         string `json:"url,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"` // seconds
	StoredIn    string `json:"stored_in"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// RequestDownloadURL handles POST /api/v1/datapacks/:datapack_id/artifacts/:content_hash/download-url.
//
// For S3-backed artifacts returns a short-lived presigned GET so the browser
// can fetch bytes directly. For DB-backed artifacts returns storage tier =
// "db" with no URL; client should use GET /artifacts/:content_hash which
// returns base64. Ownership check is the same as the underlying GET —
// RequireDatapackAccess only, no role gate.
//
//	@Summary	Presigned S3 download URL for an artifact
//	@Tags		artifacts
//	@Produce	json
//	@Param		datapack_id		path	string	true	"Datapack ID"
//	@Param		content_hash	path	string	true	"SHA-256 content hash"
//	@Success	200				{object}	APIResponse
//	@Failure	400				{object}	APIResponse
//	@Failure	404				{object}	APIResponse
//	@Failure	410				{object}	APIResponse
//	@Failure	503				{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/artifacts/{content_hash}/download-url [post]
func (h *ArtifactHandler) RequestDownloadURL(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
	}
	contentHash := strings.ToLower(strings.TrimSpace(c.Param("content_hash")))
	if contentHash == "" {
		return apiError(c, http.StatusBadRequest, "MISSING_PARAM", "content_hash path param required")
	}
	if len(contentHash) != 64 {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "content_hash must be 64-char SHA-256 hex")
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_PARAM", "content_hash must be a valid hex string")
	}

	presigned, artifact, err := h.svc.PresignDownloadURL(c.Request().Context(), dp.ID, contentHash)
	if err != nil {
		if err.Error() == "artifact expired" {
			return apiError(c, http.StatusGone, "ARTIFACT_EXPIRED", "artifact has expired")
		}
		slog.Error("artifact presign failed",
			"datapack_id", dp.ID, "content_hash", contentHash, "error", err)
		return apiError(c, http.StatusInternalServerError, "ARTIFACT_PRESIGN_FAILED", err.Error())
	}
	if artifact == nil {
		return apiError(c, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "no artifact with that hash in this datapack")
	}

	resp := artifactDownloadURLResponse{
		StoredIn:    artifact.StoredIn(),
		Filename:    artifact.Filename,
		ContentType: artifact.ContentType,
		SizeBytes:   artifact.SizeBytes,
	}
	if presigned != nil {
		resp.URL = presigned.URL
		resp.ExpiresIn = presigned.ExpiresIn
	}
	return ok(c, http.StatusOK, resp)
}
