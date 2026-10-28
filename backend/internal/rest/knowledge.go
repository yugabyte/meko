package rest

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/knowledge"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/wire"
)

// Knowledge-file list pages and activity pages, as Meko Cloud's.
const (
	documentPage    = 50
	maxDocumentPage = 200
	activityPage    = 10
	maxActivityPage = 50
)

// registerKnowledge adds the knowledge-base routes meko_ui calls, under
// /datapacks/:id/knowledge-bases.
func (a *api) registerKnowledge(g *echo.Group) {
	kb := g.Group("/datapacks/:id/knowledge-bases")
	kb.POST("/upload-url", a.uploadURL)
	kb.POST("/upload-complete", a.uploadComplete)
	kb.POST("/create", a.createKnowledgeBase)
	kb.GET("/document-status", a.documentStatus)
	kb.GET("/status", a.knowledgeStatus)
	kb.GET("/documents", a.listDocuments)
	kb.DELETE("/documents/:document", a.deleteDocument)
	kb.GET("/summary", a.knowledgeSummary)
	kb.GET("/pending-uploads", a.pendingUploads)
	kb.DELETE("/pending-uploads", a.cancelUpload)
	kb.GET("/activity-log", a.activityLog)
	kb.POST("/download-url", a.downloadURL)
}

// knowledgeError replies for the knowledge-file errors a caller can act on, as Cloud's API does.
func knowledgeError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, knowledge.ErrInvalid):
		return badRequest(c, strings.TrimPrefix(err.Error(), knowledge.ErrInvalid.Error()+": "))
	case errors.Is(err, memory.ErrForeignKey):
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "That key isn't in this datapack.")
	case errors.Is(err, memory.ErrDocumentProcessing):
		return envelope.Fail(c, http.StatusConflict, "DOCUMENT_PROCESSING", memory.ErrDocumentProcessing.Error())
	case errors.Is(err, memory.ErrReferenced):
		return envelope.Fail(c, http.StatusConflict, "OBJECT_REFERENCED", memory.ErrReferenced.Error())
	case errors.Is(err, memory.ErrNotFound):
		return notFound(c, "document not found in this datapack")
	}
	return serviceError(c, err)
}

func (a *api) uploadURL(c echo.Context) error {
	var body struct {
		Filename    string `json:"filename"`
		Size        int64  `json:"size"`
		ContentHash string `json:"content_hash"`
		Client      string `json:"client"`
	}
	cloud, ok, err := a.inCloud(c)
	if !ok {
		return err
	}
	if cloud {
		if bound, berr := bind(c, &body); !bound {
			return berr
		}
		return a.toCloud(c, http.StatusOK, http.MethodPost, cloudPath(c, "/knowledge-bases/upload-url"), body)
	}
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if ok, err = bind(c, &body); !ok {
		return err
	}
	agent := "meko_ui"
	if body.Client != "" && body.Client != "browser" {
		agent = body.Client
	}
	offer, err := a.Service.UploadURL(ctx(c), memory.UploadRequest{DatapackID: dp, Filename: body.Filename,
		Size: body.Size, ContentHash: body.ContentHash, AgentID: agent})
	if err != nil {
		return knowledgeError(c, err)
	}
	offer.URL, offer.StatusURL = a.here(c, offer.URL), a.here(c, offer.StatusURL)
	return envelope.OK(c, wire.UploadOfferOf(offer, false))
}

func (a *api) uploadComplete(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		Key      string `json:"key"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}
	if ok, err = bind(c, &body); !ok {
		return err
	}
	if body.Key == "" || body.Filename == "" {
		return badRequest(c, "key and filename are required")
	}
	if err = a.Service.ConfirmUpload(ctx(c), dp, body.Key); err != nil {
		return knowledgeError(c, err)
	}
	return envelope.OK(c, map[string]string{"message": "upload confirmed", "key": body.Key})
}

func (a *api) createKnowledgeBase(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if err = a.Service.CreateKnowledgeBase(ctx(c), dp); err != nil {
		return serviceError(c, err)
	}
	return created(c, map[string]any{"datapack_id": dp, "rag_source_id": dp, "index_name": memory.IndexName,
		"source_uri": a.Service.SourceURI(dp), "status": "build_triggered", "rejected_documents": []any{}})
}

func (a *api) documentStatus(c echo.Context) error {
	key := c.QueryParam("document_key")
	if cloud, ok, err := a.inCloud(c); !ok || cloud {
		if !ok {
			return err
		}
		if key == "" {
			return badRequest(c, "document_key query parameter is required")
		}
		return a.toCloud(c, http.StatusOK, http.MethodGet,
			cloudPath(c, "/knowledge-bases/document-status?document_key="+url.QueryEscape(key)), nil)
	}
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if key == "" {
		return badRequest(c, "document_key query parameter is required")
	}
	st, err := a.Service.DocumentStatus(ctx(c), dp, key)
	if errors.Is(err, memory.ErrNotFound) {
		return notFound(c, "document not found for document_key")
	}
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, wire.DocumentStatusOf(st, a.Service.SourceURI(dp)))
}

// allDocuments is the most files the status route lists.
const allDocuments = 1000

func (a *api) knowledgeStatus(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	docs, _, err := a.Service.Documents(ctx(c), store.DocumentPage{DatapackID: dp, Limit: allDocuments})
	if err != nil {
		return serviceError(c, err)
	}
	st, err := a.Service.KnowledgeStats(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	source := a.Service.SourceURI(dp)
	details := make([]wire.PipelineDetail, len(docs))
	for i, d := range docs {
		details[i] = wire.PipelineOf(d, source)
	}
	sources := []map[string]string{}
	if st.HasBase {
		status := "COMPLETED"
		if st.InFlight > 0 {
			status = "IN_PROGRESS"
		}
		sources = append(sources, map[string]string{"source_id": dp, "source_uri": source, "status": status})
	}
	return envelope.OK(c, map[string]any{"index_name": memory.IndexName, "source_statuses": sources,
		"pipeline_details": details})
}

func (a *api) listDocuments(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	page, size, ok, err := paging(c, documentPage, maxDocumentPage)
	if !ok {
		return err
	}
	docs, total, err := a.Service.Documents(ctx(c), store.DocumentPage{DatapackID: dp, Query: c.QueryParam("q"),
		TypeSuffix: c.QueryParam("type_filter"), Status: c.QueryParam("status_filter"), Limit: size,
		Offset: (page - 1) * size})
	if err != nil {
		return serviceError(c, err)
	}
	email := a.email(c)
	items := make([]wire.DocumentItem, len(docs))
	for i, d := range docs {
		items[i] = wire.DocumentItemOf(d, a.Service.UserID(), email)
	}
	return envelope.OK(c, map[string]any{"items": items, "total": total, "page": page, "page_size": size,
		"has_more": page*size < total})
}

// email is the local user's, for the uploader fields Cloud shows.
func (a *api) email(c echo.Context) string {
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return ""
	}
	return p.Email
}

func (a *api) deleteDocument(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	id, perr := uuid.Parse(c.Param("document"))
	if perr != nil {
		return notFound(c, "document not found in this datapack")
	}
	n, err := a.Service.DeleteDocument(ctx(c), dp, id.String())
	if err != nil {
		return knowledgeError(c, err)
	}
	return envelope.OK(c, wire.DeletedDocument{DocumentID: id.String(), ChunksDeleted: n, S3Deleted: true})
}

func (a *api) knowledgeSummary(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	d, err := a.Service.Datapack(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	st, err := a.Service.KnowledgeStats(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	counts, err := a.Service.CountDatapack(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	var em *store.Embedder
	if e, found, err := a.Service.Embedder(ctx(c)); err != nil {
		return err
	} else if found {
		em = &e
	}
	return envelope.OK(c, wire.KnowledgeSummaryOf(d.Name, st, counts.Promoted, em, knowledge.ChunkSize))
}

// pendingUploads lists files stored but not yet registered, as Cloud lists S3 objects nobody
// registered. Meko registers a file the moment its bytes arrive, so there are none.
func (a *api) pendingUploads(c echo.Context) error {
	if _, ok, err := a.pathDatapack(c); !ok {
		return err
	}
	return envelope.OK(c, map[string]any{"items": []any{}, "count": 0})
}

func (a *api) cancelUpload(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	key := c.QueryParam("key")
	if key == "" {
		return badRequest(c, "key query parameter is required")
	}
	if err = a.Service.CancelUpload(ctx(c), dp, key); err != nil {
		return knowledgeError(c, err)
	}
	return envelope.OK(c, map[string]string{"key": key})
}

func (a *api) activityLog(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	limit, ok, err := queryInt(c, "limit", activityPage)
	if !ok {
		return err
	}
	limit = min(max(limit, 1), maxActivityPage)
	d, err := a.Service.Datapack(ctx(c), dp)
	if err != nil {
		return serviceError(c, err)
	}
	acts, next, err := a.Service.Activity(ctx(c), dp, c.QueryParam("cursor"), limit)
	if err != nil {
		return serviceError(c, err)
	}
	email := a.email(c)
	items := make([]wire.ActivityItem, len(acts))
	for i, x := range acts {
		items[i] = wire.ActivityItemOf(x, d, email)
	}
	return envelope.OK(c, map[string]any{"items": items, "has_more": next != "", "next_cursor": next,
		"limit": limit})
}

func (a *api) downloadURL(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		DocumentURI string `json:"document_uri"`
	}
	if ok, err = bind(c, &body); !ok {
		return err
	}
	link, key, err := a.Service.DownloadURL(ctx(c), dp, body.DocumentURI)
	if err != nil {
		return knowledgeError(c, err)
	}
	return envelope.OK(c, map[string]any{"url": a.here(c, link), "key": key, "expires_in": 300})
}

// Tickets is the two routes the upload and download links point at, which take no token: the
// ticket in the path is their credential. The server mounts them outside /api/v1's body limit.
func Tickets(o Options) (upload, download echo.HandlerFunc) {
	a := &api{o}
	return a.receiveUpload, a.sendDownload
}

// receiveUpload takes a multipart POST to an upload link, as S3 takes one to a presigned URL: its
// fields first, then the file as "file". It answers 204, as S3 does.
func (a *api) receiveUpload(c echo.Context) error {
	mr, err := c.Request().MultipartReader()
	if err != nil {
		return badRequest(c, "the upload must be multipart/form-data, with the file as \"file\"")
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return badRequest(c, "the upload has no \"file\" part")
		}
		if err != nil {
			return badRequest(c, "the upload isn't valid multipart/form-data")
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		err = a.Service.ReceiveUpload(ctx(c), c.Param("ticket"), part)
		_ = part.Close()
		switch {
		case errors.Is(err, memory.ErrTicket):
			return envelope.Fail(c, http.StatusForbidden, "ACCESS_DENIED", memory.ErrTicket.Error())
		case err != nil:
			return knowledgeError(c, err)
		}
		return c.NoContent(http.StatusNoContent)
	}
}

// sendDownload sends the file a download link was made for, as an attachment.
func (a *api) sendDownload(c echo.Context) error {
	d, err := a.Service.OpenDownload(c.Param("ticket"))
	switch {
	case errors.Is(err, memory.ErrTicket):
		return envelope.Fail(c, http.StatusForbidden, "ACCESS_DENIED", memory.ErrTicket.Error())
	case errors.Is(err, memory.ErrNotFound):
		return notFound(c, "the file is gone")
	case err != nil:
		return err
	}
	defer func() { _ = d.File.Close() }() // read only: closing it can't lose a write
	c.Response().Header().Set(echo.HeaderContentDisposition,
		mime.FormatMediaType("attachment", map[string]string{"filename": d.Filename}))
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Stream(http.StatusOK, contentType(d.Type), d.File)
}

// contentType is what a download is sent as: its type, with text as UTF-8; HTML as plain text, so
// a browser never runs a page from Meko's origin.
func contentType(t string) string {
	switch {
	case t == "text/html":
		return "text/plain; charset=utf-8"
	case strings.HasPrefix(t, "text/") || t == "application/json":
		return t + "; charset=utf-8"
	case t == "":
		return "application/octet-stream"
	}
	return t
}
