package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// KnowledgeBaseHandler holds Echo handlers for knowledge-base endpoints.
type KnowledgeBaseHandler struct {
	knowledgeBaseSvc *service.KnowledgeBaseService
	s3Svc            *service.S3Service
	ybaSvc           *service.YBAService
}

// NewKnowledgeBaseHandler creates a new KnowledgeBaseHandler.
func NewKnowledgeBaseHandler(
	knowledgeBaseSvc *service.KnowledgeBaseService,
	s3Svc *service.S3Service,
	ybaSvc *service.YBAService,
) *KnowledgeBaseHandler {
	return &KnowledgeBaseHandler{
		knowledgeBaseSvc: knowledgeBaseSvc,
		s3Svc:            s3Svc,
		ybaSvc:           ybaSvc,
	}
}

// fixIndexOwnership transfers ownership of public.knowledge_base to the
// tenant's MCP user (which is how the RAG worker connects). The RAG worker
// later runs `CREATE INDEX` on this table, which requires ownership (DML
// privileges are insufficient).
//
// Whoever creates the table becomes its initial owner. Depending on how
// dist_rag.init_vector_index is defined (SECURITY DEFINER vs. not) and the
// caller's session role, the initial owner may be yugabyte OR the datapack's
// DB user. Either way this function moves ownership to the MCP user.
//
// No DML grant is issued to the datapack's DB user: nothing in the
// api_server reads or writes public.knowledge_base directly; all access
// goes through dist_rag.* functions which the RAG worker (MCP user) owns.
//
// Idempotent: ALTER OWNER is a no-op when already in the desired state.
// Skips cleanly if DatapackIndexName does not yet exist.
func (h *KnowledgeBaseHandler) fixIndexOwnership(dp *models.Datapack) error {
	adminConnStr, err := h.ybaSvc.GetAdminConnStr(dp.YBUniverseID)
	if err != nil {
		return fmt.Errorf("get admin conn str: %w", err)
	}

	u, err := url.Parse(adminConnStr)
	if err != nil {
		return fmt.Errorf("parse admin conn str: %w", err)
	}

	dbName := fmt.Sprintf("%s_%s", dp.AccountID, dp.Name)
	u.Path = "/" + dbName
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	dpAdminConnStr := u.String()

	adminDB, err := sql.Open("postgres", dpAdminConnStr)
	if err != nil {
		return fmt.Errorf("open admin conn to %s: %w", dbName, err)
	}
	defer adminDB.Close()

	var currentOwner string
	err = adminDB.QueryRow(
		`SELECT tableowner FROM pg_tables
		  WHERE schemaname='public' AND tablename=$1`,
		service.DatapackIndexName,
	).Scan(&currentOwner)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Debug("knowledge_base table not present; skipping ownership fix",
			"datapack", dp.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current owner: %w", err)
	}

	kbTable := "public." + repository.QuoteIdent(service.DatapackIndexName)

	if currentOwner != dp.MCPUserName {
		ownerSQL := fmt.Sprintf(`ALTER TABLE %s OWNER TO %s`,
			kbTable, repository.QuoteIdent(dp.MCPUserName))
		if _, err := adminDB.Exec(ownerSQL); err != nil {
			return fmt.Errorf("alter owner: %w", err)
		}
		slog.Info("transferred knowledge_base ownership",
			"datapack", dp.Name,
			"from", currentOwner, "to", dp.MCPUserName)
	}

	slog.Info("ensured knowledge_base ownership",
		"owner", dp.MCPUserName)
	return nil
}

// openDatapackDB opens a short-lived connection to the datapack's own database.
// Callers must close the returned *sql.DB when done.
func openDatapackDB(dp *models.Datapack) (*sql.DB, error) {
	connStr := dp.YBUniverseConn
	if !strings.Contains(connStr, "sslmode=") {
		if strings.Contains(connStr, "?") {
			connStr += "&sslmode=disable"
		} else {
			connStr += "?sslmode=disable"
		}
	}
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("open datapack DB %s: %w", dp.Name, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping datapack DB %s: %w", dp.Name, err)
	}
	return db, nil
}

// Add handles POST /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	Add a knowledge base URL
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string				true	"Datapack name"
//	@Param		body	body		service.AddRequest	true	"Knowledge base URL"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [post]
func (h *KnowledgeBaseHandler) Add(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.AddRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	kbID, err := h.knowledgeBaseSvc.Add(dpName, req)
	if err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusCreated, map[string]string{
		"knowledge_base_id": kbID,
		"url":               req.URL,
		"datapack_name":     dpName,
	})
}

// List handles GET /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	List knowledge bases for a datapack
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [get]
func (h *KnowledgeBaseHandler) List(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	kbs, err := h.knowledgeBaseSvc.List(dpName)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}
	if kbs == nil {
		kbs = []models.KnowledgeBase{}
	}
	return ok(c, http.StatusOK, kbs)
}

// Delete handles DELETE /api/v1/datapacks/:name/knowledge-bases.
//
//	@Summary	Delete a knowledge base
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string					true	"Datapack name"
//	@Param		body	body		service.DeleteRequest	true	"Delete request"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	404		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases [delete]
func (h *KnowledgeBaseHandler) Delete(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	var req service.DeleteRequest
	if err := c.Bind(&req); err != nil {
		return apiError(c, http.StatusBadRequest,
			"INVALID_JSON", "malformed request body")
	}

	if err := h.knowledgeBaseSvc.Delete(dpName, req); err != nil {
		if isValidationErr(err) {
			return apiError(c, http.StatusBadRequest,
				"VALIDATION_ERROR", err.Error())
		}
		if isNotFoundErr(err) {
			return apiError(c, http.StatusNotFound,
				"NOT_FOUND", err.Error())
		}
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", err.Error())
	}

	return ok(c, http.StatusOK, map[string]string{
		"message": "knowledge base deleted",
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
//	@Router		/datapacks/{name}/knowledge-bases/upload-url [post]
func (h *KnowledgeBaseHandler) RequestUploadURL(c echo.Context) error {
	if h.s3Svc == nil || !h.s3Svc.Enabled() {
		return apiError(c, http.StatusServiceUnavailable,
			"S3_DISABLED", "file uploads are not configured")
	}

	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
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
			"VALIDATION_ERROR", "unsupported file type: "+ext+"; allowed: .pdf, .txt, .md, .json, .mp4")
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
		account.ID,
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
			"account_id", account.ID,
			"datapack_id", dp.ID,
			"filename", req.Filename,
			"error", err,
		)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to generate upload URL")
	}

	slog.Info("presigned upload URL generated",
		"account_id", account.ID,
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
//	@Summary	Confirm a completed file upload
//	@Tags		knowledge-bases
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string					true	"Datapack name"
//	@Param		body	body		uploadCompleteRequest	true	"Upload confirmation"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	401		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases/upload-complete [post]
func (h *KnowledgeBaseHandler) ConfirmUpload(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
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
		"account_id", account.ID,
		"datapack", dpName,
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
//  1. Check if source_id is already cached in meko_system (via rag_source_id column).
//
//  2. If not cached, query dist_rag.sources on the datapack DB by source_uri.
//
//  3. If source doesn't exist: create_source → add_source_to_index → WaitAndBuild.
//
//  4. If source exists: cache it, then just trigger build_index.
//
//  5. Persist source_id back to meko_system for future lookups.
//
//     @Summary	Create a knowledge base from uploaded files
//     @Tags		knowledge-bases
//     @Produce	json
//     @Param		name	path		string	true	"Datapack name"
//     @Success	201		{object}	APIResponse
//     @Failure	400		{object}	APIResponse
//     @Failure	401		{object}	APIResponse
//     @Failure	500		{object}	APIResponse
//     @Router		/datapacks/{name}/knowledge-bases/create [post]
func (h *KnowledgeBaseHandler) CreateKB(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	account, err := auth.GetAccount(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	dpDB, err := openDatapackDB(dp)
	if err != nil {
		slog.Error("failed to connect to datapack DB",
			"datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to connect to datapack database")
	}
	defer dpDB.Close()

	ragSvc := service.NewRagService(dpDB)

	// Lazily create the vector index on first KB creation.
	if err := ragSvc.EnsureVectorIndex(); err != nil {
		slog.Error("failed to ensure vector index", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to initialize vector index")
	}

	// Always run the ownership fix: it is idempotent and self-heals tenants
	// whose knowledge_base table was created before the ownership-transfer
	// logic was deployed (those rows are stuck with the DB-user / yugabyte as
	// owner, which blocks CREATE INDEX from the RAG worker).
	if err := h.fixIndexOwnership(dp); err != nil {
		slog.Error("failed to fix index ownership", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to fix index ownership")
	}

	sourceURI := fmt.Sprintf("s3://%s/%s/%s/%s/",
		h.s3Svc.Bucket(), service.S3UserUploadsPrefix, account.ID, dp.ID)

	slog.Info("creating knowledge base",
		"account_id", account.ID,
		"datapack", dpName,
		"source_uri", sourceURI,
	)

	// Step 1: Check if source_id is cached in meko_system.
	cachedSourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dpName)
	if err != nil {
		slog.Error("failed to check cached source IDs", "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to check cached sources")
	}

	var sourceID string
	isNewSource := false

	if len(cachedSourceIDs) > 0 {
		sourceID = cachedSourceIDs[0]

		// Validate the cached source_id actually exists in the datapack DB.
		exists, err := ragSvc.SourceExists(sourceID)
		if err != nil {
			slog.Error("failed to validate cached source_id",
				"source_id", sourceID, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to validate cached source")
		}
		if !exists {
			slog.Warn("cached source_id is stale, clearing cache",
				"source_id", sourceID, "datapack", dpName)
			if err := h.knowledgeBaseSvc.DeleteByDatapack(dpName); err != nil {
				slog.Error("failed to clear stale KB cache", "error", err)
			}
			cachedSourceIDs = nil
			sourceID = ""
		}
	}

	if len(cachedSourceIDs) > 0 {
		// Source is cached and validated — insert new documents directly and rebuild.
		slog.Info("using cached source_id",
			"source_id", sourceID, "datapack", dpName)

		s3Prefix := fmt.Sprintf("%s/%s/%s/", service.S3UserUploadsPrefix, account.ID, dp.ID)
		objects, err := h.s3Svc.ListObjects(c.Request().Context(), s3Prefix)
		if err != nil {
			slog.Error("failed to list S3 objects", "prefix", s3Prefix, "error", err)
			return apiError(c, http.StatusInternalServerError,
				"S3_ERROR", "failed to list uploaded files")
		}

		existingURIs, err := ragSvc.GetExistingDocURIs(sourceID)
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
			if err := ragSvc.InsertDocument(sourceID, obj.Key, docURI, obj.ETag); err != nil {
				slog.Warn("failed to insert document",
					"document", obj.Key, "error", err)
			} else {
				newDocs++
			}
		}

		slog.Info("document sync complete",
			"total_s3_objects", len(objects),
			"already_tracked", len(existingURIs),
			"new_documents", newDocs)

		if newDocs > 0 {
			if err := ragSvc.BuildIndex(); err != nil {
				slog.Error("failed to build index for cached source", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to build index")
			}
		}
	} else {
		// Step 2: Not cached — check dist_rag.sources on the datapack DB.
		sourceID, err = ragSvc.LookupSourceByURI(sourceURI)
		if err != nil {
			slog.Error("failed to lookup source by URI", "error", err)
			return apiError(c, http.StatusInternalServerError,
				"RAG_ERROR", "failed to lookup source")
		}

		if sourceID == "" {
			// Step 3: Source doesn't exist — create it.
			isNewSource = true
			sourceID, err = ragSvc.CreateSource(sourceURI)
			if err != nil {
				slog.Error("failed to create dist_rag source", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to create source")
			}

			if err := ragSvc.AddSourceToIndex(sourceID); err != nil {
				slog.Error("failed to add source to index", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to add source to index")
			}
		} else {
			slog.Info("source found in dist_rag.sources, caching",
				"source_id", sourceID, "source_uri", sourceURI)

			// Source exists but wasn't cached — insert only new documents.
			s3Prefix := fmt.Sprintf("%s/%s/%s/", service.S3UserUploadsPrefix, account.ID, dp.ID)
			objects, err := h.s3Svc.ListObjects(c.Request().Context(), s3Prefix)
			if err != nil {
				slog.Error("failed to list S3 objects", "prefix", s3Prefix, "error", err)
				return apiError(c, http.StatusInternalServerError,
					"S3_ERROR", "failed to list uploaded files")
			}

			existingURIs, err := ragSvc.GetExistingDocURIs(sourceID)
			if err != nil {
				slog.Error("failed to get existing doc URIs", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to check existing documents")
			}

			for _, obj := range objects {
				docURI := fmt.Sprintf("s3://%s/%s", h.s3Svc.Bucket(), obj.Key)
				if existingURIs[docURI] {
					continue
				}
				if err := ragSvc.InsertDocument(sourceID, obj.Key, docURI, obj.ETag); err != nil {
					slog.Warn("failed to insert document",
						"document", obj.Key, "error", err)
				}
			}
		}

		// Persist source_id to meko_system for future lookups.
		kbID, err := h.knowledgeBaseSvc.AddWithRAG(
			dpName, sourceURI, sourceID, ragSvc.IndexName())
		if err != nil {
			slog.Error("failed to save knowledge base", "error", err)
			return apiError(c, http.StatusInternalServerError,
				"INTERNAL_ERROR", "failed to save knowledge base")
		}

		slog.Info("knowledge base entry saved",
			"kb_id", kbID, "source_id", sourceID)

		if isNewSource {
			slog.Info("starting WaitAndBuild goroutine for new source",
				"source_id", sourceID, "datapack", dpName)
			go service.WaitAndBuildAsync(dp.YBUniverseConn, sourceID)
		} else {
			if err := ragSvc.BuildIndex(); err != nil {
				slog.Error("failed to build index", "error", err)
				return apiError(c, http.StatusInternalServerError,
					"RAG_ERROR", "failed to build index")
			}
		}
	}

	slog.Info("knowledge base created",
		"account_id", account.ID,
		"datapack", dpName,
		"source_id", sourceID,
		"is_new_source", isNewSource,
		"index_name", ragSvc.IndexName(),
	)

	return ok(c, http.StatusCreated, map[string]string{
		"source_id":  sourceID,
		"index_name": ragSvc.IndexName(),
		"source_uri": sourceURI,
		"status":     "build_triggered",
	})
}

// KBStatus handles GET /api/v1/datapacks/:name/knowledge-bases/status.
//
//	@Summary	Get knowledge base pipeline status
//	@Tags		knowledge-bases
//	@Produce	json
//	@Param		name	path		string	true	"Datapack name"
//	@Success	200		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	500		{object}	APIResponse
//	@Router		/datapacks/{name}/knowledge-bases/status [get]
func (h *KnowledgeBaseHandler) KBStatus(c echo.Context) error {
	dpName := c.Param("name")
	if dpName == "" {
		return apiError(c, http.StatusBadRequest,
			"VALIDATION_ERROR", "datapack name is required")
	}

	dp, err := auth.GetDatapack(c)
	if err != nil {
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "datapack context missing")
	}

	sourceURIs, err := h.knowledgeBaseSvc.GetSourceURIs(dpName)
	if err != nil {
		slog.Error("failed to get source URIs", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to get source URIs")
	}

	sourceIDs, err := h.knowledgeBaseSvc.GetSourceIDs(dpName)
	if err != nil {
		slog.Error("failed to get source IDs", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to get source IDs")
	}

	if len(sourceIDs) == 0 {
		return ok(c, http.StatusOK, &repository.PipelineStatus{
			IndexName: service.DatapackIndexName,
		})
	}

	dpDB, err := openDatapackDB(dp)
	if err != nil {
		slog.Error("failed to connect to datapack DB for status",
			"datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"INTERNAL_ERROR", "failed to connect to datapack database")
	}
	defer dpDB.Close()

	ragSvc := service.NewRagService(dpDB)

	status, err := ragSvc.GetPipelineStatus(sourceURIs)
	if err != nil {
		slog.Error("failed to get pipeline status", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to get pipeline status")
	}

	srcStatuses, err := ragSvc.GetSourceStatuses(sourceIDs)
	if err != nil {
		slog.Error("failed to get source statuses", "datapack", dpName, "error", err)
		return apiError(c, http.StatusInternalServerError,
			"RAG_ERROR", "failed to get source statuses")
	}
	status.Sources = srcStatuses

	slog.Info("KB status response",
		"datapack", dpName,
		"sources", len(srcStatuses),
		"pipeline_details", len(status.Details),
	)

	return ok(c, http.StatusOK, status)
}
