package memory

import (
	"strings"
	"testing"
)

// A word match past the distance cutoff is kept only once the reranker has scored it.
func TestKeptKB(t *testing.T) {
	score := 0.8
	res := []KBResult{
		{ID: "near", Distance: 0.3, MatchType: "semantic"},
		{ID: "far-semantic", Distance: 0.9, MatchType: "semantic", RerankScore: &score},
		{ID: "far-keyword-reranked", Distance: 0.9, MatchType: "keyword", RerankScore: &score},
		{ID: "far-hybrid-reranked", Distance: 0.65, MatchType: "hybrid", RerankScore: &score},
		{ID: "far-keyword-unranked", Distance: 0.9, MatchType: "keyword"},
	}
	kept := keptKB(res, 0.6)
	got := make([]string, 0, len(kept))
	for _, k := range kept {
		got = append(got, k.ID)
	}
	if want := "near far-keyword-reranked far-hybrid-reranked"; strings.Join(got, " ") != want {
		t.Errorf("kept %v, want %s", got, want)
	}
}
