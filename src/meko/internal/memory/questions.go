package memory

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// Context Gaps: the searches Meko answered poorly, grouped by question, so someone can see what
// Meko is missing and teach it (docs/api.md, "Context Gaps"). No model judges a reply: the rules
// flag only what the scores can show.

// Question qualities: how poor a search's answer was.
const (
	// QualityEmpty is a search that found nothing.
	QualityEmpty = "empty"
	// QualityWeak is one that found only memories, each scored below WeakMemoryScore.
	QualityWeak = "weak"
)

// Question states.
const (
	QuestionOpen      = "open"
	QuestionDismissed = "dismissed"
	QuestionTaught    = "taught"
)

// WeakMemoryScore is the memory score (the fused score, not the cosine) below which a memory
// doesn't answer a question. Tuned on real searches: ten off-topic near-duplicates scored 0.74 to
// 0.76 while the relevant memory scored 0.77, so the bar flags only what's clearly noise.
const WeakMemoryScore = 0.75

// QuestionPage is how many questions a page of the list holds.
const QuestionPage = 100

// questionNamed is how many agents and conversations a question names.
const questionNamed = 20

// MaxQueryChars is the most of a query a question keeps.
const MaxQueryChars = 1000

// MaxTaughtChars is the most a person may teach Meko about a question at once.
const MaxTaughtChars = 2000

// ErrNoQuestion is a question the datapack doesn't have.
var ErrNoQuestion = errors.New("no such question")

// QuestionKey groups the asks of one question: the query lower-cased with its whitespace collapsed.
func QuestionKey(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

// Answer is a search's reply as the rules judge it.
type Answer struct {
	Memories []memoryengine.Memory
	// Strong counts the knowledge-base chunks and conversation hits: they passed Meko's own distance
	// cutoffs, so one that comes back is a real hit.
	Strong int
	// OnlyEmpty is a search whose results are never judged weak: knowledgebase_search keeps at least
	// one chunk whatever its distance, so it's only ever empty.
	OnlyEmpty bool
}

// Quality is how poor an answer was: QualityEmpty when it found nothing, QualityWeak when it found
// only memories each with a score below WeakMemoryScore, and "" when it found something relevant (a
// memory with no score counts as relevant).
func (a Answer) Quality() string {
	if len(a.Memories) == 0 && a.Strong == 0 {
		return QualityEmpty
	}
	if a.OnlyEmpty || a.Strong > 0 {
		return ""
	}
	for _, m := range a.Memories {
		if m.Score == nil || *m.Score >= WeakMemoryScore {
			return ""
		}
	}
	return QualityWeak
}

// AskedSearch is one search for RecordSearch: what was asked, by whom, and its answer.
type AskedSearch struct {
	DatapackID     string
	Query          string
	Tool           string
	AgentID        string
	ConversationID string
	At             time.Time
	Answer         Answer
}

// RecordSearch records a search for Context Gaps: a poor one as an ask of its question, and one
// that found something relevant as answering its question, if the datapack has it. The search
// already happened, so a failure to record it is logged, not returned.
func (s *Service) RecordSearch(ctx context.Context, a AskedSearch) {
	key := QuestionKey(a.Query)
	if key == "" || a.DatapackID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if q := a.Answer.Quality(); q == "" {
		err = s.st.AnswerQuestion(ctx, a.DatapackID, key, a.At)
	} else {
		query := a.Query
		if r := []rune(query); len(r) > MaxQueryChars {
			query = string(r[:MaxQueryChars])
		}
		ask := store.Ask{DatapackID: a.DatapackID, Key: key, At: a.At, Query: query, Quality: q, Tool: a.Tool,
			AgentID: a.AgentID, ConversationID: a.ConversationID,
			Results: len(a.Answer.Memories) + a.Answer.Strong}
		for _, m := range a.Answer.Memories {
			ask.TopScore = maxOf(ask.TopScore, m.Score)
			ask.TopRaw = maxOf(ask.TopRaw, m.RawSimilarityScore)
		}
		err = s.st.AddAsk(ctx, ask)
	}
	if err != nil && s.log != nil {
		s.log.Warn("couldn't record a search for Context Gaps", "tool", a.Tool, "error", err)
	}
}

func maxOf(cur, v *float64) *float64 {
	if v == nil || (cur != nil && *cur >= *v) {
		return cur
	}
	x := *v
	return &x
}

// Question is one of a datapack's questions as Context Gaps lists it: the query as last asked, how
// its latest ask went, who asked and when, and what was done about it.
type Question struct {
	Key      string
	Query    string
	Quality  string
	Tool     string
	Results  int
	TopScore *float64
	TopRaw   *float64
	Asks     int
	// Agents and Conversations are who asked it, latest first, at most 20 each.
	Agents        []string
	Conversations []string
	// AskedBy says whether the conversation the list was asked for asked it.
	AskedBy  bool
	FirstAt  time.Time
	LastAt   time.Time
	State    string
	MarkedAt *time.Time
	Taught   *Taught
	// Dismissal is set while the question is dismissed.
	Dismissal *Dismissal
}

// Taught is what was taught about a question, and by whom: an agent's conversation, or "" for a
// person.
type Taught struct {
	Text   string
	Memory string
	At     time.Time
	By     string
}

// Dismissal is who dismissed a question (an agent's conversation, or "" for a person) and the
// memory that already said the answer, with its text while the datapack still has it.
type Dismissal struct {
	By         string
	Memory     string
	MemoryText string
}

// Questions is a page of a datapack's questions.
type Questions struct {
	Questions []Question
	More      bool
	// Counts is how many questions are in each state; Open how many open ones by quality.
	Counts map[string]int
	Open   map[string]int
	// Searches is how many searches the call log has for the datapack (90 days), since its first.
	Searches int
	Since    *time.Time
}

// QuestionCursor is where a page starts: after the question with this LastAt and Key.
type QuestionCursor struct {
	LastAt time.Time
	Key    string
}

// QuestionFilter picks the questions of a page.
type QuestionFilter struct {
	// Cloud says the datapack is the linked Meko Cloud's: its searches counted are the ones this Meko
	// passed on to Meko Cloud.
	Cloud bool
	State string // a state, or "all"
	After *QuestionCursor
	// Conversation sets each question's AskedBy.
	Conversation string
}

// ListQuestions is a page of a datapack's questions in a state, latest asked first (ties by key).
// A question whose latest ask was followed by a search that found something relevant is left out:
// Meko knows it now. The questions routes check the datapack first (this Meko's, or the linked Meko
// Cloud's), as they do for MarkQuestion and ForgetAsks.
func (s *Service) ListQuestions(ctx context.Context, datapack string, f QuestionFilter) (Questions, error) {
	all, err := s.questions(ctx, datapack, f.Conversation)
	if err != nil {
		return Questions{}, err
	}
	out := Questions{Questions: []Question{},
		Counts: map[string]int{QuestionOpen: 0, QuestionDismissed: 0, QuestionTaught: 0},
		Open:   map[string]int{QualityEmpty: 0, QualityWeak: 0}}
	for _, q := range all {
		out.Counts[q.State]++
		if q.State == QuestionOpen {
			out.Open[q.Quality]++
		}
		if f.State != "all" && q.State != f.State {
			continue
		}
		if a := f.After; a != nil && !q.LastAt.Before(a.LastAt) && (!q.LastAt.Equal(a.LastAt) || q.Key <= a.Key) {
			continue
		}
		if len(out.Questions) == QuestionPage {
			out.More = true
			continue
		}
		out.Questions = append(out.Questions, q)
	}
	for i := range out.Questions {
		s.dismissedText(ctx, datapack, &out.Questions[i])
	}
	since := time.Now().Add(-CallRetention)
	var calls []store.Call
	if f.Cloud {
		calls, err = s.st.CloudCalls(ctx, since)
	} else {
		calls, err = s.st.Calls(ctx, datapack, since)
	}
	if err != nil {
		return Questions{}, err
	}
	for _, c := range calls {
		if _, search := searchTools[c.Tool]; search && c.OK && c.AgentID != WebUIAgent && c.DatapackID == datapack {
			if out.Searches == 0 {
				at := c.At
				out.Since = &at
			}
			out.Searches++
		}
	}
	return out, nil
}

// GetQuestion is one of a datapack's questions by its key, or ErrNoQuestion.
func (s *Service) GetQuestion(ctx context.Context, datapack, key string) (Question, error) {
	all, err := s.questions(ctx, datapack, "")
	if err != nil {
		return Question{}, err
	}
	for _, q := range all {
		if q.Key == key {
			s.dismissedText(ctx, datapack, &q)
			return q, nil
		}
	}
	return Question{}, ErrNoQuestion
}

// dismissedText fills in the text of the memory a dismissed question names, only for the questions
// a reply holds. A memory deleted since, one that isn't the local user's, or a Meko Cloud datapack's
// (not on this Meko) just has no text.
func (s *Service) dismissedText(ctx context.Context, datapack string, q *Question) {
	if q.Dismissal == nil || q.Dismissal.Memory == "" {
		return
	}
	if mem, err := s.GetMemory(ctx, datapack, q.Dismissal.Memory); err == nil {
		q.Dismissal.MemoryText = mem.Memory
	}
}

// questions is every question of a datapack still on the list, in the list's order.
func (s *Service) questions(ctx context.Context, datapack, conversation string) ([]Question, error) {
	asks, err := s.st.Asks(ctx, datapack)
	if err != nil {
		return nil, err
	}
	marks, err := s.st.Questions(ctx, datapack)
	if err != nil {
		return nil, err
	}
	byKey := map[string]store.Question{}
	for _, m := range marks {
		byKey[m.Key] = m
	}
	groups := map[string][]store.Ask{}
	order := []string{}
	for _, a := range asks { // latest first
		if _, ok := groups[a.Key]; !ok {
			order = append(order, a.Key)
		}
		groups[a.Key] = append(groups[a.Key], a)
	}
	out := []Question{}
	for _, key := range order {
		g := groups[key]
		latest := g[0]
		m := byKey[key]
		if m.AnsweredAt != nil && m.AnsweredAt.After(latest.At) {
			continue
		}
		q := Question{Key: key, Query: latest.Query, Quality: latest.Quality, Tool: latest.Tool,
			Results: latest.Results, TopScore: latest.TopScore, TopRaw: latest.TopRaw, Asks: len(g),
			Agents: []string{}, Conversations: []string{}, FirstAt: g[len(g)-1].At, LastAt: latest.At}
		for _, a := range g {
			if a.AgentID != "" && len(q.Agents) < questionNamed && !contains(q.Agents, a.AgentID) {
				q.Agents = append(q.Agents, a.AgentID)
			}
			if a.ConversationID != "" && len(q.Conversations) < questionNamed &&
				!contains(q.Conversations, a.ConversationID) {
				q.Conversations = append(q.Conversations, a.ConversationID)
			}
			if conversation != "" && a.ConversationID == conversation {
				q.AskedBy = true
			}
		}
		q.State, q.MarkedAt = stateOf(m, latest.At)
		if m.TaughtAt != nil {
			q.Taught = &Taught{Text: m.TaughtText, Memory: m.TaughtMemory, At: *m.TaughtAt, By: m.TaughtBy}
		}
		if q.State == QuestionDismissed {
			q.Dismissal = &Dismissal{By: m.DismissedBy, Memory: m.DismissedMemory}
		}
		out = append(out, q)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastAt.Equal(out[j].LastAt) {
			return out[i].LastAt.After(out[j].LastAt)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// stateOf is a question's state from what was done about it: the latest of dismissed, taught and
// opened at or after its last ask, else open. A question asked again after a mark is open again.
func stateOf(m store.Question, lastAt time.Time) (string, *time.Time) {
	var state string
	var at *time.Time
	for _, a := range []struct {
		state string
		at    *time.Time
	}{{QuestionDismissed, m.DismissedAt}, {QuestionTaught, m.TaughtAt}, {QuestionOpen, m.OpenedAt}} {
		if a.at != nil && (at == nil || a.at.After(*at)) {
			state, at = a.state, a.at
		}
	}
	if at == nil || at.Before(lastAt) || state == QuestionOpen {
		return QuestionOpen, nil
	}
	return state, at
}

// MarkQuestion records something done about a question: dismissed, taught, or opened again (state
// QuestionOpen, a restore: a dismissal is cleared, what was taught is kept). By is the conversation
// of the agent that did it, "" for a person. ErrNoQuestion when the datapack has no such question.
func (s *Service) MarkQuestion(ctx context.Context, datapack, key string, m store.QuestionMark) error {
	if m.At.IsZero() {
		m.At = time.Now()
	}
	err := s.st.MarkQuestion(ctx, datapack, key, m)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNoQuestion
	}
	return err
}

// ForgetAsks deletes one conversation's asks in a datapack made before a time, such as an agent's
// searches while it filled a new datapack, which aren't questions Meko should be taught, and says
// how many it deleted.
func (s *Service) ForgetAsks(ctx context.Context, datapack, conversation string, before time.Time) (int64, error) {
	return s.st.DeleteAsks(ctx, datapack, conversation, before)
}

// AnswerOf is a search tool's reply text as the rules judge it, for a search this Meko passed on to
// the linked Meko Cloud, whose reply it has only as text. It reads Meko Cloud's shapes loosely, as
// this Meko's own replies are built (internal/wire): context_search's {results: {memory,
// knowledge_base, conversation: {hits} or [hits]}} (or the buckets unwrapped), memory_search's
// {results: [memories]} (or the list alone), and knowledgebase_search's {results: [chunks]}. ok is
// false for a reply it can't read, which isn't judged.
func AnswerOf(tool, text string) (a Answer, ok bool) {
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		return Answer{}, false
	}
	obj, _ := v.(map[string]any)
	list := func(x any) []any { l, _ := x.([]any); return l }
	memories := func(items []any) []memoryengine.Memory {
		out := make([]memoryengine.Memory, 0, len(items))
		for _, it := range items {
			m, _ := it.(map[string]any)
			var mem memoryengine.Memory
			if f, isNum := m["score"].(float64); isNum {
				mem.Score = &f
			}
			if f, isNum := m["raw_similarity_score"].(float64); isNum {
				mem.RawSimilarityScore = &f
			}
			out = append(out, mem)
		}
		return out
	}
	switch tool {
	case "context_search":
		box, _ := obj["results"].(map[string]any)
		if box == nil {
			box = obj
		}
		_, hasMem := box["memory"]
		_, hasKB := box["knowledge_base"]
		_, hasConv := box["conversation"]
		if !hasMem && !hasKB && !hasConv {
			return Answer{}, false
		}
		a.Memories = memories(list(box["memory"]))
		a.Strong = len(list(box["knowledge_base"]))
		if conv, isObj := box["conversation"].(map[string]any); isObj {
			a.Strong += len(list(conv["hits"]))
		} else {
			a.Strong += len(list(box["conversation"]))
		}
	case "memory_search":
		items, isList := v.([]any)
		if !isList {
			if r, found := obj["results"]; found {
				items, isList = r.([]any)
			} else {
				items, isList = obj["memories"].([]any)
			}
		}
		if !isList {
			return Answer{}, false
		}
		a.Memories = memories(items)
	case "knowledgebase_search":
		items, isList := obj["results"].([]any)
		if !isList {
			return Answer{}, false
		}
		a.Strong, a.OnlyEmpty = len(items), true
	default:
		return Answer{}, false
	}
	return a, true
}
