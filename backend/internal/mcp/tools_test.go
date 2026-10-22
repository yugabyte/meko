package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

func TestMemoryTools(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	add := func(text, agent, conv string) string {
		r := ok(t, e, "memory_add", map[string]any{"text": text, "agent_id": agent, "conversation_id": conv})
		return str(obj(arr(r.JSON["results"])[0])["id"])
	}
	mine := add("the deploy key rotates every friday", "claude_code:a", "aaaa0000aaaa0000aaaa0000aaaa0000")
	theirs := add("the staging database is called orchid", "claude_code:b", "bbbb0000bbbb0000bbbb0000bbbb0000")

	// Search is across agents, with prod's item shape.
	r := ok(t, e, "memory_search", map[string]any{"query": "staging database orchid", "conversation_id": "c",
		"agent_id": "claude_code:a"})
	items := arr(r.JSON["results"])
	if len(items) == 0 || obj(items[0])["id"] != theirs {
		t.Fatalf("search: %s", r.Text)
	}
	first := obj(items[0])
	if first["agent_id"] != "claude_code:b" || first["score"] == nil || first["raw_similarity_score"] == nil ||
		obj(first["metadata"])["meko_datapack_id"] != e.DatapackID {
		t.Errorf("search item: %v", first)
	}
	// run_id narrows it to one conversation.
	r = ok(t, e, "memory_search", map[string]any{"query": "staging database orchid", "conversation_id": "c",
		"run_id": "aaaa0000-aaaa-0000-aaaa-0000aaaa0000"})
	for _, it := range arr(r.JSON["results"]) {
		if obj(it)["id"] == theirs {
			t.Errorf("run_id filter: %s", r.Text)
		}
	}

	r = ok(t, e, "memory_get_by_id", map[string]any{"memory_id": mine, "conversation_id": "c"})
	if r.JSON["memory"] != "the deploy key rotates every friday" || r.JSON["score"] != nil {
		t.Errorf("get: %s", r.Text)
	}
	wantError(t, e, "memory_get_by_id", map[string]any{"memory_id": "nope", "conversation_id": "c"}, "invalid_memory_id")
	wantError(t, e, "memory_get_by_id", map[string]any{"memory_id": "00000000-0000-0000-0000-000000000000",
		"conversation_id": "c"}, "not_found")

	// Only the writer updates or deletes a memory; to anyone else it isn't there.
	wantError(t, e, "memory_update", map[string]any{"memory_id": mine, "text": "x", "conversation_id": "c",
		"agent_id": "claude_code:b"}, "not_found")
	r = ok(t, e, "memory_update", map[string]any{"memory_id": mine, "text": "the deploy key rotates monthly",
		"conversation_id": "c", "agent_id": "claude_code:a"})
	if r.JSON["message"] != "Memory updated successfully!" {
		t.Errorf("update: %s", r.Text)
	}
	wantError(t, e, "memory_delete_by_id", map[string]any{"memory_id": mine, "conversation_id": "c"}, "not_found")

	// The list is newest first, with the text in data.
	r = ok(t, e, "memory_get_all", map[string]any{"conversation_id": "c"})
	list := arr(r.JSON["memories"])
	if num(r.JSON["total"]) != 2 || len(list) != 2 || obj(list[0])["id"] != theirs ||
		obj(list[1])["data"] != "the deploy key rotates monthly" || obj(list[0])["is_mine"] != true {
		t.Errorf("get_all: %s", r.Text)
	}
	r = ok(t, e, "memory_get_all", map[string]any{"conversation_id": "c", "promoted": true})
	if r.JSON["promoted_memories"] == nil {
		t.Errorf("get_all promoted: %s", r.Text)
	}

	wantError(t, e, "memory_delete_all", map[string]any{"conversation_id": "c", "agent_id": "claude_code:a",
		"run_id": "not-a-conversation"}, "invalid_run_id")
	// A run_id that's blank only after normalizing doesn't widen the delete to every conversation.
	wantError(t, e, "memory_delete_all", map[string]any{"conversation_id": "c", "agent_id": "claude_code:a",
		"run_id": " - "}, "invalid_run_id")
	ok(t, e, "memory_delete_all", map[string]any{"conversation_id": "c", "agent_id": "claude_code:a"})
	r = ok(t, e, "memory_get_all", map[string]any{"conversation_id": "c"})
	if num(r.JSON["total"]) != 1 {
		t.Errorf("delete_all deletes only the agent's: %s", r.Text)
	}
	r = ok(t, e, "memory_delete_by_id", map[string]any{"memory_id": theirs, "conversation_id": "c",
		"agent_id": "claude_code:b"})
	if r.JSON["message"] != "Memory deleted successfully!" {
		t.Errorf("delete: %s", r.Text)
	}
}

func TestConversationTools(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	// No datapack: the default one. The same session id is the same conversation.
	r := ok(t, e, "conversation_create", map[string]any{"agent_id": "agent-k-local", "datapack_id": nil,
		"session_id": "agent-k-local-service", "title": "AgentK service"})
	id := str(r.JSON["id"])
	if len(id) != 32 || r.JSON["title"] != "AgentK service" || r.JSON["app_id"] != nil {
		t.Fatalf("create: %s", r.Text)
	}
	if again := ok(t, e, "conversation_create", map[string]any{"agent_id": "agent-k-local",
		"session_id": "agent-k-local-service", "title": "x"}); again.JSON["id"] != id {
		t.Errorf("same session, new conversation: %s", again.Text)
	}
	wantError(t, e, "conversation_create", map[string]any{"title": "  "}, "invalid_title")
	wantError(t, e, "conversation_create", map[string]any{"title": "t", "metadata": "{"}, "invalid_metadata")

	long := strings.Repeat("the build cache lives in backend/memory_engine/.models ", 2)
	r = ok(t, e, "conversation_add_message", map[string]any{"conversation_id": id, "agent_id": "agent-k-local",
		"input": long, "output": "noted", "plan": []string{"a", "b"}, "seed": "turn-1"})
	if r.JSON["status"] != "accepted" || len(str(r.JSON["message_id"])) != 32 {
		t.Errorf("add: %s", r.Text)
	}
	wantError(t, e, "conversation_add_message", map[string]any{"conversation_id": id, "agent_id": "someone-else",
		"input": "i", "output": "o"}, "agent_id_mismatch")
	wantError(t, e, "conversation_add_message", map[string]any{"conversation_id": strings.Repeat("0", 32),
		"input": "i", "output": "o"}, "not_found")

	r = ok(t, e, "conversation_get", map[string]any{"conversation_id": id, "agent_id": "agent-k-local",
		"include_messages": true})
	msgs := arr(r.JSON["messages"])
	if num(r.JSON["message_count"]) != 1 || len(msgs) != 1 || obj(msgs[0])["input"] != long {
		t.Errorf("get: %s", r.Text)
	}

	r = ok(t, e, "conversation_update", map[string]any{"conversation_id": id, "agent_id": "agent-k-local",
		"datapack_id": e.DatapackID, "title": "New (Demo)"})
	if r.JSON["title"] != "New (Demo)" {
		t.Errorf("update: %s", r.Text)
	}
	wantError(t, e, "conversation_update", map[string]any{"conversation_id": id, "agent_id": "agent-k-local"}, "no_fields")

	r = ok(t, e, "conversation_list", map[string]any{"limit": 500})
	convs := arr(r.JSON["conversations"])
	if r.JSON["datapack_id"] != e.DatapackID || r.JSON["datapack_name"] != "default" || num(r.JSON["limit"]) != 200 ||
		len(convs) != 1 || num(obj(convs[0])["message_count"]) != 1 {
		t.Errorf("list: %s", r.Text)
	}

	r = ok(t, e, "conversation_delete", map[string]any{"conversation_id": id, "agent_id": "agent-k-local"})
	if r.JSON["status"] != "deleted" || r.JSON["conversation_id"] != id {
		t.Errorf("delete: %s", r.Text)
	}
}

func TestContextSearch(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ok(t, e, "memory_add", map[string]any{"text": "the release train leaves on tuesdays", "conversation_id": "c"})
	conv := str(ok(t, e, "conversation_create", map[string]any{"session_id": "s1", "title": "release chat"}).JSON["id"])
	ok(t, e, "conversation_add_message", map[string]any{"conversation_id": conv,
		"input": "when does the release train leave each week?", "output": "the release train leaves on tuesdays"})
	// The turn is searchable once the sweep embeds it.
	var r callResult
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r = ok(t, e, "context_search", map[string]any{"query": "when does the release train leave each week?",
			"conversation_id": "other-conversation", "limit": 5})
		conv := obj(r.JSON["results"])["conversation"]
		if conv != nil && len(arr(obj(conv)["hits"])) > 0 || time.Now().After(deadline) {
			break
		}
	}
	res := obj(r.JSON["results"])
	if kb, isList := res["knowledge_base"].([]any); !isList || len(kb) != 0 || len(arr(res["memory"])) == 0 ||
		len(arr(obj(res["conversation"])["hits"])) == 0 {
		t.Fatalf("context_search: %s", r.Text)
	}
	savings := obj(r.JSON["token_savings"])
	if num(savings["total_tokens"]) != 10000 || savings["memory"] == nil || savings["conversation"] == nil ||
		savings["tokens_saved"] == nil {
		t.Errorf("token_savings: %v", savings)
	}
	// Searching from inside that conversation leaves it out.
	r = ok(t, e, "context_search", map[string]any{"query": "when does the release train leave each week?",
		"conversation_id": conv, "force_source": "conversation"})
	res = obj(r.JSON["results"])
	if res["memory"] != nil || len(arr(obj(res["conversation"])["hits"])) != 0 {
		t.Errorf("own conversation: %s", r.Text)
	}
	r = ok(t, e, "context_search", map[string]any{"query": "release train", "conversation_id": "c",
		"force_source": "memory"})
	if res = obj(r.JSON["results"]); res["conversation"] != nil || res["memory"] == nil {
		t.Errorf("force_source memory: %s", r.Text)
	}
	// The call log has the search's counts and steps, never its query.
	calls, _ := e.Store.Calls(context.Background(), e.DatapackID, time.Time{})
	var found bool
	for _, c := range calls {
		if c.Tool == "context_search" && c.MemoryN > 0 && c.ConversationN > 0 && len(c.Steps) == 3 && c.TokensSent > 0 {
			found = true
		}
		if strings.Contains(fmt.Sprintf("%+v", c), "release") {
			t.Errorf("the call log holds text: %+v", c)
		}
	}
	if !found {
		t.Errorf("no context_search row with counts: %+v", calls)
	}
}

func TestDatapackTools(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	c := map[string]any{"conversation_id": "c"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{"conversation_id": "c"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[str(kv[i])] = kv[i+1]
		}
		return m
	}
	r := ok(t, e, "datapack_create", with("name", "adlc/proj/a/b"))
	id := str(r.JSON["datapack_id"])
	if r.JSON["name"] != "adlc/proj/a/b" || r.JSON["agent_id"] == nil {
		t.Fatalf("create: %s", r.Text)
	}
	wantError(t, e, "datapack_create", with("name", "adlc/proj/a/b"), "CONFLICT")
	wantError(t, e, "datapack_create", with("name", "ab"), "VALIDATION_ERROR")
	wantError(t, e, "datapack_create", map[string]any{"name": "nope"}, "conversation_id_required")

	r = ok(t, e, "datapack_list", c)
	if len(r.List) != 2 || obj(r.List[0])["datapack_id"] != id || obj(r.List[0])["grant"] != "owner" {
		t.Errorf("list (newest first): %s", r.Text)
	}
	r = ok(t, e, "datapack_search", with("query", "PROJ", "include_total", true))
	if len(arr(r.JSON["datapacks"])) != 1 || num(r.JSON["total"]) != 1 || r.JSON["has_more"] != false {
		t.Errorf("search: %s", r.Text)
	}
	r = ok(t, e, "datapack_search", with("limit", 1, "sort", "name"))
	if r.JSON["next_cursor"] != "1" || r.JSON["has_more"] != true {
		t.Errorf("search page: %s", r.Text)
	}
	wantError(t, e, "datapack_search", with("grant", "viewer"), "invalid_grant")
	if r = ok(t, e, "datapack_search", with("cursor", "9223372036854775807")); len(arr(r.JSON["datapacks"])) != 0 ||
		r.JSON["has_more"] != false {
		t.Errorf("search past the end: %s", r.Text)
	}
	// A refused rename changes nothing else it was given.
	wantError(t, e, "datapack_update", with("datapack_id", id, "name", "default", "conversation_search_opt_out", true),
		"CONFLICT")
	if r = ok(t, e, "datapack_describe", with("datapack_id", id)); r.JSON["conversation_search_opt_out"] != false {
		t.Errorf("opt-out written by a refused update: %s", r.Text)
	}

	r = ok(t, e, "datapack_update", with("datapack_id", id, "description", "AgentK project \"b\""))
	if r.JSON["message"] != "datapack updated" {
		t.Errorf("update: %s", r.Text)
	}
	if r = ok(t, e, "datapack_update", with("datapack_id", id, "name", "  ")); r.JSON["result"] != "nothing_to_update" {
		t.Errorf("nothing to update: %s", r.Text)
	}
	ok(t, e, "datapack_update", with("datapack_id", id, "conversation_search_opt_out", true))
	r = ok(t, e, "datapack_describe", with("datapack_id", id))
	if r.JSON["description"] != "AgentK project \"b\"" || r.JSON["conversation_search_opt_out"] != true ||
		num(r.JSON["memory_count"]) != 0 || r.JSON["knowledge_chunk_count"] == nil {
		t.Errorf("describe: %s", r.Text)
	}

	ok(t, e, "datapack_delete", with("datapack_id", id))
	wantError(t, e, "datapack_describe", with("datapack_id", id), "datapack_access_denied")
	// Update and delete pass on the API's code for a missing datapack, as Cloud's do.
	wantError(t, e, "datapack_update", with("datapack_id", id, "description", "x"), "NOT_FOUND")
	wantError(t, e, "datapack_delete", with("datapack_id", id), "NOT_FOUND")
	// With the default datapack gone too, a call that names none has nowhere to go.
	ok(t, e, "datapack_delete", with("datapack_id", e.DatapackID))
	wantError(t, e, "memory_add", map[string]any{"text": "x", "conversation_id": "c"}, "datapack_id_required")
}

func TestTrackTokenUsage(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	r := call(t, e, "track_token_usage", map[string]any{"conversation_id": "c", "name": "LLM Response",
		"input_tokens": 120, "output_tokens": 30})
	if r.IsError || r.JSON["recorded"] != true || obj(r.Raw["structuredContent"])["recorded"] != true {
		t.Fatalf("track: %s %v", r.Text, r.Raw)
	}
	if r = call(t, e, "track_token_usage", map[string]any{"name": "x"}); r.JSON["recorded"] != false ||
		r.JSON["error"] != "conversation_id_required" {
		t.Errorf("no conversation: %s", r.Text)
	}
	r = call(t, e, "track_token_usage", map[string]any{"conversation_id": "c", "name": "x", "input_tokens": -1})
	if r.JSON["recorded"] != false {
		t.Errorf("negative: %s", r.Text)
	}
	r = call(t, e, "track_token_usage", map[string]any{"conversation_id": "c", "name": "x",
		"datapack_id": "00000000-0000-0000-0000-000000000000"})
	if r.JSON["recorded"] != false || r.JSON["error"] != "datapack_access_denied" {
		t.Errorf("missing datapack: %s", r.Text)
	}
	all, _ := e.Store.Calls(context.Background(), "", time.Time{})
	for _, c := range all {
		if c.ErrorCode != "" && c.OK {
			t.Errorf("a call that recorded nothing is logged as ok: %+v", c)
		}
	}
	calls, _ := e.Store.Calls(context.Background(), e.DatapackID, time.Time{})
	if len(calls) == 0 || calls[0].InputTokens != 120 || calls[0].OutputTokens != 30 {
		t.Errorf("recorded tokens: %+v", calls)
	}
}

func TestPromoteAndArtifacts(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	add := func(text string) string {
		r := ok(t, e, "memory_add", map[string]any{"text": text, "agent_id": "a", "conversation_id": "c"})
		return str(obj(arr(r.JSON["results"])[0])["id"])
	}
	keep, promote := add("keep me as a memory"), add("the team uses trunk-based development")
	if _, err := e.Service.KeepMemories(context.Background(), e.DatapackID, []string{keep}); err != nil {
		t.Fatal(err)
	}
	wantError(t, e, "memory_promote", map[string]any{"conversation_id": "c", "memory_ids": []string{}},
		"memory_ids_required")
	wantError(t, e, "memory_promote", map[string]any{"conversation_id": "c", "memory_ids": []string{"x"}},
		"invalid_memory_id")
	missing := "00000000-0000-0000-0000-000000000000"
	r := ok(t, e, "memory_promote", map[string]any{"conversation_id": "c", "agent_id": "a",
		"memory_ids": []string{promote, keep, missing}})
	if len(arr(r.JSON["inserted_ids"])) != 1 || str(arr(r.JSON["inserted_ids"])[0]) != promote ||
		len(arr(r.JSON["not_found_ids"])) != 2 || r.JSON["updated_ids"] == nil {
		t.Fatalf("promote: %s", r.Text)
	}
	// It left the memories: search doesn't find it, and it can't be changed by its old id.
	r = ok(t, e, "memory_search", map[string]any{"query": "trunk-based development", "conversation_id": "c"})
	for _, it := range arr(r.JSON["results"]) {
		if obj(it)["id"] == promote {
			t.Errorf("a promoted memory is still searchable: %s", r.Text)
		}
	}
	wantError(t, e, "memory_update", map[string]any{"memory_id": promote, "text": "x", "conversation_id": "c",
		"agent_id": "a"}, "memory_is_promoted")
	r = ok(t, e, "memory_get_all", map[string]any{"conversation_id": "c", "promoted": true})
	pm := arr(r.JSON["promoted_memories"])
	if len(pm) != 1 || obj(pm[0])["data"] != "the team uses trunk-based development" ||
		obj(pm[0])["promoted_to_kb_at"] == nil {
		t.Errorf("promoted list: %s", r.Text)
	}
	r = ok(t, e, "datapack_describe", map[string]any{"conversation_id": "c"})
	if num(r.JSON["memory_count"]) != 1 || num(r.JSON["learnings_count"]) != 0 ||
		num(r.JSON["collective_memory_count"]) != 1 || num(r.JSON["shared_knowledge_count"]) != 1 {
		t.Errorf("describe counts: %s", r.Text)
	}

	body := base64.StdEncoding.EncodeToString([]byte("<h1>design</h1>"))
	r = ok(t, e, "artifact_put", map[string]any{"filename": "design.html", "content_base64": body,
		"content_type": "text/html", "conversation_id": "c"})
	hash := str(r.JSON["content_hash"])
	sum := sha256.Sum256([]byte("<h1>design</h1>"))
	if hash != hex.EncodeToString(sum[:]) || r.JSON["stored_in"] != "db" || num(r.JSON["size_bytes"]) != 15 {
		t.Fatalf("put: %s", r.Text)
	}
	if again := ok(t, e, "artifact_put", map[string]any{"filename": "x.html", "content_base64": body,
		"content_type": "text/html", "conversation_id": "c"}); again.JSON["artifact_id"] != r.JSON["artifact_id"] {
		t.Errorf("same bytes, new artifact: %s", again.Text)
	}
	r = ok(t, e, "artifact_get", map[string]any{"content_hash": strings.ToUpper(hash), "conversation_id": "c"})
	if r.JSON["content_base64"] != body || r.JSON["filename"] != "design.html" {
		t.Errorf("get: %s", r.Text)
	}
	wantError(t, e, "artifact_get", map[string]any{"content_hash": strings.Repeat("0", 64), "conversation_id": "c"},
		"ARTIFACT_NOT_FOUND")
	wantError(t, e, "artifact_put", map[string]any{"filename": "x", "content_base64": "not base64!",
		"content_type": "text/plain", "conversation_id": "c"}, "invalid_base64")
	big := base64.StdEncoding.EncodeToString(make([]byte, 5<<20+1))
	wantError(t, e, "artifact_put", map[string]any{"filename": "big.bin", "content_base64": big,
		"content_type": "application/octet-stream", "conversation_id": "c"}, "artifact_too_large")
}
