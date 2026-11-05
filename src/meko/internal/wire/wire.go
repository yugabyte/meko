// Package wire is Meko Cloud's reply shapes, written here from scratch: what MCP tools and REST
// routes send. MCP and REST build their replies here, so one shape never has two versions.
package wire

import (
	"time"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// Time is how replies write times: RFC 3339 in UTC.
func Time(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// OwnerGrant is every datapack's grant: the local user owns them all.
const OwnerGrant = "owner"

// Datapack is a datapack row, as datapack_list and GET /datapacks return them.
type Datapack struct {
	DatapackID   string `json:"datapack_id"`
	DatapackName string `json:"datapack_name"`
	Description  string `json:"description"`
	Grant        string `json:"grant"`
	UserID       string `json:"user_id"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// DatapackRow is a datapack in Cloud's row shape.
func DatapackRow(d store.Datapack, userID string) Datapack {
	return Datapack{DatapackID: d.ID, DatapackName: d.Name, Description: d.Description, Grant: OwnerGrant,
		UserID: userID, CreatedAt: Time(d.CreatedAt), UpdatedAt: Time(d.UpdatedAt)}
}

// DatapackDescription is a datapack with what it holds, as datapack_describe and GET
// /datapacks/:id return it.
type DatapackDescription struct {
	Datapack
	MemoryCount              int  `json:"memory_count"`
	LearningsCount           int  `json:"learnings_count"`
	CollectiveMemoryCount    int  `json:"collective_memory_count"`
	KnowledgeCount           int  `json:"knowledge_count"` // Cloud's deprecated name for knowledge_chunk_count
	KnowledgeChunkCount      int  `json:"knowledge_chunk_count"`
	KnowledgeBaseFileCount   int  `json:"knowledge_base_file_count"`
	SharedKnowledgeCount     int  `json:"shared_knowledge_count"`
	ConversationCount        int  `json:"conversation_count"`
	ConversationSearchOptOut bool `json:"conversation_search_opt_out"`
}

// Describe is a datapack and its counts, as Cloud counts them: knowledge_chunk_count is every
// chunk of the knowledge base, promoted memories' too; knowledge_base_file_count is how many
// knowledge bases the datapack has (one or none), not its files; shared_knowledge_count is the
// promoted memories plus that.
func Describe(d store.Datapack, userID string, c memory.DatapackCounts) DatapackDescription {
	bases := 0
	if c.KnowledgeBase {
		bases = 1
	}
	return DatapackDescription{Datapack: DatapackRow(d, userID), MemoryCount: c.Memories,
		LearningsCount: c.Learnings, CollectiveMemoryCount: c.Promoted, KnowledgeCount: c.KnowledgeChunks,
		KnowledgeChunkCount: c.KnowledgeChunks, KnowledgeBaseFileCount: bases, SharedKnowledgeCount: c.Promoted + bases,
		ConversationCount: c.Conversations, ConversationSearchOptOut: d.ConversationSearchOptOut}
}

// Conversation is a conversation as conversation_create, _update and _get return it. AppID is
// Cloud's, and always null here.
type Conversation struct {
	ID           string         `json:"id"`
	AgentID      string         `json:"agent_id"`
	UserID       string         `json:"user_id"`
	AppID        *string        `json:"app_id"`
	RunID        *string        `json:"run_id"`
	Title        string         `json:"title"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	MessageCount *int           `json:"message_count,omitempty"`
	Messages     []Message      `json:"messages,omitempty"`
}

// ConversationOf is a stored conversation in Cloud's shape; withCount adds message_count.
func ConversationOf(c store.Conversation, userID string, withCount bool) Conversation {
	out := Conversation{ID: c.ID, AgentID: c.AgentID, UserID: userID, Title: c.Title, Metadata: c.Metadata,
		CreatedAt: Time(c.CreatedAt), UpdatedAt: Time(c.UpdatedAt)}
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}
	if c.RunID != "" {
		out.RunID = &c.RunID
	}
	if withCount {
		n := c.MessageCount
		out.MessageCount = &n
	}
	return out
}

// Message is one turn, as conversation_get returns it.
type Message struct {
	MessageID      string         `json:"message_id"`
	ConversationID string         `json:"conversation_id"`
	Input          string         `json:"input"`
	Output         string         `json:"output"`
	Reasoning      string         `json:"reasoning"`
	Plan           []string       `json:"plan"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      string         `json:"created_at"`
}

// MessageOf is a stored turn in Cloud's shape.
func MessageOf(m store.Message) Message {
	out := Message{MessageID: m.ID, ConversationID: m.ConversationID, Input: m.Input, Output: m.Output,
		Reasoning: m.Reasoning, Plan: m.Plan, Metadata: m.Metadata, CreatedAt: Time(m.CreatedAt)}
	if out.Plan == nil {
		out.Plan = []string{}
	}
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}
	return out
}

// ConversationList is a page of a datapack's conversations, as conversation_list returns it.
type ConversationList struct {
	DatapackID        string         `json:"datapack_id"`
	DatapackName      string         `json:"datapack_name"`
	LangfuseProjectID *string        `json:"langfuse_project_id"`
	Limit             int            `json:"limit"`
	Offset            int            `json:"offset"`
	HasMore           bool           `json:"has_more"`
	Conversations     []Conversation `json:"conversations"`
}

// MemoryItem is a memory in a memory list, as GET /memories and memory_get_all return them: the
// text is in data, not memory.
type MemoryItem struct {
	ID             string  `json:"id"`
	Data           string  `json:"data"`
	AgentID        string  `json:"agent_id"`
	ConversationID *string `json:"conversation_id,omitempty"`
	UserID         string  `json:"user_id"`
	UserName       string  `json:"user_name,omitempty"`
	UserEmail      string  `json:"user_email,omitempty"`
	CreatedAt      string  `json:"created_at,omitempty"`
	IsMine         bool    `json:"is_mine"`
	// Meko local's own: "extract" for a memory the memory extractor saved, with the turn it came from.
	Source    string `json:"source,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// MemoryList is a page of memories.
type MemoryList struct {
	Criteria   map[string]any `json:"criteria"`
	Memories   []MemoryItem   `json:"memories"`
	TopK       int            `json:"top_k"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	TotalPages int            `json:"total_pages"`
}

// MemoryListOf is a page of memories in Cloud's list shape. Every memory listed is the local
// user's own.
func MemoryListOf(ms []memoryengine.Memory, criteria map[string]any, total, page, pageSize int) MemoryList {
	out := MemoryList{Criteria: criteria, Memories: make([]MemoryItem, len(ms)), TopK: pageSize, Total: total,
		Page: max(page, 1), PageSize: pageSize, TotalPages: max(1, (total+pageSize-1)/max(pageSize, 1))}
	for i, m := range ms {
		it := MemoryItem{ID: m.ID, Data: m.Memory, AgentID: m.AgentID, UserID: m.UserID, CreatedAt: m.CreatedAt,
			IsMine: true}
		if m.RunID != "" {
			it.ConversationID = &m.RunID
		}
		it.Source, it.MessageID = Provenance(m.Metadata)
		out.Memories[i] = it
	}
	return out
}

// Token estimates, as Meko Cloud makes them: the weights context size counts with too.
const (
	kbChunkTokens          = memory.TokensPerKBChunk
	memoryTokens           = memory.TokensPerMemory
	conversationTokens     = memory.TokensPerConversation
	defaultBaselineTokens  = 10 * kbChunkTokens
	scoreHigh, scoreMedium = 0.75, 0.5
)

// SourceSavings is one source's share of a context_search's estimated savings.
type SourceSavings struct {
	TokensSent  int `json:"tokens_sent"`
	TokensSaved int `json:"tokens_saved"`
}

// TokenSavings is context_search's estimate of the tokens it saved against sending the datapack's
// whole knowledge base. Each source is there only when it returned something.
type TokenSavings struct {
	TotalTokens   int            `json:"total_tokens"`
	KnowledgeBase *SourceSavings `json:"knowledge_base,omitempty"`
	Memory        *SourceSavings `json:"memory,omitempty"`
	Conversation  *SourceSavings `json:"conversation,omitempty"`
	TokensSaved   *int           `json:"tokens_saved,omitempty"`
	// Baseline is what TokensSaved is measured against: the datapack's whole knowledge base (1000 a
	// chunk), or Cloud's default of 10 chunks. The call log keeps it.
	Baseline int `json:"-"`
}

// Sent is the tokens the results sent in all.
func (t TokenSavings) Sent() int {
	n := 0
	for _, s := range []*SourceSavings{t.KnowledgeBase, t.Memory, t.Conversation} {
		if s != nil {
			n += s.TokensSent
		}
	}
	return n
}

// Saved is the tokens saved in all.
func (t TokenSavings) Saved() int {
	if t.TokensSaved == nil {
		return 0
	}
	return *t.TokensSaved
}

// Savings estimates a context_search's savings as Meko Cloud does. What it sent is 1000 tokens a
// knowledge-base result, 200 a memory, and 1000 when past conversations matched. Its saving is
// against the whole knowledge base (Baseline). Each source's own saving is against the knowledge
// results' tokens when the knowledge base returned some (total_tokens), else the same baseline.
func Savings(r memory.ContextResults) TokenSavings {
	total := defaultBaselineTokens
	switch {
	case len(r.KnowledgeBase) > 0:
		total = len(r.KnowledgeBase) * kbChunkTokens
	case r.KnowledgeChunks > 0:
		total = r.KnowledgeChunks * kbChunkTokens
	}
	baseline := defaultBaselineTokens
	if r.KnowledgeChunks > 0 {
		baseline = r.KnowledgeChunks * kbChunkTokens
	}
	out := TokenSavings{TotalTokens: total, Baseline: baseline}
	source := func(sent int) *SourceSavings {
		return &SourceSavings{TokensSent: sent, TokensSaved: max(0, total-sent)}
	}
	if len(r.KnowledgeBase) > 0 {
		out.KnowledgeBase = source(len(r.KnowledgeBase) * kbChunkTokens)
	}
	if len(r.Memory) > 0 {
		out.Memory = source(len(r.Memory) * memoryTokens)
	}
	if r.Conversation != nil && len(r.Conversation.Hits) > 0 {
		out.Conversation = source(conversationTokens)
	}
	if sent := out.Sent(); sent > 0 {
		saved := max(0, baseline-sent)
		out.TokensSaved = &saved
	}
	return out
}

// Bands counts results by match quality by cosine similarity, as Meko Cloud's metrics band them: a
// memory by its raw cosine (its score when it has none), a knowledge-base result and a conversation
// hit by 1 - distance.
func Bands(r memory.ContextResults) (high, mid, low int) {
	add := func(v float64) {
		switch {
		case v >= scoreHigh:
			high++
		case v >= scoreMedium:
			mid++
		default:
			low++
		}
	}
	// The fused score averages the cosine with keyword and entity terms, so it reads higher or lower
	// than the cosine the other sources are banded by: not comparable in either direction.
	for _, m := range r.Memory {
		switch {
		case m.RawSimilarityScore != nil:
			add(*m.RawSimilarityScore)
		case m.Score != nil:
			add(*m.Score)
		}
	}
	for _, k := range r.KnowledgeBase {
		add(1 - k.Distance)
	}
	if r.Conversation != nil {
		for _, h := range r.Conversation.Hits {
			add(1 - h.Distance)
		}
	}
	return high, mid, low
}

// ContextSearch is context_search's reply.
type ContextSearch struct {
	Results      ContextSearchResults `json:"results"`
	TokenSavings TokenSavings         `json:"token_savings"`
}

// ContextSearchResults is each source's results: null when it wasn't searched.
type ContextSearchResults struct {
	KnowledgeBase []memory.KBResult           `json:"knowledge_base"`
	Memory        []memoryengine.Memory       `json:"memory"`
	Conversation  *memory.ConversationResults `json:"conversation"`
}

// ContextSearchOf is context_search's reply for its results.
func ContextSearchOf(r memory.ContextResults) ContextSearch {
	return ContextSearch{
		Results:      ContextSearchResults{KnowledgeBase: r.KnowledgeBase, Memory: r.Memory, Conversation: r.Conversation},
		TokenSavings: Savings(r),
	}
}

// PromotedItem is a promoted memory, as GET /promoted-memories and memory_get_all with promoted
// list them.
type PromotedItem struct {
	ID             string  `json:"id"`
	ConversationID *string `json:"conversation_id,omitempty"`
	AgentID        string  `json:"agent_id,omitempty"`
	Data           string  `json:"data"`
	CreatedAt      string  `json:"created_at"`
	PromotedToKBAt *string `json:"promoted_to_kb_at"` // null while it waits for its vector
}

// PromotedList is a page of promoted memories.
type PromotedList struct {
	Criteria         map[string]any `json:"criteria"`
	PromotedMemories []PromotedItem `json:"promoted_memories"`
	Total            int            `json:"total"`
	Page             int            `json:"page"`
	PageSize         int            `json:"page_size"`
	TotalPages       int            `json:"total_pages"`
}

// PromotedListOf is a page of promoted memories in Cloud's shape.
func PromotedListOf(ps []store.Promoted, criteria map[string]any, total, page, pageSize int) PromotedList {
	out := PromotedList{Criteria: criteria, PromotedMemories: make([]PromotedItem, len(ps)), Total: total,
		Page: max(page, 1), PageSize: pageSize, TotalPages: max(1, (total+pageSize-1)/max(pageSize, 1))}
	for i, p := range ps {
		it := PromotedItem{ID: p.ID, AgentID: p.AgentID, Data: p.Text, CreatedAt: Time(p.CreatedAt)}
		if p.ConversationID != "" {
			it.ConversationID = &p.ConversationID
		}
		if p.PromotedToKBAt != nil {
			t := Time(*p.PromotedToKBAt)
			it.PromotedToKBAt = &t
		}
		out.PromotedMemories[i] = it
	}
	return out
}

// Artifact is a kept file's description, as artifact_put and the artifact routes return it. The
// bytes are always in the database ("db"), and artifact_get always returns them inline.
type Artifact struct {
	ArtifactID     string  `json:"artifact_id"`
	ContentHash    string  `json:"content_hash"`
	Filename       string  `json:"filename"`
	ContentType    string  `json:"content_type"`
	SizeBytes      int     `json:"size_bytes"`
	StoredIn       string  `json:"stored_in"`
	CreatedAt      string  `json:"created_at"`
	LastAccessedAt *string `json:"last_accessed_at,omitempty"`
	ContentBase64  string  `json:"content_base64,omitempty"`
}

// ArtifactOf is a kept file's description, without its bytes.
func ArtifactOf(a store.Artifact) Artifact {
	out := Artifact{ArtifactID: a.ID, ContentHash: a.ContentHash, Filename: a.Filename, ContentType: a.ContentType,
		SizeBytes: a.SizeBytes, StoredIn: "db", CreatedAt: Time(a.CreatedAt)}
	if a.LastAccessedAt != nil {
		t := Time(*a.LastAccessedAt)
		out.LastAccessedAt = &t
	}
	return out
}

// Provenance is where a memory came from, from its metadata: "extract" and the turn's id for one the
// memory extractor saved, empty otherwise.
func Provenance(md map[string]any) (source, messageID string) {
	source, _ = md[memory.MetaSource].(string)
	messageID, _ = md[memory.MetaMessageID].(string)
	return source, messageID
}
