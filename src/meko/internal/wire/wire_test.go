package wire

import (
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
)

// A memory is banded by its raw cosine, as Cloud bands it, and by its score only when it has no
// raw one; knowledge-base and conversation hits by 1 - distance.
func TestBands(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	r := memory.ContextResults{
		Memory: []memoryengine.Memory{
			{Score: f(0.3), RawSimilarityScore: f(0.8)}, // fused low, cosine high: high
			{Score: f(0.6)}, // no cosine: by score, mid
			{Score: f(0.9), RawSimilarityScore: f(0.2)}, // low
			{}, // no score at all: not counted
		},
		KnowledgeBase: []memory.KBResult{{Distance: 0.1}, {Distance: 0.45}},
		Conversation:  &memory.ConversationResults{Hits: []memory.ConversationHit{{Distance: 0.7}}},
	}
	if h, m, l := Bands(r); h != 2 || m != 2 || l != 2 {
		t.Errorf("Bands = %d, %d, %d; want 2, 2, 2", h, m, l)
	}
}

// Context efficiency, as Meko Cloud measures it: a search sends 1000 tokens a knowledge-base result,
// 200 a memory and 1000 for matched conversations, and saves against the whole knowledge base. The
// worked example in the docs: 14 chunks are a 14,000-token baseline; 1 chunk and 3 memories send
// 1,600 and save 12,400.
func TestSavings(t *testing.T) {
	kb := []memory.KBResult{{Distance: 0.2}}
	mems := make([]memoryengine.Memory, 3)
	s := Savings(memory.ContextResults{KnowledgeBase: kb, Memory: mems, KnowledgeChunks: 14})
	if s.Sent() != 1600 || s.Baseline != 14000 || s.Saved() != 12400 {
		t.Errorf("sent %d, baseline %d, saved %d; want 1600, 14000, 12400", s.Sent(), s.Baseline, s.Saved())
	}
	// Each source's own share stays against the knowledge results returned, as Cloud's reply has it.
	if s.TotalTokens != 1000 || s.Memory.TokensSaved != 400 {
		t.Errorf("per source: total %d, memory saved %d", s.TotalTokens, s.Memory.TokensSaved)
	}
	s = Savings(memory.ContextResults{Memory: make([]memoryengine.Memory, 2),
		Conversation: &memory.ConversationResults{Hits: []memory.ConversationHit{{}}}})
	if s.Sent() != 1400 || s.Baseline != 10000 || s.Saved() != 8600 {
		t.Errorf("no files: sent %d, baseline %d, saved %d", s.Sent(), s.Baseline, s.Saved())
	}
	big := Savings(memory.ContextResults{KnowledgeBase: make([]memory.KBResult, 3), KnowledgeChunks: 2})
	if big.Saved() != 0 || big.Baseline != 2000 {
		t.Errorf("sending more than the baseline saves %d (baseline %d), want 0", big.Saved(), big.Baseline)
	}
	if none := Savings(memory.ContextResults{KnowledgeChunks: 5}); none.TokensSaved != nil || none.Sent() != 0 {
		t.Errorf("nothing sent: %+v", none)
	}
}
