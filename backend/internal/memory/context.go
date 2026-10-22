package memory

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/store"
)

// MinRawSimilarity is the cosine below which memory_search (and context_search when it searches
// memories only) drops a memory as a weak match, as Meko Cloud does by default.
const MinRawSimilarity = 0.2

// ContextSearch asks for what a datapack knows about a query, from each source asked for.
type ContextSearch struct {
	DatapackID     string
	AgentID        string
	ConversationID string // the caller's conversation, left out of conversation search
	Query          string
	Limit          int // per source
	Memory         bool
	Conversation   bool
	KnowledgeBase  bool
	// MinRawSimilarity, if set, drops memories whose cosine is below it.
	MinRawSimilarity float64
}

// ContextResults is each source's results. A nil source wasn't searched: it wasn't asked for, or
// it couldn't be (the engine isn't ready). An empty one was, and found nothing.
type ContextResults struct {
	Memory        []engine.Memory
	Conversation  *ConversationResults
	KnowledgeBase []KBResult
	// KnowledgeChunks is the datapack's knowledge-base chunks, the savings estimate's baseline.
	KnowledgeChunks int
	Steps           []store.Step
}

// ContextSearch searches the sources asked for at the same time, so it takes as long as the
// slowest. A source that fails is logged and left empty (or nil when the engine can't embed), as
// Meko Cloud does: the caller still gets the others.
func (s *Service) ContextSearch(ctx context.Context, r ContextSearch) (ContextResults, error) {
	if err := s.needDatapack(ctx, r.DatapackID); err != nil {
		return ContextResults{}, err
	}
	var (
		out ContextResults
		mu  sync.Mutex
		wg  sync.WaitGroup
	)
	step := func(name string, start time.Time, n int) {
		mu.Lock()
		defer mu.Unlock()
		out.Steps = append(out.Steps, store.Step{Step: name, Ms: time.Since(start).Milliseconds(), N: n})
	}
	if r.Memory {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			res, err := s.SearchMemories(ctx, Search{DatapackID: r.DatapackID, Query: r.Query, Limit: r.Limit})
			if err != nil {
				res = s.failedSource("memory", err)
			} else if r.MinRawSimilarity > 0 {
				res = aboveRaw(res, r.MinRawSimilarity)
			}
			step("memory_search", start, len(res))
			mu.Lock()
			out.Memory = res
			mu.Unlock()
		}()
	}
	if r.KnowledgeBase {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			res, err := s.SearchKnowledge(ctx, r.DatapackID, r.Query, r.Limit)
			if err != nil {
				if s.sourceUnavailable("knowledge_base", err) {
					return
				}
				res = []KBResult{}
			}
			kept := []KBResult{}
			for _, k := range res {
				if k.Distance <= s.kbMaxDistance {
					kept = append(kept, k)
				}
			}
			step("knowledge_base_search", start, len(kept))
			mu.Lock()
			out.KnowledgeBase = kept
			mu.Unlock()
		}()
	}
	if r.Conversation {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			res, err := s.SearchConversations(ctx, r.DatapackID, r.ConversationID, r.Query)
			if err != nil {
				if s.failedSource("conversation", err) == nil {
					return
				}
				res = ConversationResults{Hits: []ConversationHit{}, Thread: []ThreadTurn{}}
			}
			step("conversation_search", start, len(res.Hits))
			mu.Lock()
			out.Conversation = &res
			mu.Unlock()
		}()
	}
	wg.Wait()
	chunks, _, err := s.st.KnowledgeCounts(ctx, r.DatapackID)
	if err != nil {
		return ContextResults{}, err
	}
	out.KnowledgeChunks = chunks
	return out, nil
}

// failedSource logs a source that failed and returns what it reports: nil (not searched) when the
// engine couldn't take the query, else empty.
func (s *Service) failedSource(source string, err error) []engine.Memory {
	if s.sourceUnavailable(source, err) {
		return nil
	}
	return []engine.Memory{}
}

// sourceUnavailable logs a source that failed and says whether it couldn't be searched at all (the
// engine couldn't take the query), which context_search reports as null rather than empty.
func (s *Service) sourceUnavailable(source string, err error) bool {
	if s.log != nil {
		s.log.Warn("a context_search source failed", "source", source, "error", err)
	}
	var notReady *engine.NotReadyError
	return errors.Is(err, engine.ErrUnavailable) || errors.As(err, &notReady)
}

// aboveRaw keeps the memories whose cosine (or, without one, score) reaches min.
func aboveRaw(ms []engine.Memory, minimum float64) []engine.Memory {
	out := []engine.Memory{}
	for _, m := range ms {
		v := m.RawSimilarityScore
		if v == nil {
			v = m.Score
		}
		if v != nil && *v >= minimum {
			out = append(out, m)
		}
	}
	return out
}

// FilterRaw keeps the memories whose cosine reaches MinRawSimilarity, as memory_search does.
func FilterRaw(ms []engine.Memory) []engine.Memory { return aboveRaw(ms, MinRawSimilarity) }
