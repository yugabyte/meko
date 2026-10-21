package mcp_test

import (
	"encoding/base64"
	"regexp"
	"testing"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

// TestAgentKContract replays the calls AgentK Local's Meko client makes
// (extensions/agentk/src/server/meko.ts, MekoClient), with the same tool names and arguments, and
// checks every reply has what MekoClient reads from it. Keep it in step with meko.ts: when a method
// there changes a call or reads a new field, change its step here. #189 replaces this with AgentK's
// own tests against Meko.
//
// MekoClient treats a reply with an "error" key, or isError, as a failure, so the steps that must
// succeed check both. Its transport behavior (retries, timeouts, reconnecting) is its own, and
// stays tested against AgentK's FakeMeko.
func TestAgentKContract(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)

	// check(): a ping.
	if status, reply := rpc(t, e, testenv.ClientToken, map[string]any{"id": 1, "method": "ping"}); status != 200 ||
		reply["result"] == nil {
		t.Fatalf("check: %d %v", status, reply)
	}

	// serviceConv() -> ensureConversation(AGENTK_AGENT_ID, null, SERVICE_KEY, ...): reads id.
	svc := ok(t, e, "conversation_create", map[string]any{"agent_id": "agent-k-local", "datapack_id": nil,
		"session_id": "agent-k-local-service", "title": "AgentK Local service"})
	conv := str(svc.JSON["id"])
	if !hex32.MatchString(conv) {
		t.Fatalf("ensureConversation: id %q", conv)
	}

	// listDatapacks(): datapack_list {conversation_id}: a bare array, each with datapack_id and
	// datapack_name, and grant, created_at, description, user_id, owner name and email if any.
	r := ok(t, e, "datapack_list", map[string]any{"conversation_id": conv})
	if len(r.List) == 0 {
		t.Fatalf("listDatapacks: %s", r.Text)
	}
	row := obj(r.List[0])
	if str(row["datapack_id"]) == "" || str(row["datapack_name"]) == "" || row["grant"] != "owner" ||
		row["created_at"] == nil || row["user_id"] == nil {
		t.Errorf("listDatapacks row: %v", row)
	}

	// createDatapack(name): datapack_create {name, conversation_id}: reads datapack_id and datapack_name
	// (or name).
	r = ok(t, e, "datapack_create", map[string]any{"name": "adlc/proj/acme/app", "conversation_id": conv})
	dp := str(r.JSON["datapack_id"])
	if dp == "" || r.JSON["name"] != "adlc/proj/acme/app" {
		t.Fatalf("createDatapack: %s", r.Text)
	}

	// setDatapackDescription(id, d): datapack_update {datapack_id, description, conversation_id}.
	ok(t, e, "datapack_update", map[string]any{"datapack_id": dp, "description": "AgentK Local project \"app\"",
		"conversation_id": conv})

	// describeDatapack(id): datapack_describe {datapack_id, conversation_id}: reads memory_count and
	// knowledge_chunk_count (numbers), grant, owner name and email.
	r = ok(t, e, "datapack_describe", map[string]any{"datapack_id": dp, "conversation_id": conv})
	if _, isNum := r.JSON["memory_count"].(float64); !isNum || r.JSON["grant"] != "owner" {
		t.Errorf("describeDatapack: %s", r.Text)
	}
	if _, isNum := r.JSON["knowledge_chunk_count"].(float64); !isNum {
		t.Errorf("describeDatapack's knowledge_chunk_count: %s", r.Text)
	}

	// A session's conversation, then retitleConversation(agentId, datapackId, id, title):
	// conversation_update {agent_id, datapack_id, conversation_id, title}.
	agent := "claude_code:app-1a2b3c4d"
	sess := str(ok(t, e, "conversation_create", map[string]any{"agent_id": agent, "datapack_id": dp,
		"session_id": "0b5c9d6e-1111-4222-8333-944455556666", "title": "Fix the build"}).JSON["id"])
	ok(t, e, "conversation_update", map[string]any{"agent_id": agent, "datapack_id": dp, "conversation_id": sess,
		"title": "Fix the build (Demo)"})
	// Another agent can't: an error reply MekoClient raises.
	wantError(t, e, "conversation_update", map[string]any{"agent_id": "claude_code:other", "datapack_id": dp,
		"conversation_id": sess, "title": "x"}, "agent_id_mismatch")

	// addMemory(text, datapackId, agentId, conversationId, metadata): memory_add with metadata as a
	// JSON string (or null).
	ok(t, e, "memory_add", map[string]any{"text": "Gotcha: migrations are append-only.", "datapack_id": dp,
		"agent_id": agent, "conversation_id": sess, "metadata": `{"source":"agent-k-local","kind":"learned"}`})
	ok(t, e, "memory_add", map[string]any{"text": "Decision: ship the rename first.", "datapack_id": dp,
		"agent_id": agent, "conversation_id": sess, "metadata": nil})

	// similar(query, datapackId, agentId, conversationId, 3): context_search with force_source memory:
	// reads results.memory[].id, .memory and .agent_id.
	r = ok(t, e, "context_search", map[string]any{"query": "migrations are append-only", "conversation_id": sess,
		"datapack_id": dp, "agent_id": "claude_code:me", "force_source": "memory", "limit": 3})
	mems := arr(obj(r.JSON["results"])["memory"])
	if len(mems) == 0 {
		t.Fatalf("similar: %s", r.Text)
	}
	first := obj(mems[0])
	id := str(first["id"])
	if id == "" || first["memory"] != "Gotcha: migrations are append-only." || first["agent_id"] != agent {
		t.Errorf("similar's item: %v", first)
	}

	// updateMemory(id, text, datapackId, agentId, conversationId): memory_update, as the writer...
	ok(t, e, "memory_update", map[string]any{"memory_id": id,
		"text": "Gotcha: migrations are append-only; never edit one.", "datapack_id": dp, "agent_id": agent,
		"conversation_id": sess})
	// ...and with agent_id null (a memory whose writer AgentK doesn't know): an error reply.
	wantError(t, e, "memory_update", map[string]any{"memory_id": id, "text": "x", "datapack_id": dp,
		"agent_id": nil, "conversation_id": sess}, "not_found")

	// search(query, datapack, agentId, conversationId): context_search, limit 8: reads results.memory,
	// results.knowledge_base and results.conversation.hits, each item's text and score.
	r = ok(t, e, "context_search", map[string]any{"query": "migrations", "conversation_id": conv, "datapack_id": dp,
		"agent_id": agent, "limit": 8})
	res := obj(r.JSON["results"])
	if len(arr(res["memory"])) == 0 || obj(arr(res["memory"])[0])["score"] == nil {
		t.Errorf("search's memories: %s", r.Text)
	}
	if conv, isObj := res["conversation"].(map[string]any); !isObj || conv["hits"] == nil {
		t.Errorf("search's conversation bucket: %s", r.Text)
	}

	// putArtifact(filename, content, contentType, ...): artifact_put: reads content_hash, 64 hex.
	content := "# Design\n\nThe whole design."
	r = ok(t, e, "artifact_put", map[string]any{"filename": "design.md",
		"content_base64": base64.StdEncoding.EncodeToString([]byte(content)), "content_type": "text/markdown",
		"datapack_id": dp, "agent_id": agent, "conversation_id": sess})
	hash := str(r.JSON["content_hash"])
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		t.Fatalf("putArtifact: %s", r.Text)
	}
	// getArtifact(hash, ...): artifact_get: reads content_base64 (or local_path).
	r = ok(t, e, "artifact_get", map[string]any{"content_hash": hash, "datapack_id": dp, "agent_id": agent,
		"conversation_id": sess})
	if b, _ := base64.StdEncoding.DecodeString(str(r.JSON["content_base64"])); string(b) != content {
		t.Errorf("getArtifact: %s", r.Text)
	}
	// A hash Meko doesn't have: an error whose text says not found, which MekoClient reads as missing.
	r = call(t, e, "artifact_get", map[string]any{"content_hash": "ab" + hash[2:], "datapack_id": dp,
		"agent_id": agent, "conversation_id": sess})
	if r.IsError || !regexp.MustCompile(`(?i)not[_ ]found|expired`).MatchString(str(r.JSON["error"])+" "+
		str(r.JSON["detail"])) {
		t.Errorf("getArtifact of a missing hash: %s", r.Text)
	}
}
