// Package memory is Meko's memories, conversations and knowledge base as Go calls: what the MCP
// tools and REST routes serve. Model work (embedding, reranking, and mem0's add and search) goes
// to the Node engine; reads that need no model come straight from the store.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/knowledge"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// Errors the service returns, besides the engine's (engine.ErrUnavailable, *engine.NotReadyError).
var (
	ErrNoDatapack = errors.New("no such datapack")
	ErrNotFound   = errors.New("no such memory")
	ErrBadRequest = errors.New("bad request")
)

// Service is the memory, conversation and knowledge-base operations of one install.
type Service struct {
	st            *store.Store
	eng           *engine.Client
	userID        string
	log           *slog.Logger
	noEngine      string
	child         func() (supervisor.Status, bool)
	maxDistance   float64
	kbMaxDistance float64
	kick          chan struct{}
	indexKick     chan struct{}
	files         *knowledge.Files
	fileMu        sync.Mutex // a file's bytes and its row change together: storing one, deleting one
	tickets       *tickets
	baseURL       string
}

// Options configure a Service.
type Options struct {
	UserID string // the install's local user
	Log    *slog.Logger
	// NoEngine, if set, says why serve couldn't start the engine (no Node, or it isn't built).
	NoEngine string
	// EngineChild, if set, is the supervisor's status of the engine's child process.
	EngineChild func() (supervisor.Status, bool)
	// MaxDistance is conversation search's cutoff; 0 means DefaultMaxDistance.
	MaxDistance float64
	// KBMaxDistance is context_search's cutoff for knowledge-base results; 0 means
	// DefaultKBMaxDistance.
	KBMaxDistance float64
	// FilesDir is where knowledge files' bytes live (paths.Layout.KnowledgeFiles).
	FilesDir string
	// BaseURL is this server's, http://127.0.0.1:<port>, for the upload and download links it gives.
	BaseURL string
}

// New is the service over st and the engine eng.
func New(st *store.Store, eng *engine.Client, o Options) *Service {
	if o.MaxDistance <= 0 {
		o.MaxDistance = DefaultMaxDistance
	}
	if o.KBMaxDistance <= 0 {
		o.KBMaxDistance = DefaultKBMaxDistance
	}
	return &Service{st: st, eng: eng, userID: o.UserID, log: o.Log, noEngine: o.NoEngine, child: o.EngineChild,
		maxDistance: o.MaxDistance, kbMaxDistance: o.KBMaxDistance, kick: make(chan struct{}, 1),
		indexKick: make(chan struct{}, 1), files: knowledge.NewFiles(o.FilesDir),
		tickets: &tickets{m: map[string]*ticket{}}, baseURL: strings.TrimSuffix(o.BaseURL, "/")}
}

// UserID is the install's local user, the user_id on every memory.
func (s *Service) UserID() string { return s.userID }

// engineErr turns the engine client's errors into the service's.
func (s *Service) engineErr(err error) error {
	var api *engine.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrUnavailable) && s.noEngine != "":
		return fmt.Errorf("%w: %s", engine.ErrUnavailable, s.noEngine)
	case errors.As(err, &api) && api.Code == "NOT_FOUND":
		return ErrNotFound
	case errors.As(err, &api) && api.Status == 400:
		return fmt.Errorf("%w: %s", ErrBadRequest, api.Message)
	}
	return err
}

func (s *Service) needDatapack(ctx context.Context, id string) error {
	ok, err := s.st.DatapackExists(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoDatapack
	}
	return nil
}

// AddMemory asks for a memory to be stored verbatim, as prod's memory_add does (no LLM).
type AddMemory struct {
	DatapackID     string
	AgentID        string
	ConversationID string
	Text           string
	// Messages, if set, are stored instead of Text: each non-system one becomes a memory.
	Messages []engine.Message
	Metadata map[string]any
}

// AddMemory stores memories verbatim and returns what it added.
func (s *Service) AddMemory(ctx context.Context, r AddMemory) ([]engine.Added, error) {
	if err := s.needDatapack(ctx, r.DatapackID); err != nil {
		return nil, err
	}
	msgs := r.Messages
	if len(msgs) == 0 {
		msgs = []engine.Message{{Role: "user", Content: r.Text}}
	}
	added, err := s.eng.Add(ctx, engine.AddRequest{
		DatapackID: r.DatapackID, UserID: s.userID, AgentID: r.AgentID, ConversationID: r.ConversationID,
		Messages: msgs, Metadata: r.Metadata,
	})
	return added, s.engineErr(err)
}

// Search asks for a datapack's memories that best match a query.
type Search struct {
	DatapackID     string
	AgentID        string // optional: prod's memory_search searches across agents
	ConversationID string // optional: only memories from this conversation
	Query          string
	Limit          int
}

// SearchMemories is the best memories for a query, best first, with prod's 0-1 score and the
// cosine as raw_similarity_score.
func (s *Service) SearchMemories(ctx context.Context, r Search) ([]engine.Memory, error) {
	if err := s.needDatapack(ctx, r.DatapackID); err != nil {
		return nil, err
	}
	res, err := s.eng.Search(ctx, engine.SearchRequest{
		DatapackID: r.DatapackID, UserID: s.userID, AgentID: r.AgentID, ConversationID: r.ConversationID,
		Query: r.Query, Limit: r.Limit,
	})
	return res, s.engineErr(err)
}

// UpdateMemory replaces a memory of the local user's, or returns ErrPromoted for one promoted.
func (s *Service) UpdateMemory(ctx context.Context, datapack, id, text string) error {
	if err := s.promoted(ctx, datapack, id); err != nil {
		return err
	}
	if _, err := s.own(ctx, datapack, id); err != nil {
		return err
	}
	return s.engineErr(s.eng.Update(ctx, datapack, id, text))
}

// DeleteMemory deletes a memory of the local user's, or returns ErrPromoted for one promoted.
func (s *Service) DeleteMemory(ctx context.Context, datapack, id string) error {
	if err := s.promoted(ctx, datapack, id); err != nil {
		return err
	}
	if _, err := s.own(ctx, datapack, id); err != nil {
		return err
	}
	return s.engineErr(s.eng.Delete(ctx, datapack, id))
}

// GetMemory is one of the local user's memories in the datapack, or ErrNotFound.
func (s *Service) GetMemory(ctx context.Context, datapack, id string) (engine.Memory, error) {
	rec, err := s.own(ctx, datapack, id)
	if err != nil {
		return engine.Memory{}, err
	}
	return fromRecord(datapack, *rec), nil
}

// own is the stored memory if it's the local user's, else ErrNotFound: get, update and delete
// reach only the memories search and list can.
func (s *Service) own(ctx context.Context, datapack, id string) (*store.Record, error) {
	rec, err := s.st.GetVector(ctx, store.Memories, datapack, id)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Payload["user_id"] != s.userID {
		return nil, ErrNotFound
	}
	return rec, nil
}

// ListMemories is the datapack's memories of the local user that match f, oldest first, up to
// limit, and how many match in all.
func (s *Service) ListMemories(ctx context.Context, datapack string, f store.Filter, limit int) (
	[]engine.Memory, int, error) {
	scoped := store.Filter{"user_id": s.userID}
	if len(f) > 0 {
		scoped["AND"] = []any{map[string]any(f)}
	}
	recs, total, err := s.st.ListVectors(ctx, store.Memories, datapack, scoped, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]engine.Memory, len(recs))
	for i, r := range recs {
		out[i] = fromRecord(datapack, r)
	}
	return out, total, nil
}

// fromRecord is a stored memory in prod's reply shape, as the engine's search returns them.
func fromRecord(datapack string, r store.Record) engine.Memory {
	p := r.Payload
	str := func(k string) string {
		s, _ := p[k].(string)
		return s
	}
	m := engine.Memory{
		ID: r.ID, Memory: str("data"), Hash: str("hash"), CreatedAt: str("createdAt"), UpdatedAt: str("updatedAt"),
		UserID: str("user_id"), AgentID: str("agent_id"), RunID: str("meko_conversation_id"), ActorID: str("actor_id"),
		Role: str("role"), AttributedTo: str("attributedTo"), ExpirationDate: str("expiration_date"),
	}
	md := map[string]any{}
	for k, v := range p {
		switch k {
		case "data", "hash", "createdAt", "updatedAt", "textLemmatized", "user_id", "agent_id", "role", "actor_id",
			"attributedTo", "expiration_date":
		default:
			md[k] = v
		}
	}
	md["meko_datapack_id"] = datapack
	md["meko_user_id"] = m.UserID
	if m.AgentID != "" {
		md["meko_agent_id"] = m.AgentID
	}
	m.Metadata = md
	return m
}

// EngineStatus is the engine's state for status reports: starting (no answer yet), loading (its
// model), ready, error (it says why), restarting (it crashed and the supervisor restarts it),
// failed (it crashed too often and isn't restarted), or unavailable (it can't be started).
type EngineStatus struct {
	State    string           `json:"state"`
	Reason   string           `json:"reason,omitempty"` // why it isn't ready
	Embedder *engine.Embedder `json:"embedder,omitempty"`
	Reranker *engine.Reranker `json:"reranker,omitempty"` // the knowledge base's; the engine is ready without it
}

// EngineStatus asks the engine how it is.
func (s *Service) EngineStatus(ctx context.Context) EngineStatus {
	if s.noEngine != "" {
		return EngineStatus{State: "unavailable", Reason: s.noEngine}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	h, err := s.eng.Health(ctx)
	if err != nil {
		if s.child != nil {
			if c, ok := s.child(); ok && c.State == supervisor.Failed {
				return EngineStatus{State: "failed", Reason: c.LastError}
			} else if ok && c.State == supervisor.Backoff {
				return EngineStatus{State: "restarting", Reason: c.LastError}
			}
		}
		return EngineStatus{State: "starting", Reason: "not answering yet"}
	}
	return EngineStatus{State: h.State, Reason: h.Error, Embedder: &h.Embedder, Reranker: h.Reranker}
}

// DeleteAllMemories deletes the local user's memories in a datapack written by one agent,
// optionally only those of one conversation, and says how many it deleted. Each goes through the
// engine, which also unlinks it from mem0's entities.
func (s *Service) DeleteAllMemories(ctx context.Context, datapack, agentID, conversationID string) (int, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return 0, err
	}
	ids, err := s.st.MemoryIDs(ctx, datapack, s.userID, agentOr(agentID), conversationID)
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if err := s.engineErr(s.eng.Delete(ctx, datapack, id)); err != nil && !errors.Is(err, ErrNotFound) {
			return i, err
		}
	}
	return len(ids), nil
}

// Page asks for a page of the local user's memories, newest first.
type Page struct {
	DatapackID string
	AgentID    string // "" for every agent's
	Scope      string // store.ScopeLearnings for learnings only; "" or store.ScopeMemories for all
	Query      string // a case-insensitive substring of the text
	Page       int    // from 1
	PageSize   int
}

// ListMemoryPage is a page of the local user's memories and how many match in all.
func (s *Service) ListMemoryPage(ctx context.Context, p Page) ([]engine.Memory, int, error) {
	if err := s.needDatapack(ctx, p.DatapackID); err != nil {
		return nil, 0, err
	}
	recs, total, err := s.st.ListMemoryPage(ctx, store.MemoryPage{DatapackID: p.DatapackID, UserID: s.userID,
		AgentID: p.AgentID, Scope: p.Scope, Query: p.Query, Limit: p.PageSize, Offset: (max(p.Page, 1) - 1) * p.PageSize})
	if err != nil {
		return nil, 0, err
	}
	out := make([]engine.Memory, len(recs))
	for i, r := range recs {
		out[i] = fromRecord(p.DatapackID, r)
	}
	return out, total, nil
}
