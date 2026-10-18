// Package memory is Meko Local's memories and conversations as Go calls: what milestone 3's MCP
// tools and REST routes will serve. Model work (embedding, and mem0's add and search) goes to the
// Node engine; reads that need no model come straight from the store.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yugabyte/meko-local/backend/internal/engine"
	"github.com/yugabyte/meko-local/backend/internal/store"
)

// Errors the service returns, besides the engine's (engine.ErrUnavailable, *engine.NotReadyError).
var (
	ErrNoDatapack = errors.New("no such datapack")
	ErrNotFound   = errors.New("no such memory")
	ErrBadRequest = errors.New("bad request")
)

// Service is the memory and conversation operations of one install.
type Service struct {
	st       *store.Store
	eng      *engine.Client
	userID   string
	log      *slog.Logger
	noEngine string
}

// Options configure a Service.
type Options struct {
	UserID string // the install's local user
	Log    *slog.Logger
	// NoEngine, if set, says why serve couldn't start the engine (no Node, or it isn't built).
	NoEngine string
}

// New is the service over st and the engine eng.
func New(st *store.Store, eng *engine.Client, o Options) *Service {
	return &Service{st: st, eng: eng, userID: o.UserID, log: o.Log, noEngine: o.NoEngine}
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

// UpdateMemory replaces a memory's text.
func (s *Service) UpdateMemory(ctx context.Context, datapack, id, text string) error {
	return s.engineErr(s.eng.Update(ctx, datapack, id, text))
}

// DeleteMemory deletes a memory.
func (s *Service) DeleteMemory(ctx context.Context, datapack, id string) error {
	return s.engineErr(s.eng.Delete(ctx, datapack, id))
}

// GetMemory is one memory of the datapack, or ErrNotFound.
func (s *Service) GetMemory(ctx context.Context, datapack, id string) (engine.Memory, error) {
	rec, err := s.st.GetVector(ctx, store.Memories, datapack, id)
	if err != nil {
		return engine.Memory{}, err
	}
	if rec == nil {
		return engine.Memory{}, ErrNotFound
	}
	return fromRecord(datapack, *rec), nil
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
// model), ready, error, or unavailable (with why).
type EngineStatus struct {
	State    string           `json:"state"`
	Reason   string           `json:"reason,omitempty"` // why it isn't ready
	Embedder *engine.Embedder `json:"embedder,omitempty"`
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
		return EngineStatus{State: "starting", Reason: "not answering yet"}
	}
	return EngineStatus{State: h.State, Reason: h.Error, Embedder: &h.Embedder}
}
