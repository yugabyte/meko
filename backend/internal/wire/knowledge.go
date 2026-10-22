package wire

import (
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
)

// KnowledgeSearch is knowledgebase_search's reply.
type KnowledgeSearch struct {
	Results []memory.KBResult `json:"results"`
	Count   int               `json:"count"`
}

// UploadOffer is upload-url's reply, and knowledgebase_upload_document's (without status_url and
// with datapack_id): where to POST the file, or the file already there.
type UploadOffer struct {
	DatapackID       string            `json:"datapack_id,omitempty"`
	URL              string            `json:"url"`
	Fields           map[string]string `json:"fields,omitempty"`
	Key              string            `json:"key"`
	AlreadyUploaded  bool              `json:"already_uploaded"`
	ExistingFilename string            `json:"existing_filename,omitempty"`
	StatusURL        string            `json:"status_url,omitempty"`
}

// UploadOfferOf is an upload offer in Cloud's shape; forMCP gives the tool's (datapack_id, no
// status_url), else the REST route's.
func UploadOfferOf(o memory.UploadOffer, forMCP bool) UploadOffer {
	out := UploadOffer{URL: o.URL, Fields: o.Fields, Key: o.Key, AlreadyUploaded: o.AlreadyUploaded,
		ExistingFilename: o.ExistingFilename}
	if forMCP {
		out.DatapackID = o.DatapackID
	} else {
		out.StatusURL = o.StatusURL
	}
	return out
}

// DocumentStatus is document-status's reply, and knowledgebase_document_status's: Cloud's state
// words and message, upload while the bytes are awaited, document once they're stored, and the
// legacy pipeline fields at the top. AgentK reads state, and document_status only from a Meko
// without it.
type DocumentStatus struct {
	DocumentKey string         `json:"document_key"`
	Filename    string         `json:"filename"`
	State       string         `json:"state"`
	Message     string         `json:"message"`
	Upload      *UploadState   `json:"upload,omitempty"`
	Document    *DocumentState `json:"document,omitempty"`
	*PipelineDetail
}

// UploadedFile is knowledgebase_upload_file's reply: the file's status, and whether the datapack
// had its bytes already.
type UploadedFile struct {
	DatapackID      string `json:"datapack_id"`
	AlreadyUploaded bool   `json:"already_uploaded"`
	DocumentStatus
}

// UploadState is an upload link still waiting for its file.
type UploadState struct {
	ExpiresAt string `json:"expires_at"`
	Attempts  int    `json:"attempts"`
}

// DocumentState is a stored file's indexing state.
type DocumentState struct {
	ChunksProcessed int    `json:"chunks_processed"`
	UploadedAt      string `json:"uploaded_at"`
	SizeBytes       int64  `json:"size_bytes"`
	Status          string `json:"status"`
}

// PipelineDetail is a file's indexing as Cloud's status route lists it.
type PipelineDetail struct {
	IndexName       string `json:"index_name"`
	SourceURI       string `json:"source_uri"`
	DocumentName    string `json:"document_name"`
	DocumentStatus  string `json:"document_status"`
	ChunksProcessed int    `json:"chunks_processed"`
}

// PipelineOf is a stored file's indexing in Cloud's shape.
func PipelineOf(d store.Document, sourceURI string) PipelineDetail {
	return PipelineDetail{IndexName: memory.IndexName, SourceURI: sourceURI, DocumentName: d.Key,
		DocumentStatus: d.Status, ChunksProcessed: d.ChunksProcessed}
}

// DocumentStatusOf is a file's state in Cloud's shape.
func DocumentStatusOf(st memory.DocumentStatus, sourceURI string) DocumentStatus {
	out := DocumentStatus{DocumentKey: st.Key, Filename: st.Filename}
	if st.Document == nil {
		out.State, out.Message = "awaiting_upload", "Waiting for the file at its upload link."
		if st.WaitingUntil != nil {
			out.Upload = &UploadState{ExpiresAt: Time(*st.WaitingUntil)}
		}
		return out
	}
	d := *st.Document
	switch d.Status {
	case store.DocCompleted:
		out.State, out.Message = "completed", "The file is indexed and searchable."
	case store.DocFailed:
		out.State, out.Message = "failed", "Indexing failed for this file."
		if d.Error != "" {
			out.Message += " " + upperFirst(d.Error) + "."
		}
	default:
		out.State, out.Message = "indexing", "The file is being indexed."
	}
	out.Document = &DocumentState{ChunksProcessed: d.ChunksProcessed, UploadedAt: Time(d.UploadedAt),
		SizeBytes: d.SizeBytes, Status: d.Status}
	p := PipelineOf(d, sourceURI)
	out.PipelineDetail = &p
	return out
}

func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// DocumentItem is a file as GET .../knowledge-bases/documents lists it.
type DocumentItem struct {
	ID               string  `json:"id"`
	DocumentName     string  `json:"document_name"`
	DocumentURI      string  `json:"document_uri"`
	DocumentType     string  `json:"document_type"`
	DocumentChecksum string  `json:"document_checksum"`
	ChunksProcessed  int     `json:"chunks_processed"`
	CompletedAt      *string `json:"completed_at,omitempty"`
	UploadedAt       string  `json:"uploaded_at"`
	Status           string  `json:"status"`
	UploadedByUserID string  `json:"uploaded_by_user_id"`
	UploadedByEmail  string  `json:"uploaded_by_email,omitempty"`
	SizeBytes        int64   `json:"size_bytes"`
	Filename         string  `json:"filename"`
	Error            string  `json:"error,omitempty"`
}

// DocumentItemOf is a stored file in Cloud's list shape. Its checksum is its SHA-256 (Cloud's is
// the S3 ETag).
func DocumentItemOf(d store.Document, userID, email string) DocumentItem {
	out := DocumentItem{ID: d.ID, DocumentName: d.Key, DocumentURI: memory.DocumentURI(d.Key),
		DocumentType: d.DocumentType, DocumentChecksum: d.ContentHash, ChunksProcessed: d.ChunksProcessed,
		UploadedAt: Time(d.UploadedAt), Status: d.Status, UploadedByUserID: userID, UploadedByEmail: email,
		SizeBytes: d.SizeBytes, Filename: d.Filename, Error: d.Error}
	if d.CompletedAt != nil {
		t := Time(*d.CompletedAt)
		out.CompletedAt = &t
	}
	return out
}

// DeletedDocument is what deleting a file reports, as Cloud does: s3_deleted says its bytes went.
type DeletedDocument struct {
	DocumentID    string `json:"document_id"`
	ChunksDeleted int    `json:"chunks_deleted"`
	S3Deleted     bool   `json:"s3_deleted"`
}

// KnowledgeSummary is GET .../knowledge-bases/summary's reply.
type KnowledgeSummary struct {
	TotalFiles            int     `json:"total_files"`
	TotalChunks           int     `json:"total_chunks"`
	LastFileUploadedAt    *string `json:"last_file_uploaded_at,omitempty"`
	HeaviestSourceName    *string `json:"heaviest_source_name,omitempty"`
	HeaviestSourceChunks  *int    `json:"heaviest_source_chunks,omitempty"`
	EmbeddingModel        *string `json:"embedding_model,omitempty"`
	EmbeddingDimensions   *int    `json:"embedding_dimensions,omitempty"`
	AIProvider            *string `json:"ai_provider,omitempty"`
	ChunkSize             int     `json:"chunk_size"`
	PromotedMemoriesCount int     `json:"promoted_memories_count"`
	Description           string  `json:"description"`
	FilesSummary          string  `json:"files_summary"`
}

// embeddingName is the model's name without its pinned commit and quantization.
var embeddingName = regexp.MustCompile(`[@:].*$`)

// KnowledgeSummaryOf sums up a datapack's knowledge base in Cloud's shape and words.
func KnowledgeSummaryOf(datapackName string, st store.KBStats, promoted int, e *store.Embedder,
	chunkSize int) KnowledgeSummary {
	out := KnowledgeSummary{TotalFiles: st.IndexedFiles, TotalChunks: st.FileChunks, ChunkSize: chunkSize,
		PromotedMemoriesCount: promoted}
	if st.LastUploadAt != nil {
		t := Time(*st.LastUploadAt)
		out.LastFileUploadedAt = &t
	}
	heaviest := ""
	if st.Heaviest != "" {
		name, n := filepath.Base(st.Heaviest), st.HeaviestChunks
		out.HeaviestSourceName, out.HeaviestSourceChunks = &name, &n
		heaviest = " Heaviest source: " + name + "."
	}
	if e != nil {
		model, dims, provider := embeddingName.ReplaceAllString(e.Model, ""), e.Dims, e.Provider
		out.EmbeddingModel, out.EmbeddingDimensions, out.AIProvider = &model, &dims, &provider
	}
	files := plural(out.TotalFiles, "file")
	chunks := plural(out.TotalChunks, "chunk")
	out.Description = fmt.Sprintf("Knowledge base for %s — %s totaling %s, reinforced by %s.%s", datapackName,
		files, chunks, plural(promoted, "promoted memory"), heaviest)
	if out.TotalFiles == 0 {
		out.FilesSummary = "No files uploaded yet."
	} else {
		out.FilesSummary = fmt.Sprintf("%s totaling %s.%s", files, chunks, heaviest)
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if word == "promoted memory" {
		return fmt.Sprintf("%d promoted memories", n)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ActivityItem is one event of GET .../knowledge-bases/activity-log.
type ActivityItem struct {
	Type            string `json:"type"`
	Timestamp       string `json:"timestamp"`
	DatapackID      string `json:"datapack_id"`
	DatapackName    string `json:"datapack_name"`
	DocumentName    string `json:"document_name,omitempty"`
	DocumentType    string `json:"document_type,omitempty"`
	ChunksProcessed *int   `json:"chunks_processed,omitempty"`
	SizeBytes       *int64 `json:"size_bytes,omitempty"`
	UploadedByEmail string `json:"uploaded_by_email,omitempty"`
	MemoryID        string `json:"memory_id,omitempty"`
	Content         string `json:"content,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
}

// ActivityItemOf is a knowledge-base event in Cloud's shape.
func ActivityItemOf(a store.Activity, d store.Datapack, email string) ActivityItem {
	out := ActivityItem{Type: a.Type, Timestamp: Time(a.Timestamp), DatapackID: d.ID, DatapackName: d.Name}
	if a.Type == "file_upload" {
		n, size := a.ChunksProcessed, a.SizeBytes
		out.DocumentName, out.DocumentType, out.ChunksProcessed, out.SizeBytes = a.DocumentKey, a.DocumentType, &n, &size
		out.UploadedByEmail = email
		return out
	}
	out.MemoryID, out.Content, out.AgentID, out.ConversationID = a.ID, a.Content, a.AgentID, a.ConversationID
	return out
}
