package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/yugabyte/meko/backend/internal/knowledge"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

func knowledgeTools() []*tool {
	return []*tool{
		{
			name: "knowledgebase_search", title: "Search Knowledge Base", kind: readOnly, failCode: "search_failed",
			desc: "Search the datapack's knowledge base: its uploaded files and its promoted memories, by meaning " +
				"and keywords, reranked by relevance. Results carry chunk_text, document_id, distance, match_type " +
				"and a 0-1 rerank_score; a promoted memory's metadata_filters has source \"memory\" and its " +
				"memory_id. Hits carry no filename.",
			params: []param{
				{name: "query", typ: "string", required: true, desc: "What to find, in plain language."},
				pConversation,
				{name: "datapack_id", typ: "string", required: true,
					desc: "The datapack (UUID). Blank uses the default datapack."},
				pAgent,
				{name: "limit", typ: "integer", def: 10,
					desc: "Widens the candidates searched; how many results come back depends on how well they match."},
				{name: "skip_conversation", typ: "boolean", def: false, desc: "Ignored; kept for clients that send it."},
			},
			run: knowledgeSearch,
		},
		{
			name: "knowledgebase_upload_document", title: "Upload Knowledge File", kind: write,
			failCode: "knowledgebase_upload_document_failed",
			desc: "Get a link to upload a file (" + knowledge.Extensions + "; up to 50 MiB) to the datapack's " +
				"knowledge base. POST the file there as multipart/form-data: every entry of fields first, then the " +
				"file as \"file\". The link works once, for 5 minutes. When already_uploaded is true the datapack " +
				"has these bytes and there's nothing to upload. Then follow it with knowledgebase_document_status " +
				"and the key.",
			params: []param{
				{name: "filename", typ: "string", required: true, desc: "The file's name, with its extension."},
				{name: "size", typ: "integer", required: true, desc: "Its exact size in bytes."},
				{name: "content_hash", typ: "string", required: true, desc: "Its SHA-256, 64 hex characters."},
				{name: "content_head_base64", typ: "string", required: true,
					desc: "Its first 3072 bytes (all of it if smaller), base64, to check its type."},
				pConversation, pDatapack, pAgent,
			},
			run: knowledgeUpload,
		},
		{
			name: "knowledgebase_upload_file", title: "Upload Local Knowledge File", kind: write,
			failCode: "knowledgebase_upload_file_failed",
			desc: "Upload a file on this machine (" + knowledge.Extensions + "; up to 50 MiB) to the datapack's " +
				"knowledge base in one call: Meko reads it from path itself, so nothing is encoded or posted. " +
				"Prefer it to knowledgebase_upload_document for a file on disk. The reply is the file's status " +
				"(usually indexing); follow it with knowledgebase_document_status and document_key until " +
				"completed. already_uploaded is true when the datapack already has these bytes.",
			params: []param{
				{name: "path", typ: "string", required: true, desc: "The file's absolute path."},
				pConversation, pDatapack, pAgent,
			},
			run: knowledgeUploadFile,
		},
		{
			name: "knowledgebase_document_status", title: "Knowledge File Status", kind: readOnly,
			failCode: "knowledgebase_document_status_failed",
			desc: "How an uploaded file is doing: state is awaiting_upload, indexing, completed (searchable) or " +
				"failed (message says why).",
			params: []param{
				{name: "document_key", typ: "string", required: true,
					desc: "The key knowledgebase_upload_document returned."},
				pConversation, pDatapack, pAgent,
			},
			run: knowledgeStatus,
		},
		{
			name: "knowledgebase_delete_document", title: "Delete Knowledge File", kind: destructive,
			failCode: "knowledgebase_delete_document_failed",
			desc: "Delete a file from the datapack's knowledge base, with its chunks and its bytes, by its " +
				"document_id. A file still being indexed can't be deleted yet. Search hits carry no filename.",
			params: []param{
				{name: "document_id", typ: "string", required: true, desc: "The file's id (UUID)."},
				pConversation, pDatapack,
			},
			run: knowledgeDelete,
		},
	}
}

func knowledgeSearch(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	query, err := a.str("query")
	if err != nil {
		return nil, err
	}
	limit, err := a.integer("limit", 10)
	if err != nil {
		return nil, err
	}
	if _, err = a.boolean("skip_conversation"); err != nil {
		return nil, err
	}
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	res, err := c.svc.SearchKnowledge(ctx, dp, query, max(limit, 1))
	if err != nil {
		return nil, err
	}
	c.rec.KBN = len(res)
	for _, r := range res {
		switch v := 1 - r.Distance; {
		case v >= 0.75:
			c.rec.ScoreHigh++
		case v >= 0.5:
			c.rec.ScoreMid++
		default:
			c.rec.ScoreLow++
		}
	}
	return wire.KnowledgeSearch{Results: res, Count: len(res)}, nil
}

// knowledgeError is a knowledge-file call's error as Cloud's tools pass its API's on: the API's
// code, its message and its HTTP status.
func knowledgeError(err error) error {
	withStatus := func(code string, status int, detail string) error {
		return &toolError{Code: code, Detail: detail, Extra: map[string]any{"status": status}}
	}
	switch {
	case errors.Is(err, knowledge.ErrInvalid):
		return fail("VALIDATION_ERROR", strings.TrimPrefix(err.Error(), knowledge.ErrInvalid.Error()+": "))
	case errors.Is(err, memory.ErrNotFound):
		return withStatus("NOT_FOUND", http.StatusNotFound, "document not found in this datapack")
	case errors.Is(err, memory.ErrDocumentProcessing):
		return withStatus("DOCUMENT_PROCESSING", http.StatusConflict, memory.ErrDocumentProcessing.Error())
	}
	return err
}

func knowledgeUpload(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	filename, err := a.str("filename")
	if err != nil {
		return nil, err
	}
	size, err := a.integer("size", 0)
	if err != nil {
		return nil, err
	}
	hash, err := a.str("content_hash")
	if err != nil {
		return nil, err
	}
	head64, err := a.str("content_head_base64")
	if err != nil {
		return nil, err
	}
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(head64) == "" {
		return nil, fail("content_head_base64_required",
			"Pass the file's first 3072 bytes (all of it if smaller), base64, as content_head_base64.")
	}
	head, err := base64.StdEncoding.DecodeString(strings.TrimSpace(head64))
	if err != nil {
		return nil, fail("VALIDATION_ERROR", "content_head_base64 is not valid base64.")
	}
	offer, err := c.svc.UploadURL(ctx, memory.UploadRequest{DatapackID: dp, Filename: filename, Size: int64(size),
		ContentHash: hash, AgentID: agent, Head: head})
	if err != nil {
		return nil, knowledgeError(err)
	}
	return wire.UploadOfferOf(offer, true), nil
}

func knowledgeUploadFile(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	path, err := a.str("path")
	if err != nil {
		return nil, err
	}
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, fail("VALIDATION_ERROR", "path is required")
	}
	offer, err := c.svc.UploadLocalFile(ctx, dp, agent, strings.TrimSpace(path))
	if err != nil {
		return nil, knowledgeError(err)
	}
	st, err := c.svc.DocumentStatus(ctx, dp, offer.Key)
	if err != nil {
		return nil, err
	}
	return wire.UploadedFile{DatapackID: dp, AlreadyUploaded: offer.AlreadyUploaded,
		DocumentStatus: wire.DocumentStatusOf(st, c.svc.SourceURI(dp))}, nil
}

func knowledgeStatus(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	key, err := a.str("document_key")
	if err != nil {
		return nil, err
	}
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, fail("VALIDATION_ERROR", "document_key is required")
	}
	st, err := c.svc.DocumentStatus(ctx, dp, strings.TrimSpace(key))
	if errors.Is(err, memory.ErrNotFound) {
		return nil, fail("NOT_FOUND", "document not found for document_key")
	}
	if err != nil {
		return nil, err
	}
	return wire.DocumentStatusOf(st, c.svc.SourceURI(dp)), nil
}

func knowledgeDelete(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := a.str("document_id")
	if err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	u, perr := uuid.Parse(id)
	if perr != nil {
		return nil, fail("invalid_document_id", fmt.Sprintf("'%s' is not a valid document ID (UUID). Use the "+
			"document_id from knowledgebase_search results.", id))
	}
	id = u.String()
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	n, err := c.svc.DeleteDocument(ctx, dp, id)
	if err != nil {
		return nil, knowledgeError(err)
	}
	return wire.DeletedDocument{DocumentID: id, ChunksDeleted: n, S3Deleted: true}, nil
}
