package memory

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

// The local user's profile, kept in settings.
const (
	keyFirstName  = "profile_first_name"
	keyLastName   = "profile_last_name"
	keyEmail      = "profile_email"
	keyOnboarding = "profile_onboarding_done"
)

// Profile is the install's one user, as meko_ui shows them.
type Profile struct {
	FirstName, LastName, Email string
	OnboardingDone             bool
}

// Profile is the local user's profile: "Local User" with onboarding done until they change it
// (meko_ui's onboarding isn't part of the local web UI).
func (s *Service) Profile(ctx context.Context) (Profile, error) {
	p := Profile{FirstName: "Local", LastName: "User", OnboardingDone: true}
	for key, dst := range map[string]*string{keyFirstName: &p.FirstName, keyLastName: &p.LastName, keyEmail: &p.Email} {
		if v, ok, err := s.st.Setting(ctx, key); err != nil {
			return p, err
		} else if ok {
			*dst = v
		}
	}
	if v, ok, err := s.st.Setting(ctx, keyOnboarding); err != nil {
		return p, err
	} else if ok {
		p.OnboardingDone = v == "true"
	}
	return p, nil
}

// SetName sets the local user's first and last names.
func (s *Service) SetName(ctx context.Context, first, last string) error {
	if err := s.st.SetSetting(ctx, keyFirstName, first); err != nil {
		return err
	}
	return s.st.SetSetting(ctx, keyLastName, last)
}

// SetOnboarding records whether the local user finished meko_ui's onboarding.
func (s *Service) SetOnboarding(ctx context.Context, done bool) error {
	return s.st.SetSetting(ctx, keyOnboarding, strconv.FormatBool(done))
}

// ConversationSummaries is a page of a datapack's conversations, most recently active first,
// optionally only those whose title or turns contain query, and whether there are more.
func (s *Service) ConversationSummaries(ctx context.Context, datapack, query string, limit, offset int) (
	[]store.ConversationSummary, bool, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, false, err
	}
	return s.st.ConversationSummaries(ctx, datapack, query, limit, offset)
}

// CountConversations counts a datapack's conversations.
func (s *Service) CountConversations(ctx context.Context, datapack string) (int, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return 0, err
	}
	return s.st.CountConversations(ctx, datapack)
}

// Turn is one turn of a conversation's thread with the memories it produced.
type Turn struct {
	store.Message
	Memories []store.SavedMemory
	// Calls is the turn's decision trace: the conversation's Meko calls (what it read and what it
	// wrote) in the same window as Memories, apart from the calls that store the conversation itself.
	Calls []store.Call
}

// notInTrace are the calls that store a conversation rather than act within it.
var notInTrace = map[string]bool{"conversation_create": true, "conversation_add_message": true}

// ConversationThread is a conversation's summary and a page of its turns, oldest first, each with
// the memories it produced and the calls it made. An extracted memory names its turn; any other is
// the turn's when it was saved in its conversation after the turn before it and no later than it, as
// Meko Cloud infers them (#190). Its calls are matched by time the same way.
func (s *Service) ConversationThread(ctx context.Context, datapack, id string, limit, offset int) (
	store.ConversationSummary, []Turn, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return store.ConversationSummary{}, nil, err
	}
	id = normalizeID(id)
	if id == "" { // "-" or blank: the store reads an empty id as "the latest conversation"
		return store.ConversationSummary{}, nil, ErrNoConversation
	}
	c, err := s.st.ConversationSummaryOf(ctx, datapack, id)
	if errors.Is(err, store.ErrNotFound) {
		return c, nil, ErrNoConversation
	}
	if err != nil {
		return c, nil, err
	}
	msgs, err := s.st.Messages(ctx, datapack, c.ID, limit, offset)
	if err != nil {
		return c, nil, err
	}
	var after time.Time // the turn before the page's first
	if offset > 0 {
		prev, perr := s.st.Messages(ctx, datapack, c.ID, 1, offset-1)
		if perr != nil {
			return c, nil, perr
		}
		if len(prev) == 1 {
			after = prev[0].CreatedAt
		}
	}
	saved, err := s.st.ConversationMemories(ctx, datapack, c.ID)
	if err != nil {
		return c, nil, err
	}
	// From the conversation's own start: a session's conversation deleted and made again has the
	// same id, and the old one's calls stay in the call log.
	calls, err := s.st.ConversationCalls(ctx, datapack, c.ID, c.CreatedAt)
	if err != nil {
		return c, nil, err
	}
	turns := make([]Turn, len(msgs))
	next := 0 // calls and turns are both oldest first: each call is looked at once
	for i, m := range msgs {
		turns[i] = Turn{Message: m, Memories: []store.SavedMemory{}, Calls: []store.Call{}}
		for _, sm := range saved {
			if sm.MessageID == m.ID || sm.MessageID == "" && sm.CreatedAt.After(after) && !sm.CreatedAt.After(m.CreatedAt) {
				turns[i].Memories = append(turns[i].Memories, sm)
			}
		}
		for ; next < len(calls) && !calls[next].At.After(m.CreatedAt); next++ {
			if cl := calls[next]; cl.At.After(after) && !notInTrace[cl.Tool] {
				turns[i].Calls = append(turns[i].Calls, cl)
			}
		}
		after = m.CreatedAt
	}
	return c, turns, nil
}

// DatapackAgents is every agent seen in a datapack.
func (s *Service) DatapackAgents(ctx context.Context, datapack string) ([]store.AgentSeen, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, err
	}
	return s.st.DatapackAgents(ctx, datapack)
}

// SizeDatapack measures what a datapack holds.
func (s *Service) SizeDatapack(ctx context.Context, datapack string) (store.DatapackSizes, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return store.DatapackSizes{}, err
	}
	return s.st.SizeDatapack(ctx, datapack)
}

// Token weights, as Meko Cloud's estimate of what a context_search sent counts them: a knowledge-base
// chunk 1000, a memory 200, and past conversations 1000. Context size counts what a datapack holds
// with the same weights, so it reads against efficiency, but a conversation turn by its length, about
// 4 bytes a token: turns run from "yes" to a pasted log, so no one weight fits them.
const (
	TokensPerKBChunk      = 1000
	TokensPerMemory       = 200
	TokensPerConversation = 1000
	bytesPerToken         = 4
)

// SizePart is one source's share of a datapack's context size: Count is its knowledge-base chunks,
// memories or conversation turns.
type SizePart struct {
	Count  int `json:"count"`
	Tokens int `json:"tokens"`
}

// ContextSize is how much a datapack holds, in tokens: its knowledge base (promoted memories' chunks
// included), its memories and its conversation turns.
type ContextSize struct {
	Tokens        int      `json:"tokens"`
	Knowledge     SizePart `json:"knowledge"`
	Memories      SizePart `json:"memories"`
	Conversations SizePart `json:"conversations"`
}

// SizeOf is a datapack's context size from its knowledge base's stats and its sizes.
func SizeOf(kb store.KBStats, z store.DatapackSizes) ContextSize {
	out := ContextSize{
		Knowledge:     SizePart{Count: kb.Chunks, Tokens: kb.Chunks * TokensPerKBChunk},
		Memories:      SizePart{Count: z.Memories, Tokens: z.Memories * TokensPerMemory},
		Conversations: SizePart{Count: z.Turns, Tokens: z.TurnBytes / bytesPerToken},
	}
	out.Tokens = out.Knowledge.Tokens + out.Memories.Tokens + out.Conversations.Tokens
	return out
}

// Search tools, whose calls are a datapack's requests in its metrics.
var searchTools = map[string]string{"context_search": "hybrid", "memory_search": "memory", "knowledgebase_search": "kb"}

// DayActivity is one day's searches by kind, as meko_ui's activity chart shows them.
type DayActivity struct {
	Date               string // YYYY-MM-DD, UTC
	Hybrid, KB, Memory int
}

// CallStats is what the call log says about a datapack's searches.
type CallStats struct {
	Requests            int
	TokensSent          int
	TokensSaved         int
	SavedBySource       map[string]int // hybrid, memory, kb
	ScoreHigh, Mid, Low int
	Agents              int
	LastAccessed        *time.Time
	Daily               []DayActivity // the last 30 days with any, oldest first
	MemoryAdds          int
	KBSearches          int // knowledgebase_search calls
	InputTokens         int
	OutputTokens        int
	// Context efficiency and quality, as Meko Cloud counts them: only context_search calls that sent
	// something, each saving measured against its whole knowledge base.
	Context ContextStats
}

// ContextStats is Meko Cloud's context efficiency and quality over a datapack's context_search calls.
type ContextStats struct {
	Requests            int // context_search calls that sent something
	Sent, Saved, IfRaw  int // tokens sent and saved, and the baselines the saving was measured against
	ScoreHigh, Mid, Low int // their results by match quality
}

// defaultIfRaw is the baseline of a call recorded without one (before the call log kept it): Cloud's
// default of 10 knowledge-base chunks.
const defaultIfRaw = 10_000

// statsDays is how many days of activity CallStats reports by default.
const statsDays = 30

// window is the calls a days argument covers: 0 is the call log's whole retention, with the last
// statsDays days of activity (Cloud's dashboard); anything else is that many days for both, at
// most the retention.
func window(days int) (since time.Time, activityDays int) {
	if days <= 0 {
		return time.Now().Add(-CallRetention), statsDays
	}
	days = min(days, int(CallRetention.Hours()/24))
	return time.Now().AddDate(0, 0, -days), days
}

// CallStats sums a datapack's calls ("" for every datapack's) over the last lastDays (see window).
func (s *Service) CallStats(ctx context.Context, datapack string, lastDays int) (CallStats, error) {
	from, activityDays := window(lastDays)
	calls, err := s.st.Calls(ctx, datapack, from)
	if err != nil {
		return CallStats{}, err
	}
	out := CallStats{SavedBySource: map[string]int{"hybrid": 0, "memory": 0, "kb": 0}}
	agents := map[string]bool{}
	days := map[string]*DayActivity{}
	since := time.Now().UTC().AddDate(0, 0, -activityDays+1).Format(time.DateOnly)
	for _, c := range calls {
		if c.AgentID != "" && c.AgentID != WebUIAgent {
			agents[c.AgentID] = true
		}
		at := c.At
		out.LastAccessed = &at
		out.InputTokens += c.InputTokens
		out.OutputTokens += c.OutputTokens
		if c.Tool == "memory_add" && c.OK {
			out.MemoryAdds++
		}
		kind, isSearch := searchTools[c.Tool]
		if !isSearch || !c.OK {
			continue
		}
		out.Requests++
		if kind == "kb" {
			out.KBSearches++
		}
		out.TokensSent += c.TokensSent
		out.TokensSaved += c.TokensSaved
		out.SavedBySource[kind] += c.TokensSaved
		out.ScoreHigh += c.ScoreHigh
		out.Mid += c.ScoreMid
		out.Low += c.ScoreLow
		if kind == "hybrid" && c.TokensSent > 0 {
			// A call recorded without a baseline measured its saving against another one; both are
			// counted against the default instead, so saved never exceeds the baseline.
			ifRaw, saved := c.TokensIfRaw, c.TokensSaved
			if ifRaw == 0 {
				ifRaw, saved = defaultIfRaw, max(0, defaultIfRaw-c.TokensSent)
			}
			out.Context.Requests++
			out.Context.Sent += c.TokensSent
			out.Context.Saved += saved
			out.Context.IfRaw += ifRaw
			out.Context.ScoreHigh += c.ScoreHigh
			out.Context.Mid += c.ScoreMid
			out.Context.Low += c.ScoreLow
		}
		day := c.At.UTC().Format(time.DateOnly)
		if day < since {
			continue
		}
		d := days[day]
		if d == nil {
			d = &DayActivity{Date: day}
			days[day] = d
		}
		switch kind {
		case "hybrid":
			d.Hybrid++
		case "kb":
			d.KB++
		default:
			d.Memory++
		}
	}
	out.Agents = len(agents)
	out.Daily = make([]DayActivity, 0, len(days))
	for _, d := range days {
		out.Daily = append(out.Daily, *d)
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Date < out.Daily[j].Date })
	return out, nil
}

// WebUIAgent is the agent the web UI's own calls are recorded as. It's the user, not an agent, so
// the agent counts leave it out; its searches still count as searches.
const WebUIAgent = "meko_web_ui"

// recentCalls is how many of the latest calls CallUsage lists.
const recentCalls = 25

// ToolUsage is one tool's calls in CallUsage: how many, how many failed, and how long they took.
type ToolUsage struct {
	Tool         string
	Calls        int
	Errors       int
	P50Ms, P95Ms int64
}

// StepUsage is one timed part of the searches, such as searching one source.
type StepUsage struct {
	Step         string
	Calls        int
	P50Ms, P95Ms int64
}

// AgentUsage is one agent's calls in CallUsage.
type AgentUsage struct {
	AgentID  string
	Calls    int
	Searches int
	LastAt   time.Time
}

// CallUsage is how a datapack's calls went over the call log's retention: per tool, per search
// step and per agent, and the latest calls. Meko's own: Cloud's dashboard has no such view.
type CallUsage struct {
	Calls        int
	Errors       int
	Tools        []ToolUsage  // most called first
	Steps        []StepUsage  // by name
	Agents       []AgentUsage // most recently seen first
	Recent       []store.Call // newest first
	InputTokens  int          // what track_token_usage reported, before it was removed
	OutputTokens int
}

// CallUsage summarizes a datapack's calls ("" for every datapack's) over the last days (see window).
func (s *Service) CallUsage(ctx context.Context, datapack string, days int) (CallUsage, error) {
	from, _ := window(days)
	calls, err := s.st.Calls(ctx, datapack, from)
	if err != nil {
		return CallUsage{}, err
	}
	out := CallUsage{Calls: len(calls), Tools: []ToolUsage{}, Steps: []StepUsage{}, Agents: []AgentUsage{},
		Recent: []store.Call{}}
	toolMs, stepMs := map[string][]int64{}, map[string][]int64{}
	tools, agents := map[string]*ToolUsage{}, map[string]*AgentUsage{}
	for _, c := range calls {
		t := tools[c.Tool]
		if t == nil {
			t = &ToolUsage{Tool: c.Tool}
			tools[c.Tool] = t
		}
		t.Calls++
		if !c.OK {
			t.Errors++
			out.Errors++
		}
		toolMs[c.Tool] = append(toolMs[c.Tool], c.Ms)
		for _, st := range c.Steps {
			stepMs[st.Step] = append(stepMs[st.Step], st.Ms)
		}
		out.InputTokens += c.InputTokens
		out.OutputTokens += c.OutputTokens
		if c.AgentID != "" {
			a := agents[c.AgentID]
			if a == nil {
				a = &AgentUsage{AgentID: c.AgentID}
				agents[c.AgentID] = a
			}
			a.Calls++
			if _, ok := searchTools[c.Tool]; ok {
				a.Searches++
			}
			if c.At.After(a.LastAt) {
				a.LastAt = c.At
			}
		}
	}
	for name, t := range tools {
		t.P50Ms, t.P95Ms = percentile(toolMs[name], 50), percentile(toolMs[name], 95)
		out.Tools = append(out.Tools, *t)
	}
	sort.Slice(out.Tools, func(i, j int) bool {
		if out.Tools[i].Calls != out.Tools[j].Calls {
			return out.Tools[i].Calls > out.Tools[j].Calls
		}
		return out.Tools[i].Tool < out.Tools[j].Tool
	})
	for name, ms := range stepMs {
		out.Steps = append(out.Steps, StepUsage{Step: name, Calls: len(ms), P50Ms: percentile(ms, 50),
			P95Ms: percentile(ms, 95)})
	}
	sort.Slice(out.Steps, func(i, j int) bool { return out.Steps[i].Step < out.Steps[j].Step })
	for _, a := range agents {
		out.Agents = append(out.Agents, *a)
	}
	sort.Slice(out.Agents, func(i, j int) bool { return out.Agents[i].LastAt.After(out.Agents[j].LastAt) })
	for i := len(calls) - 1; i >= 0 && len(out.Recent) < recentCalls; i-- {
		out.Recent = append(out.Recent, calls[i])
	}
	return out, nil
}

// percentile is the nearest-rank p-th percentile of ms, 0 for none. It sorts ms.
func percentile(ms []int64, p int) int64 {
	if len(ms) == 0 {
		return 0
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	i := (p*len(ms)+99)/100 - 1
	return ms[max(0, min(i, len(ms)-1))]
}

// CountAll counts the install's datapacks and conversations.
func (s *Service) CountAll(ctx context.Context) (datapacks, conversations int, err error) {
	return s.st.CountAll(ctx)
}

// KnowledgeBytes is the size of every datapack's knowledge files.
func (s *Service) KnowledgeBytes(ctx context.Context) (int64, error) { return s.st.KnowledgeBytes(ctx) }
