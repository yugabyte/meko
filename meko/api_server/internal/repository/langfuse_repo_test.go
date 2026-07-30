package repository

import (
	"reflect"
	"testing"
	"time"
)

func TestChildSubTimingClass(t *testing.T) {
	cases := []struct {
		name       string
		obs        string
		parentWant string
		keyWant    string
	}{
		{"mem0 vector search", "Searching (yugabytedb / vector)", "memory_search", "vector_ms"},
		{"mem0 mage search", "Searching (Mage)", "", ""}, // intentionally NOT mapped — too generic
		{"mem0 graph db", "Search Graph DB (mage / graph)", "memory_search", "graph_ms"},
		{"mem0 graph db apache", "Search Graph DB (yugabytedb / graph)", "memory_search", "graph_ms"},
		{"mcp execute query exact", "Execute Query (yugabytedb)", "knowledgebase_search", "vector_ms"},
		{"mem0 execute query nested (NOT matched)", "Execute Query (yugabytedb / vector)", "", ""},
		{"empty", "", "", ""},
		{"unrelated", "Reasoning", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent, key := childSubTimingClass(c.obs)
			if parent != c.parentWant || key != c.keyWant {
				t.Fatalf("childSubTimingClass(%q) = (%q,%q), want (%q,%q)",
					c.obs, parent, key, c.parentWant, c.keyWant)
			}
		})
	}
}

func TestProcessingStepFromObservation_SetsSpanID(t *testing.T) {
	obs := agentReasoningObservation{
		ID:         "obs_abc123",
		Name:       "Memory Search (Meko MCP)",
		Type:       "TOOL",
		StartTime:  time.Unix(1700000000, 0),
		DurationMs: 84,
		Input:      `{"query":"audit log compliance","agent_id":"claude","limit":10}`,
	}
	step := processingStepFromObservation(obs)
	if step == nil {
		t.Fatal("expected non-nil step")
	}
	if step.Name != "memory_search" {
		t.Fatalf("step.Name = %q, want memory_search", step.Name)
	}
	if step.SpanID != "obs_abc123" {
		t.Fatalf("step.SpanID = %q, want obs_abc123", step.SpanID)
	}
	if step.DurationMs != 84 {
		t.Fatalf("step.DurationMs = %d, want 84", step.DurationMs)
	}
	if step.Query != "audit log compliance" {
		t.Fatalf("step.Query = %q, want %q", step.Query, "audit log compliance")
	}
	if got := step.ToolArgs["agent_id"]; got != "claude" {
		t.Fatalf("step.ToolArgs[agent_id] = %v, want claude", got)
	}
	if got := step.ToolArgs["limit"]; got != float64(10) {
		t.Fatalf("step.ToolArgs[limit] = %v, want 10", got)
	}
	// SubTimings is populated by the parent loop, not by this helper.
	if step.SubTimings != nil {
		t.Fatalf("step.SubTimings = %v, want nil at construction", step.SubTimings)
	}
}

func TestProcessingStepFromObservation_Generation(t *testing.T) {
	obs := agentReasoningObservation{
		ID:         "obs_gen_1",
		Name:       "OpenAI Generation",
		Type:       "GENERATION",
		DurationMs: 240,
	}
	step := processingStepFromObservation(obs)
	if step == nil || step.Name != "llm_invoke" {
		t.Fatalf("expected llm_invoke step, got %+v", step)
	}
	if step.SpanID != "obs_gen_1" {
		t.Fatalf("SpanID = %q, want obs_gen_1", step.SpanID)
	}
}

func TestProcessingStepFromObservation_UnknownReturnsNil(t *testing.T) {
	obs := agentReasoningObservation{Name: "Random Span", Type: "SPAN"}
	if step := processingStepFromObservation(obs); step != nil {
		t.Fatalf("expected nil for unknown span, got %+v", step)
	}
}

func TestParsePlanSteps(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"null", "null", nil},
		{"empty array", "[]", nil},
		{"single", `["recommend monthly partitioning"]`, []string{"recommend monthly partitioning"}},
		{
			"multi step",
			`["call memory_add", "attach metadata", "confirm to user"]`,
			[]string{"call memory_add", "attach metadata", "confirm to user"},
		},
		{
			"trims whitespace and drops blanks",
			`["  step one ", "", "   ", "step two"]`,
			[]string{"step one", "step two"},
		},
		{"malformed json swallowed", `[not json`, nil},
		{"wrong type swallowed", `{"steps":["x"]}`, nil},
		{"non-string array swallowed", `[1, 2, 3]`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePlanSteps(c.raw)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parsePlanSteps(%q) = %#v, want %#v", c.raw, got, c.want)
			}
		})
	}
}
