package wire

import (
	"testing"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/memoryengine"
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
