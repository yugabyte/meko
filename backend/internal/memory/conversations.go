package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yugabyte/meko-local/backend/internal/store"
)

// Conversation search and turn indexing follow prod Meko's parameters.
const (
	// DefaultAgentID is the agent of a call that names none.
	DefaultAgentID = "meko_agent"
	// substantive is how long (in characters, after trimming) a turn's input or output must be for
	// the turn to be searchable.
	substantive = 40
	// chunkSize and chunkOverlap cut a turn's text into windows for embedding.
	chunkSize    = 1024
	chunkOverlap = 256
	// searchCandidates is how many nearest chunks a conversation search looks at.
	searchCandidates = 10
	// threadConversations is how many matched conversations come back whole, and threadTurns the
	// most turns they bring in all (the most recent kept).
	threadConversations = 3
	threadTurns         = 50
	// DefaultMaxDistance is the farthest (cosine distance) a chunk may be from the query to be a hit.
	// Prod's 0.35 suits its embedding models; with all-MiniLM-L6-v2 a paraphrased question measured
	// 0.52 from the turn that answered it, and unrelated turns 0.98 and more.
	DefaultMaxDistance = 0.6
)

// More errors the conversation calls return.
var (
	ErrAgentMismatch = errors.New("the conversation belongs to another agent")
)

// hexID is a 32-hex id: the seed itself when it already is one (hyphens dropped), the first 32 hex
// characters of the seed's SHA-256 otherwise, or random for no seed. Prod derives conversation ids
// from session ids and message ids from seeds this way, so the same seed is the same row.
func hexID(seed string) string {
	if seed == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	if s := strings.ToLower(strings.ReplaceAll(seed, "-", "")); len(s) == 32 && isHex(s) {
		return s
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// messageID is a turn's id from the caller's seed: always hashed (unlike a session id), so a seed
// that happens to be 32 hex characters still names its own turn.
func messageID(seed string) string {
	if seed == "" {
		return hexID("")
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// normalizeID is a conversation id as callers may write it (with hyphens, upper case).
func normalizeID(id string) string { return strings.ToLower(strings.ReplaceAll(id, "-", "")) }

func agentOr(agentID string) string {
	if agentID == "" {
		return DefaultAgentID
	}
	return agentID
}

// CreateConversation asks for a conversation.
type CreateConversation struct {
	AgentID    string
	DatapackID string // "" for none
	SessionID  string // the same session id is the same conversation
	RunID      string
	Title      string
	Metadata   map[string]any
}

// CreateConversation makes a conversation, or returns the datapack's existing one for the session.
func (s *Service) CreateConversation(ctx context.Context, r CreateConversation) (store.Conversation, error) {
	if strings.TrimSpace(r.Title) == "" {
		return store.Conversation{}, fmt.Errorf("%w: a conversation needs a title", ErrBadRequest)
	}
	c, _, err := s.st.CreateConversation(ctx, store.Conversation{
		ID: hexID(r.SessionID), DatapackID: r.DatapackID, AgentID: agentOr(r.AgentID), SessionID: r.SessionID,
		RunID: r.RunID, Title: r.Title, Metadata: r.Metadata,
	})
	if errors.Is(err, store.ErrNoDatapack) {
		return store.Conversation{}, ErrNoDatapack
	}
	return c, err
}

// conversation is the caller's conversation, checking it's theirs.
func (s *Service) conversation(ctx context.Context, datapack, id, agentID string) (store.Conversation, error) {
	c, err := s.st.GetConversation(ctx, datapack, normalizeID(id))
	if errors.Is(err, store.ErrNotFound) {
		return c, ErrNotFound
	}
	if err == nil && c.AgentID != agentOr(agentID) {
		return c, ErrAgentMismatch
	}
	return c, err
}

// AddMessage asks for a turn to be added to a conversation.
type AddMessage struct {
	ConversationID string
	AgentID        string
	DatapackID     string
	Input, Output  string
	Reasoning      string
	Plan           []string
	Metadata       map[string]any
	Seed           string // the same seed is the same turn
	// SkipSearchEmbed keeps the turn out of conversation search.
	SkipSearchEmbed bool
}

// AddedMessage is what adding a turn did. SearchSkipped says why the turn won't be searchable,
// as prod's reply does: "datapack_opted_out", "caller_skip_search_embed" or "no_datapack".
type AddedMessage struct {
	Status         string `json:"status"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	SearchSkipped  string `json:"conversation_search_skipped,omitempty"`
}

// AddMessage stores a turn and, when it's searchable, its chunks, which the embedder sweep embeds.
// It returns at once: nothing here waits on the engine.
func (s *Service) AddMessage(ctx context.Context, r AddMessage) (AddedMessage, error) {
	c, err := s.conversation(ctx, r.DatapackID, r.ConversationID, r.AgentID)
	if err != nil {
		return AddedMessage{}, err
	}
	out := AddedMessage{Status: "accepted", ConversationID: c.ID}
	var chunks []string
	if isSubstantive(r.Input, r.Output) {
		switch {
		case c.DatapackID == "":
			out.SearchSkipped = "no_datapack"
		case r.SkipSearchEmbed:
			out.SearchSkipped = "caller_skip_search_embed"
		default:
			optOut, oerr := s.st.ConversationSearchOptOut(ctx, c.DatapackID)
			if oerr != nil {
				return AddedMessage{}, oerr
			}
			if optOut {
				out.SearchSkipped = "datapack_opted_out"
			} else {
				chunks = chunk(strings.TrimSpace(r.Input + "\n\n" + r.Output))
			}
		}
	}
	m, _, err := s.st.AddMessage(ctx, c.DatapackID, store.Message{
		ID: messageID(r.Seed), ConversationID: c.ID, Input: r.Input, Output: r.Output, Reasoning: r.Reasoning,
		Plan: r.Plan, Metadata: r.Metadata,
	}, chunks)
	if err != nil {
		return AddedMessage{}, err
	}
	out.MessageID = m.ID
	if len(chunks) > 0 {
		s.kickEmbedder()
	}
	return out, nil
}

func isSubstantive(input, output string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(input)) >= substantive ||
		utf8.RuneCountInString(strings.TrimSpace(output)) >= substantive
}

// chunk cuts text into windows of chunkSize characters, each starting chunkSize-chunkOverlap after
// the last.
func chunk(text string) []string {
	r := []rune(text)
	if len(r) <= chunkSize {
		return []string{text}
	}
	var out []string
	for start := 0; ; start += chunkSize - chunkOverlap {
		end := min(start+chunkSize, len(r))
		out = append(out, string(r[start:end]))
		if end == len(r) {
			return out
		}
	}
}

// GetConversation is a conversation, with up to limit of its turns from offset when
// withMessages is set.
func (s *Service) GetConversation(ctx context.Context, datapack, id, agentID string, withMessages bool,
	limit, offset int) (store.Conversation, []store.Message, error) {
	c, err := s.conversation(ctx, datapack, id, agentID)
	if err != nil || !withMessages {
		return c, nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	msgs, err := s.st.Messages(ctx, datapack, c.ID, limit, offset)
	return c, msgs, err
}

// ListConversations is the datapack's conversations, most recently updated first (optionally one
// agent's), and whether there are more.
func (s *Service) ListConversations(ctx context.Context, datapack, agentID string, limit, offset int) (
	[]store.Conversation, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.st.ListConversations(ctx, datapack, agentID, limit, offset)
}

// UpdateConversation sets a conversation's title or metadata (at least one).
func (s *Service) UpdateConversation(ctx context.Context, datapack, id, agentID string, title *string,
	metadata map[string]any) (store.Conversation, error) {
	if title == nil && metadata == nil {
		return store.Conversation{}, fmt.Errorf("%w: give a title or metadata to change", ErrBadRequest)
	}
	if title != nil && strings.TrimSpace(*title) == "" {
		return store.Conversation{}, fmt.Errorf("%w: a conversation needs a title", ErrBadRequest)
	}
	c, err := s.conversation(ctx, datapack, id, agentID)
	if err != nil {
		return c, err
	}
	return s.st.UpdateConversation(ctx, datapack, c.ID, title, metadata)
}

// DeleteConversation deletes a conversation and its turns. Memories that name it stay, as in prod.
func (s *Service) DeleteConversation(ctx context.Context, datapack, id, agentID string) error {
	c, err := s.conversation(ctx, datapack, id, agentID)
	if err != nil {
		return err
	}
	return s.st.DeleteConversation(ctx, datapack, c.ID)
}

// SetConversationSearchOptOut opts a datapack out of conversation search (new turns aren't
// embedded; ones already embedded stay searchable), or back in.
func (s *Service) SetConversationSearchOptOut(ctx context.Context, datapack string, optOut bool) error {
	err := s.st.SetConversationSearchOptOut(ctx, datapack, optOut)
	if errors.Is(err, store.ErrNoDatapack) {
		return ErrNoDatapack
	}
	return err
}

// ConversationHit is a chunk of a past turn near the query.
type ConversationHit struct {
	Content        string   `json:"content"`
	Distance       float64  `json:"distance"`
	RerankScore    *float64 `json:"rerank_score"` // null until the cross-encoder arrives (milestone 4)
	ConversationID string   `json:"conversation_id"`
	MessageID      string   `json:"message_id"`
	UserID         string   `json:"user_id"`
}

// ThreadTurn is one turn of a matched conversation.
type ThreadTurn struct {
	Content        string   `json:"content"`
	Distance       *float64 `json:"distance"` // the turn's nearest chunk, if it was a candidate
	ConversationID string   `json:"conversation_id"`
	MessageID      string   `json:"message_id"`
	UserID         string   `json:"user_id"`
}

// ConversationResults is context_search's conversation bucket.
type ConversationResults struct {
	Hits   []ConversationHit `json:"hits"`
	Thread []ThreadTurn      `json:"thread"`
}

// SearchConversations finds past turns of the datapack near the query, outside the caller's own
// conversation: the hits within the distance cutoff, and the best few conversations whole.
func (s *Service) SearchConversations(ctx context.Context, datapack, exclude, query string) (
	ConversationResults, error) {
	if strings.TrimSpace(query) == "" {
		return ConversationResults{}, fmt.Errorf("%w: query must not be empty", ErrBadRequest)
	}
	if err := s.needDatapack(ctx, datapack); err != nil {
		return ConversationResults{}, err
	}
	vecs, err := s.eng.Embed(ctx, []string{query})
	if err != nil {
		return ConversationResults{}, s.engineErr(err)
	}
	cands, err := s.st.SearchChunks(ctx, datapack, vecs[0], searchCandidates, normalizeID(exclude))
	if err != nil {
		return ConversationResults{}, err
	}
	out := ConversationResults{Hits: []ConversationHit{}, Thread: []ThreadTurn{}}
	nearest := map[string]float64{} // message id -> its nearest candidate
	var convs []string
	for _, h := range cands {
		if d, ok := nearest[h.MessageID]; !ok || h.Distance < d {
			nearest[h.MessageID] = h.Distance
		}
		if h.Distance > s.maxDistance {
			continue
		}
		out.Hits = append(out.Hits, ConversationHit{Content: h.Text, Distance: h.Distance,
			ConversationID: h.ConversationID, MessageID: h.MessageID, UserID: s.userID})
		if !contains(convs, h.ConversationID) && len(convs) < threadConversations {
			convs = append(convs, h.ConversationID)
		}
	}
	var turns []store.Message
	for _, c := range convs {
		msgs, err := s.st.Messages(ctx, datapack, c, 1000, 0)
		if err != nil {
			return ConversationResults{}, err
		}
		for _, m := range msgs {
			if isSubstantive(m.Input, m.Output) {
				turns = append(turns, m)
			}
		}
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].CreatedAt.Before(turns[j].CreatedAt) })
	if len(turns) > threadTurns {
		turns = turns[len(turns)-threadTurns:]
	}
	for _, m := range turns {
		t := ThreadTurn{Content: strings.TrimSpace(m.Input + "\n\n" + m.Output), ConversationID: m.ConversationID,
			MessageID: m.ID, UserID: s.userID}
		if d, ok := nearest[m.ID]; ok {
			t.Distance = &d
		}
		out.Thread = append(out.Thread, t)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Embedder sweep timing: how many chunks one engine call embeds, and how often it looks for
// chunks left behind (the engine was down or loading when their turns arrived).
const (
	embedBatch = 64
	sweepEvery = 30 * time.Second
)

// kickEmbedder wakes the embedder sweep without waiting for it.
func (s *Service) kickEmbedder() {
	select {
	case s.kick <- struct{}{}:
	default: // already woken
	}
}

// RunEmbedder embeds turns' chunks for conversation search until ctx ends: whenever a turn is added,
// and every sweepEvery for any left behind. serve runs it.
func (s *Service) RunEmbedder(ctx context.Context) {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		if err := s.EmbedPending(ctx); err != nil && ctx.Err() == nil {
			s.log.Debug("chunks wait for the engine", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-tick.C:
		}
	}
}

// EmbedPending embeds every chunk still without a vector, a batch at a time.
func (s *Service) EmbedPending(ctx context.Context) error {
	for {
		pending, err := s.st.PendingChunks(ctx, embedBatch)
		if err != nil || len(pending) == 0 {
			return err
		}
		texts := make([]string, len(pending))
		ids := make([]int64, len(pending))
		for i, c := range pending {
			texts[i], ids[i] = c.Text, c.Rowid
		}
		vecs, err := s.eng.Embed(ctx, texts)
		if err != nil {
			return s.engineErr(err)
		}
		if err := s.st.PutChunkVectors(ctx, ids, vecs); err != nil {
			return err
		}
		if len(pending) < embedBatch {
			return nil
		}
	}
}
