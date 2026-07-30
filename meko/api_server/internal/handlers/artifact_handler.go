package handlers

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/service"
)

const defaultMaxArtifactSizeBytes int64 = 5 * 1024 * 1024 // 5 MiB

// ArtifactHandler serves the two artifact endpoints:
//
//	POST /api/v1/datapacks/:datapack_id/artifacts
//	GET  /api/v1/datapacks/:datapack_id/artifacts/:content_hash
type ArtifactHandler struct {
	svc                 *service.ArtifactService
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
	ArtifactID    string `json:"artifact_id"`
	ContentHash   string `json:"content_hash"`
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	SizeBytes     int64  `json:"size_bytes"`
	StoredIn      string `json:"stored_in"`
	ContentBase64 string `json:"content_base64,omitempty"` // inline (db) artifacts
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
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/artifacts [post]
func (h *ArtifactHandler) Put(c echo.Context) error {
	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "datapack context missing")
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
		return apiError(c, http.StatusInternalServerError, "ARTIFACT_PUT_FAILED", err.Error())
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
		return apiError(c, http.StatusInternalServerError, "ARTIFACT_GET_FAILED", err.Error())
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
