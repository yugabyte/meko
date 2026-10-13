package repository

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetDatapackProvisioningInfo(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT datapack_name, user_id
		 FROM meko_system.datapacks
		 WHERE datapack_id = $1`,
	)).
		WithArgs("datapack-1").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_name", "user_id"}).
			AddRow("  my-pack  ", "  owner-1  "))

	repo := NewLangfuseRepo(db, nil, nil)
	got, err := repo.GetDatapackProvisioningInfo("datapack-1")
	if err != nil {
		t.Fatalf("GetDatapackProvisioningInfo: %v", err)
	}
	if got == nil {
		t.Fatal("GetDatapackProvisioningInfo returned nil")
	}
	if got.Name != "my-pack" || got.OwnerUserID != "owner-1" {
		t.Fatalf("GetDatapackProvisioningInfo = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

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

// MEKO-663: context_search emits one span per leg, and the conversation leg's
// span has always been dropped by the observation-name filter. These pin the
// three names the MCP server writes and the payloads they carry, because that
// pairing is the whole contract behind the decision trace's Preprocess phase.
func TestProcessingStepFromObservation_ContextSearchLegs(t *testing.T) {
	cases := []struct {
		obs  string
		want string
	}{
		{"Knowledge Base Search (Meko MCP)", "knowledgebase_search"},
		{"Memory Search (Meko MCP)", "memory_search"},
		{"Conversation Search (Meko MCP)", "conversation_search"},
	}
	for _, c := range cases {
		t.Run(c.obs, func(t *testing.T) {
			step := processingStepFromObservation(agentReasoningObservation{
				ID: "obs_1", Name: c.obs, Type: "TOOL",
			})
			if step == nil {
				t.Fatalf("processingStepFromObservation(%q) = nil", c.obs)
			}
			if step.Name != c.want {
				t.Fatalf("step.Name = %q, want %q", step.Name, c.want)
			}
		})
	}
}

func TestExtractConversationChunks(t *testing.T) {
	// The shape _conversation_search_core produces, serialized onto the span.
	output := `{"conversation_hits":[
		{"content":"further hit","distance":0.30,"conversation_id":"c2","message_id":"m2","user_id":"u2"},
		{"content":"nearest hit","distance":0.10,"conversation_id":"c1","message_id":"m1","user_id":"u1"}
	],"thread":[],"count":2}`

	chunks := extractConversationChunks(parseJSONValue(output))
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2", len(chunks))
	}
	// Cosine distance, so nearest first regardless of the order it arrived in.
	if chunks[0].ConversationID != "c1" {
		t.Fatalf("chunks[0].ConversationID = %q, want c1", chunks[0].ConversationID)
	}
	if chunks[0].Content != "nearest hit" || chunks[0].MessageID != "m1" || chunks[0].UserID != "u1" {
		t.Fatalf("chunks[0] = %+v", chunks[0])
	}
	if chunks[0].Score == nil || *chunks[0].Score != 0.9 {
		t.Fatalf("chunks[0].Score = %v, want 0.9 (1 - distance)", chunks[0].Score)
	}
}

// A hit with no distance is a shape surprise, not evidence of irrelevance —
// keep it, and leave the score unset rather than inventing one.
func TestExtractConversationChunks_MissingDistanceIsKeptUnscored(t *testing.T) {
	chunks := extractConversationChunks(parseJSONValue(`{"conversation_hits":[{"content":"no distance"}]}`))
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	if chunks[0].Score != nil || chunks[0].Distance != nil {
		t.Fatalf("expected no score/distance, got %+v", chunks[0])
	}
}

func TestExtractConversationChunks_IgnoresEmptyAndMalformed(t *testing.T) {
	chunks := extractConversationChunks(parseJSONValue(
		`{"conversation_hits":[{"content":""},"not an object",{"content":"kept"}]}`))
	if len(chunks) != 1 || chunks[0].Content != "kept" {
		t.Fatalf("chunks = %+v, want just the one with content", chunks)
	}
	// The counts-only output the parent context_search span carries must
	// produce nothing rather than a phantom chunk.
	if got := extractConversationChunks(parseJSONValue(
		`{"conversation_hits":3,"knowledge_base_count":2}`)); len(got) != 0 {
		t.Fatalf("counts-only output produced %d chunks", len(got))
	}
}

// Which chunk list the conversation_search step ends up with. The span now
// carries the ranked hits on every turn, while user_metadata carries a list
// only on some — and on the BLENDED path that list is unranked thread turns,
// so "metadata always wins" would throw away the better data.
func TestAttachConversationChunks(t *testing.T) {
	spanChunk := func() []ConversationChunk {
		d := 0.1
		s := 0.9
		return []ConversationChunk{{Content: "ranked hit", Distance: &d, Score: &s}}
	}
	metaChunks := []ConversationChunk{
		{Content: "thread turn one"},
		{Content: "thread turn two"},
	}
	withSpanChunks := func() []ProcessingStep {
		return []ProcessingStep{{Name: "conversation_search", ConversationChunks: spanChunk()}}
	}

	t.Run("a real cache hit wins — those chunks are the answer", func(t *testing.T) {
		steps := attachConversationChunks(withSpanChunks(), metaChunks, "conversation_cache")
		if len(steps[0].ConversationChunks) != 2 {
			t.Fatalf("got %d chunks, want the 2 from metadata", len(steps[0].ConversationChunks))
		}
		// Regression: Detail kept the span's count beside a different number
		// of chunks, so the card read "results: 1 hit" over two rows.
		if steps[0].Detail != "2 hits" {
			t.Fatalf("Detail = %q, want 2 hits", steps[0].Detail)
		}
	})

	// Not a quality judgement: cache-hit metadata is built the same way as
	// blended (`thread or hits`), so this usually trades scored hits for
	// unscored turns. It wins because those turns ARE what was summarized.
	t.Run("a blended turn keeps the span's ranked hits", func(t *testing.T) {
		steps := attachConversationChunks(withSpanChunks(), metaChunks, "conversation_context")
		if len(steps[0].ConversationChunks) != 1 || steps[0].ConversationChunks[0].Content != "ranked hit" {
			t.Fatalf("blended turn took the unranked metadata: %+v", steps[0].ConversationChunks)
		}
		if steps[0].ConversationChunks[0].Score == nil {
			t.Fatal("kept a chunk but lost its score")
		}
	})

	t.Run("metadata fills a step the span left empty", func(t *testing.T) {
		steps := attachConversationChunks(
			[]ProcessingStep{{Name: "conversation_search"}}, metaChunks, "conversation_context")
		if len(steps[0].ConversationChunks) != 2 {
			t.Fatalf("got %d chunks, want 2", len(steps[0].ConversationChunks))
		}
	})

	// Traces recorded before the MCP server emitted the span have no step to
	// attach to; their chunks still have to surface.
	t.Run("synthesizes the step when the span never landed", func(t *testing.T) {
		steps := attachConversationChunks(
			[]ProcessingStep{{Name: "knowledgebase_search", SpanID: "kb_span"}},
			metaChunks, "conversation_cache")
		if len(steps) != 2 || steps[1].Name != "conversation_search" {
			t.Fatalf("steps = %+v", steps)
		}
		if steps[1].ParentSpanID != "kb_span" {
			t.Fatalf("ParentSpanID = %q, want it nested under the KB step", steps[1].ParentSpanID)
		}
		if steps[1].Detail != "2 hits" {
			t.Fatalf("Detail = %q, want 2 hits", steps[1].Detail)
		}
	})

	t.Run("no metadata leaves the span's chunks alone", func(t *testing.T) {
		steps := attachConversationChunks(withSpanChunks(), nil, "")
		if len(steps) != 1 || len(steps[0].ConversationChunks) != 1 {
			t.Fatalf("steps = %+v", steps)
		}
	})
}

// A new install has no meko_multi_tenant_graph, so the shared tail must still define
// every CTE the SELECTs read without touching a table that is not there.
func TestTraceMemoryAggregationCTEsWithoutTheGraph(t *testing.T) {
	q := traceMemoryAggregationCTEs(4, 5, false)
	if strings.Contains(q, "meko_multi_tenant_graph") {
		t.Error("the graph-less query still reads meko_multi_tenant_graph")
	}
	if !strings.Contains(q, "edges AS (") || !strings.Contains(q, "nodes AS (") {
		t.Errorf("the SELECTs read both nodes and edges, but the tail lacks one:\n%s", q)
	}
	if strings.Contains(q, "$4") || strings.Contains(q, "$5") {
		t.Error("the graph-less query references the tenant patterns, which are not passed in that case")
	}
}

func TestTraceMemoryAggregationCTEsWithTheGraph(t *testing.T) {
	q := traceMemoryAggregationCTEs(4, 5, true)
	if !strings.Contains(q, "meko_multi_tenant_graph") {
		t.Error("the graph query does not read meko_multi_tenant_graph")
	}
	if !strings.Contains(q, "$4") || !strings.Contains(q, "$5") {
		t.Errorf("the tenant pattern positions were not substituted:\n%s", q)
	}
}

func TestLegacyGraphPresent(t *testing.T) {
	for _, want := range []bool{true, false} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		mock.ExpectQuery("to_regclass").WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(want))

		got, err := NewLangfuseRepo(nil, db, nil).legacyGraphPresent()
		if err != nil || got != want {
			t.Errorf("legacyGraphPresent = %v, %v, want %v", got, err, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	}
}

// With no graph the lookup must still answer, and must pass exactly the parameters the
// graph-less statement references: the driver rejects an extra one.
func TestBatchLookupTraceMemoryMetricsByIDsWithoutTheGraph(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	const conv = "11111111-1111-4111-8111-111111111111"
	mock.ExpectQuery(regexp.QuoteMeta("NULL::text AS relation_name")).
		WithArgs("dp-1", conv, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "vector", "entities", "relations", "entity_names", "relation_names"}).
			AddRow("mem-1", 1, 0, 0, "{}", "{}"))

	got, err := NewLangfuseRepo(nil, db, nil).batchLookupTraceMemoryMetricsByIDs("dp-1", conv, []string{"mem-1"}, false)
	if err != nil {
		t.Fatalf("batchLookupTraceMemoryMetricsByIDs: %v", err)
	}
	if m := got["mem-1"]; m == nil || m.Vector != 1 || m.Relations != 0 {
		t.Errorf("metrics = %+v", got["mem-1"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
