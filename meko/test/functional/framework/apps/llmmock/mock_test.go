package llmmock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock/server"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mockpod"
)

func startMock(t *testing.T) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(server.New(t.Logf))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), srv.URL
}

func post(t *testing.T, url string, body any) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func embed(t *testing.T, base string, input any, extra map[string]any) [][]float64 {
	t.Helper()
	req := map[string]any{"model": "text-embedding-3-small", "input": input}
	for k, v := range extra {
		req[k] = v
	}
	resp, body := post(t, base+"/v1/embeddings", req)
	if resp.StatusCode != 200 {
		t.Fatalf("embeddings: %s: %s", resp.Status, body)
	}
	var out struct {
		Data []struct {
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	var vecs [][]float64
	for _, d := range out.Data {
		var v []float64
		if json.Unmarshal(d.Embedding, &v) != nil {
			var s string
			if err := json.Unmarshal(d.Embedding, &s); err != nil {
				t.Fatalf("embedding is neither floats nor base64: %s", d.Embedding)
			}
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+4 <= len(raw); i += 4 {
				v = append(v, float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i:]))))
			}
		}
		vecs = append(vecs, v)
	}
	return vecs
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func chat(t *testing.T, base string, req map[string]any) (int, map[string]any) {
	t.Helper()
	req["model"] = "gpt-4o-mini"
	resp, body := post(t, base+"/v1/chat/completions", req)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

func reply(out map[string]any) string {
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

func TestEmbeddingsHaveTheRequestedWidthAndSharedWordsAreClose(t *testing.T) {
	_, base := startMock(t)
	for _, tc := range []struct {
		model string
		extra map[string]any
		want  int
	}{
		{"text-embedding-3-small", nil, 1536},
		{"text-embedding-3-large", nil, 3072},
		{"text-embedding-3-large", map[string]any{"dimensions": 1536}, 1536},
		{"text-embedding-ada-002", nil, 1536},
	} {
		extra := map[string]any{"model": tc.model}
		for k, v := range tc.extra {
			extra[k] = v
		}
		if got := len(embed(t, base, "hello", extra)[0]); got != tc.want {
			t.Errorf("%s %v: %d dims, want %d", tc.model, tc.extra, got, tc.want)
		}
	}
	v := embed(t, base, []string{"the user likes green tea", "green tea is the user's favourite", "quarterly revenue forecast"}, nil)
	if near, far := cosine(v[0], v[1]), cosine(v[0], v[2]); near < 0.4 || far > 0.1 {
		t.Errorf("cosine of texts sharing words %.2f and of unrelated texts %.2f", near, far)
	}
	again := embed(t, base, "the user likes green tea", nil)[0]
	if cosine(again, v[0]) < 0.999 {
		t.Errorf("the same text embeds differently twice")
	}
}

func TestEmbeddingsAcceptBase64AndTokenIDs(t *testing.T) {
	_, base := startMock(t)
	f := embed(t, base, "alpha beta", nil)[0]
	b := embed(t, base, "alpha beta", map[string]any{"encoding_format": "base64"})[0]
	if len(b) != len(f) || cosine(f, b) < 0.9999 {
		t.Errorf("base64 and float vectors differ: %d vs %d dims", len(b), len(f))
	}
	ids := embed(t, base, [][]int{{11, 22, 33}, {11, 22, 44}, {900, 901}}, nil)
	if len(ids) != 3 || cosine(ids[0], ids[1]) < 0.3 || cosine(ids[0], ids[2]) > 0.1 {
		t.Errorf("token id inputs: %d vectors, cosines %.2f and %.2f", len(ids), cosine(ids[0], ids[1]), cosine(ids[0], ids[2]))
	}
}

func TestFixturesPinTheVectorAndPadIt(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	if err := c.SetEmbedding(ctx,
		Fixture{Text: "query", Vector: []float64{1, 0}},
		Fixture{Text: "memory", Vector: []float64{0.6, 0.8}},
		Fixture{Text: "chunk about", Vector: []float64{0, 1}, Contains: true},
	); err != nil {
		t.Fatal(err)
	}
	v := embed(t, base, []string{"query", "memory", "a chunk about cats", "plain"}, nil)
	if len(v[0]) != 1536 || v[0][0] != 1 || v[0][5] != 0 {
		t.Errorf("fixture not padded to the model width: %d dims, %v", len(v[0]), v[0][:3])
	}
	if got := cosine(v[0], v[1]); math.Abs(got-0.6) > 1e-6 {
		t.Errorf("cosine(query, memory) = %.3f, want 0.6", got)
	}
	if got := cosine(v[0], v[2]); math.Abs(got) > 1e-6 {
		t.Errorf("a Contains fixture did not match: cosine %.3f", got)
	}
	calls, _ := c.Calls(ctx, Filter{Route: RouteEmbeddings})
	if len(calls) != 1 || calls[0].Answer != "fixture,hash" || len(calls[0].Inputs) != 4 {
		t.Errorf("recorded %+v", calls)
	}
	if err := c.ClearEmbedding(ctx, "query"); err != nil {
		t.Fatal(err)
	}
	if again := embed(t, base, "query", nil)[0]; again[0] == 1 && again[1] == 0 {
		t.Errorf("the cleared fixture still answers")
	}
}

const mem0Extract = "You are a Personal Information Organizer. Extract facts."

func TestBuiltInAnswersForTheStackPrompts(t *testing.T) {
	_, base := startMock(t)
	_, out := chat(t, base, map[string]any{"messages": []any{
		map[string]string{"role": "system", "content": mem0Extract},
		map[string]string{"role": "user", "content": "Input:\nuser: I drink green tea. My dog is Rex.\nassistant: Noted, thanks."},
	}})
	var facts struct{ Facts []string }
	if err := json.Unmarshal([]byte(reply(out)), &facts); err != nil || len(facts.Facts) != 2 || facts.Facts[1] != "My dog is Rex." {
		t.Errorf("extraction answered %q (%v)", reply(out), err)
	}

	extract := "# ROLE\n\nYou are a Memory Extractor.\n\n# INPUTS\n\n## New Messages\n\nDo NOT extract:\n- Generic assistant acknowledgments\n\n## Summary\n\nA narrative summary.\n"
	turn := "## Summary\n\n## Existing Memories\n[]\n\n## New Messages\nuser: I drink green tea. My dog is Rex.\n\n\n## Observation Date\n2026-10-02"
	_, out = chat(t, base, map[string]any{"messages": []any{
		map[string]string{"role": "system", "content": extract},
		map[string]string{"role": "user", "content": turn},
	}})
	var mem struct{ Memory []map[string]string }
	if err := json.Unmarshal([]byte(reply(out)), &mem); err != nil || len(mem.Memory) != 2 || mem.Memory[0]["id"] != "0" || mem.Memory[1]["text"] != "My dog is Rex." {
		t.Errorf("single-call extraction answered %q (%v)", reply(out), err)
	}

	update := "You are a smart memory manager. Current memory: []. The retrieved facts:\n```\n{\"facts\": [\"I drink green tea.\"]}\n```"
	_, out = chat(t, base, map[string]any{"messages": []any{map[string]string{"role": "user", "content": update}}})
	var ev struct{ Memory []map[string]string }
	if err := json.Unmarshal([]byte(reply(out)), &ev); err != nil || len(ev.Memory) != 1 || ev.Memory[0]["event"] != "ADD" || ev.Memory[0]["text"] != "I drink green tea." {
		t.Errorf("update answered %q (%v)", reply(out), err)
	}

	_, out = chat(t, base, map[string]any{"messages": []any{map[string]string{"role": "user", "content": "plain question"}}})
	if !strings.Contains(reply(out), "plain question") {
		t.Errorf("default answer %q", reply(out))
	}
}

func TestScriptsAnswerFirstMatchAndRunOut(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	id, err := c.AddScript(ctx, Script{Match: "capital of France", Reply: "Paris, scripted.", Times: 2})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(q string) string {
		_, out := chat(t, base, map[string]any{"messages": []any{map[string]string{"role": "user", "content": q}}})
		return reply(out)
	}
	for i := 0; i < 2; i++ {
		if got := ask("What is the capital of France?"); got != "Paris, scripted." {
			t.Errorf("call %d: %q", i, got)
		}
	}
	if got := ask("What is the capital of France?"); strings.Contains(got, "scripted") {
		t.Errorf("a script with Times 2 answered a third time: %q", got)
	}
	if got := ask("something else"); strings.Contains(got, "scripted") {
		t.Errorf("a script answered a prompt it does not match: %q", got)
	}
	calls, _ := c.Calls(ctx, Filter{Contains: "capital of France"})
	if len(calls) != 3 || calls[0].Answer != "script:"+id || !strings.HasPrefix(calls[2].Answer, "builtin:") {
		t.Errorf("recorded answers %v", answers(calls))
	}
}

func TestScriptSystemMatchTellsCallersApart(t *testing.T) {
	c, base := startMock(t)
	if _, err := c.AddScript(context.Background(), Script{Match: "same question", System: "gateway persona", Reply: "gateway only"}); err != nil {
		t.Fatal(err)
	}
	ask := func(system string) string {
		_, out := chat(t, base, map[string]any{"messages": []any{
			map[string]string{"role": "system", "content": system},
			map[string]string{"role": "user", "content": "same question"},
		}})
		return reply(out)
	}
	if got := ask("the gateway persona is here"); got != "gateway only" {
		t.Errorf("the gateway call got %q", got)
	}
	if got := ask("another caller"); got == "gateway only" {
		t.Errorf("the script answered a call with a different system prompt")
	}
}

func answers(calls []Call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Answer)
	}
	return out
}

func TestToolCallScriptsNeedToolsAndStopAfterTheResult(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	if _, err := c.AddScript(ctx, Script{Match: "list my memories", ToolCalls: []ToolCall{{Name: "memory_get_all", Arguments: map[string]any{"limit": 3}}}}); err != nil {
		t.Fatal(err)
	}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "memory_get_all"}}}
	user := map[string]string{"role": "user", "content": "please list my memories"}

	_, out := chat(t, base, map[string]any{"messages": []any{user}, "tools": tools})
	choice := out["choices"].([]any)[0].(map[string]any)
	calls := choice["message"].(map[string]any)["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || fn["name"] != "memory_get_all" || fn["arguments"] != `{"limit": 3}` {
		t.Errorf("tool call reply %v", choice)
	}

	_, out = chat(t, base, map[string]any{"messages": []any{user}})
	if reply(out) == "" {
		t.Errorf("a request with no tools got no text: %v", out)
	}

	_, out = chat(t, base, map[string]any{"tools": tools, "messages": []any{user,
		map[string]any{"role": "assistant", "tool_calls": calls},
		map[string]any{"role": "tool", "tool_call_id": "x", "content": "[]"}}})
	if out["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("the script answered again after the tool result: %v", out)
	}
}

func sse(t *testing.T, base string, body map[string]any) (text string, done bool) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			done = true
		}
		if rest, ok := strings.CutPrefix(line, "data: {"); ok {
			var chunk struct {
				Choices []struct{ Delta struct{ Content string } }
			}
			if json.Unmarshal([]byte("{"+rest), &chunk) == nil && len(chunk.Choices) > 0 {
				text += chunk.Choices[0].Delta.Content
			}
		}
	}
	return text, done
}

func TestStreamingChat(t *testing.T) {
	c, base := startMock(t)
	if _, err := c.AddScript(context.Background(), Script{Match: "stream me", Reply: "one two three four five six seven"}); err != nil {
		t.Fatal(err)
	}
	text, done := sse(t, base, map[string]any{"model": "m", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "stream me"}}})
	if text != "one two three four five six seven" || !done {
		t.Errorf("streamed %q, done %v", text, done)
	}
	calls, _ := c.Calls(context.Background(), Filter{Route: RouteChat})
	if len(calls) != 1 || !calls[0].Stream {
		t.Errorf("recorded %+v", calls)
	}
}

func TestResponsesAPI(t *testing.T) {
	_, base := startMock(t)
	_, body := post(t, base+"/v1/responses", map[string]any{"model": "gpt-4.1-nano", "input": "Generate a summary of the following text: alpha beta"})
	var out struct {
		Output []struct {
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Output[0].Content[0].Text != "Summary: alpha beta" {
		t.Errorf("responses answered %s (%v)", body, err)
	}
}

func TestFaults(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	req := func(route string, text string) (*http.Response, []byte) {
		if route == RouteEmbeddings {
			return post(t, base+"/v1/embeddings", map[string]any{"model": "m", "input": text})
		}
		return post(t, base+"/v1/chat/completions", map[string]any{"model": "m", "messages": []any{map[string]string{"role": "user", "content": text}}})
	}

	rate := RateLimit(7 * time.Second)
	rate.Route, rate.Match, rate.Times = RouteEmbeddings, "marker-429", 2
	id, err := c.AddFault(ctx, rate)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		resp, body := req(RouteEmbeddings, "has marker-429 inside")
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || !strings.Contains(string(body), "error") {
			t.Errorf("call %d: %s retry-after %q %s", i, resp.Status, resp.Header.Get("Retry-After"), body)
		}
	}
	if resp, _ := req(RouteEmbeddings, "has marker-429 inside"); resp.StatusCode != 200 {
		t.Errorf("the fault outlived its Times: %s", resp.Status)
	}
	if resp, _ := req(RouteEmbeddings, "no marker"); resp.StatusCode != 200 {
		t.Errorf("a fault hit a request that does not match: %s", resp.Status)
	}
	if err := c.RemoveFault(ctx, id); err != nil {
		t.Fatal(err)
	}

	f500 := HTTPStatus(500)
	f500.Route, f500.Match = RouteChat, "marker-500"
	if _, err := c.AddFault(ctx, f500); err != nil {
		t.Fatal(err)
	}
	if resp, _ := req(RouteChat, "marker-500"); resp.StatusCode != 500 {
		t.Errorf("chat fault: %s", resp.Status)
	}
	if resp, _ := req(RouteEmbeddings, "marker-500"); resp.StatusCode != 200 {
		t.Errorf("a chat fault hit embeddings: %s", resp.Status)
	}

	bad := Malformed()
	bad.Match = "marker-bad"
	if _, err := c.AddFault(ctx, bad); err != nil {
		t.Fatal(err)
	}
	resp, body := req(RouteChat, "marker-bad")
	if resp.StatusCode != 200 || json.Valid(body) {
		t.Errorf("malformed fault: %s, valid JSON %v", resp.Status, json.Valid(body))
	}
	if calls, _ := c.Calls(ctx, Filter{Contains: "marker-bad"}); len(calls) != 1 || calls[0].Status != 200 {
		t.Errorf("the malformed call was recorded as %+v", calls)
	}

	slow := Latency(300 * time.Millisecond)
	slow.Match = "marker-slow"
	if _, err := c.AddFault(ctx, slow); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if resp, _ := req(RouteChat, "marker-slow"); resp.StatusCode != 200 || time.Since(start) < 300*time.Millisecond {
		t.Errorf("latency fault: %s after %s", resp.Status, time.Since(start))
	}

	hang := Timeout(5 * time.Second)
	hang.Match = "marker-hang"
	if _, err := c.AddFault(ctx, hang); err != nil {
		t.Fatal(err)
	}
	short := &http.Client{Timeout: 400 * time.Millisecond}
	b, _ := json.Marshal(map[string]any{"model": "m", "input": "marker-hang"})
	if _, err := short.Post(base+"/v1/embeddings", "application/json", bytes.NewReader(b)); err == nil {
		t.Errorf("a timeout fault answered")
	}

	calls, _ := c.Calls(ctx, Filter{Contains: "marker-429"})
	if len(calls) < 3 || calls[0].Status != 429 || calls[0].Fault != id || calls[2].Status != 200 {
		t.Errorf("recorded %+v", calls)
	}
}

func TestCutStreamEndsBeforeTheTerminator(t *testing.T) {
	c, base := startMock(t)
	cut := CutStream()
	cut.Match = "marker-cut"
	if _, err := c.AddFault(context.Background(), cut); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "marker-cut"}}})
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("a cut stream read to a clean end: %q", body)
	}
	if bytes.Contains(body, []byte("[DONE]")) {
		t.Errorf("a cut stream carries the terminator: %q", body)
	}
}

type frame struct {
	event   string
	payload map[string]any
}

func eventStream(t *testing.T, raw []byte) []frame {
	t.Helper()
	var out []frame
	for len(raw) > 0 {
		total := int(binary.BigEndian.Uint32(raw[0:4]))
		hlen := int(binary.BigEndian.Uint32(raw[4:8]))
		if total > len(raw) || crc32.ChecksumIEEE(raw[:8]) != binary.BigEndian.Uint32(raw[8:12]) ||
			crc32.ChecksumIEEE(raw[:total-4]) != binary.BigEndian.Uint32(raw[total-4:total]) {
			t.Fatalf("bad event stream frame")
		}
		headers := raw[12 : 12+hlen]
		var event string
		for len(headers) > 0 {
			nl := int(headers[0])
			name := string(headers[1 : 1+nl])
			vl := int(binary.BigEndian.Uint16(headers[2+nl : 4+nl]))
			val := string(headers[4+nl : 4+nl+vl])
			if name == ":event-type" {
				event = val
			}
			headers = headers[4+nl+vl:]
		}
		var payload map[string]any
		if err := json.Unmarshal(raw[12+hlen:total-4], &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, frame{event, payload})
		raw = raw[total:]
	}
	return out
}

func TestBedrockConverseConverseStreamAndInvoke(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	if _, err := c.AddScript(ctx, Script{Match: "bedrock-q", Reply: "scripted nova words here"}); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]string{"text": "bedrock-q"}}}}}

	resp, raw := post(t, base+"/model/us.amazon.nova-2-lite-v1%3A0/converse", body)
	var conv struct {
		Output struct {
			Message struct{ Content []map[string]string }
		}
		StopReason string
	}
	if err := json.Unmarshal(raw, &conv); err != nil || resp.StatusCode != 200 || conv.Output.Message.Content[0]["text"] != "scripted nova words here" || conv.StopReason != "end_turn" {
		t.Errorf("converse: %s %s", resp.Status, raw)
	}

	_, raw = post(t, base+"/model/us.amazon.nova-2-lite-v1%3A0/converse-stream", body)
	var text, stop string
	for _, f := range eventStream(t, raw) {
		switch f.event {
		case "contentBlockDelta":
			text += f.payload["delta"].(map[string]any)["text"].(string)
		case "messageStop":
			stop = f.payload["stopReason"].(string)
		}
	}
	if text != "scripted nova words here" || stop != "end_turn" {
		t.Errorf("converse-stream: %q stop %q", text, stop)
	}

	_, raw = post(t, base+"/model/amazon.titan-embed-text-v2%3A0/invoke", map[string]any{"inputText": "hello bedrock"})
	var emb struct{ Embedding []float64 }
	if err := json.Unmarshal(raw, &emb); err != nil || len(emb.Embedding) != 1024 {
		t.Errorf("titan v2 embedding: %d dims (%v)", len(emb.Embedding), err)
	}
	_, raw = post(t, base+"/model/amazon.titan-embed-text-v1/invoke", map[string]any{"inputText": "hello bedrock"})
	if err := json.Unmarshal(raw, &emb); err != nil || len(emb.Embedding) != 1536 {
		t.Errorf("titan v1 embedding: %d dims", len(emb.Embedding))
	}

	th := RateLimit(time.Second)
	th.Route, th.Match = RouteBedrock, "bedrock-throttle"
	if _, err := c.AddFault(ctx, th); err != nil {
		t.Fatal(err)
	}
	body["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]string{"text": "bedrock-throttle"}}
	resp, _ = post(t, base+"/model/m/converse", body)
	if resp.StatusCode != 429 || resp.Header.Get("x-amzn-ErrorType") != "ThrottlingException" {
		t.Errorf("bedrock throttle: %s, %q", resp.Status, resp.Header.Get("x-amzn-ErrorType"))
	}

	calls, _ := c.Calls(ctx, Filter{Route: RouteConverseStream})
	if len(calls) != 1 || calls[0].API != "bedrock" || !calls[0].Stream || calls[0].Model != "us.amazon.nova-2-lite-v1:0" {
		t.Errorf("recorded %+v", calls)
	}
}

func TestInvokeModelCohereEmbeddingsAndAnthropicChat(t *testing.T) {
	_, base := startMock(t)
	_, raw := post(t, base+"/model/cohere.embed-english-v3/invoke", map[string]any{"texts": []string{"one", "two"}, "input_type": "search_document"})
	var coh struct{ Embeddings [][]float64 }
	if err := json.Unmarshal(raw, &coh); err != nil || len(coh.Embeddings) != 2 || len(coh.Embeddings[0]) != 1024 {
		t.Errorf("cohere embed: %s", raw)
	}
	_, raw = post(t, base+"/model/us.anthropic.claude-3-5-haiku-20241022-v1%3A0/invoke", map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"messages":          []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "hello claude"}}}},
	})
	var msg struct {
		Content    []map[string]string
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) != 1 || !strings.Contains(msg.Content[0]["text"], "hello claude") || msg.StopReason != "end_turn" {
		t.Errorf("anthropic invoke: %s", raw)
	}
}

func TestCallsFiltersAndReset(t *testing.T) {
	c, base := startMock(t)
	ctx := context.Background()
	embed(t, base, "first-text", nil)
	chat(t, base, map[string]any{"messages": []any{map[string]string{"role": "user", "content": "second-text"}}})
	all, _ := c.Calls(ctx, Filter{})
	if len(all) != 2 || all[0].Route != RouteEmbeddings || all[1].Route != RouteChatCompletions || all[1].Messages[0].Role != "user" {
		t.Fatalf("recorded %+v", all)
	}
	if got, _ := c.Calls(ctx, Filter{Contains: "second"}); len(got) != 1 || got[0].Route != RouteChatCompletions {
		t.Errorf("contains filter: %+v", got)
	}
	if got, _ := c.Calls(ctx, Filter{After: all[0].Seq}); len(got) != 1 {
		t.Errorf("after filter: %+v", got)
	}
	if err := c.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Calls(ctx, Filter{}); len(got) != 0 {
		t.Errorf("calls after reset: %+v", got)
	}
}

func TestManifestRunsTheBinaryAndCarriesTheJanitorLabels(t *testing.T) {
	m, err := Manifest(Spec{Name: "llmmock"}, mockpod.Binary{Path: "/x/mekoqa", SHA: "0123456789abcdef0123"})
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Kind     string
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(m), &list); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, it := range list.Items {
		kinds[it.Kind] = true
		if it.Metadata.Labels["mekoqa"] != "1" || it.Metadata.Labels["mekoqa-test"] != "true" {
			t.Errorf("%s lacks the janitor labels: %v", it.Kind, it.Metadata.Labels)
		}
	}
	if len(kinds) != 2 || !kinds["Deployment"] || !kinds["Service"] {
		t.Errorf("kinds %v, want Deployment and Service", kinds)
	}
	for _, want := range []string{`"mirror.gcr.io/library/alpine:3.20"`, `/opt/mekoqa/mekoqa`, `"mock","llm","--port","8080"`, `/_mock/health`, `"mekoqa/bin":"0123456789abcdef"`} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %s", want)
		}
	}
	if got := OpenAIBaseURL("llmmock", "ns1"); got != "http://llmmock.ns1.svc.cluster.local:8080/v1" {
		t.Errorf("OpenAIBaseURL = %s", got)
	}
}

func TestStatsCountEveryCallPastTheRing(t *testing.T) {
	c, base := startMock(t)
	embed(t, base, "one", nil)
	embed(t, base, "two", nil)
	chat(t, base, map[string]any{"model": "gpt-4o-mini", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	st, err := c.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Routes[RouteEmbeddings] != 2 || st.Routes[RouteChatCompletions] != 1 || st.Seq != 3 {
		t.Errorf("stats %+v, want 2 embeddings and 1 chat over seq 3", st)
	}
	if err := c.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Stats(context.Background()); len(st.Routes) != 0 {
		t.Errorf("stats after reset %+v", st)
	}
}
