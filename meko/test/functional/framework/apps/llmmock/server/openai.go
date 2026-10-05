package server

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	embeddingLabels = []string{"*", "embeddings", "openai"}
	chatLabels      = []string{"*", "chat", "chat_completions", "openai"}
	responsesLabels = []string{"*", "chat", "openai", "responses"}
)

func joinTexts(messages []message) string {
	texts := make([]string, len(messages))
	for i, m := range messages {
		texts[i] = m.Text
	}
	return strings.Join(texts, "\n")
}

func (s *Server) faulted(w http.ResponseWriter, r *http.Request, c *call, labels []string, text, api string) bool {
	labels = slices.Clone(labels)
	sort.Strings(labels)
	s.st.update(c, func(c *call) { c.Labels = labels })
	faults := s.st.takeFaults(labels, text)
	for _, f := range faults {
		s.st.update(c, func(c *call) { c.Fault = f.ID })
		if f.DelayMS > 0 && !sleepCtx(r, time.Duration(f.DelayMS)*time.Millisecond) {
			return true
		}
		switch f.Kind {
		case "status", "malformed", "cut_stream", "timeout":
			status := s.faultResponse(w, r, f, api)
			s.st.update(c, func(c *call) { c.Status = status })
			return true
		}
	}
	return false
}

func (s *Server) faultResponse(w http.ResponseWriter, r *http.Request, f fault, api string) int {
	switch f.Kind {
	case "timeout":
		ms := f.HangMS
		if ms == 0 {
			ms = hangDefaultMS
		}
		sleepCtx(r, time.Duration(ms)*time.Millisecond)
		panic(http.ErrAbortHandler)
	case "malformed":
		writeRaw(w, 200, []byte(`{"id": "llmmock", "choices": [{"message": {"content": "trunc`), nil)
		return 200
	case "cut_stream":
		ct, partial := "application/vnd.amazon.eventstream", []byte{0, 0}
		if api == "openai" {
			ct, partial = "text/event-stream", []byte(`data: {"id": "llmmock", "choices": [{"delta": {"content": "cut`)
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(200)
		_, _ = w.Write(partial)
		flush(w)
		panic(http.ErrAbortHandler)
	}
	status := f.Status
	if status == 0 {
		status = 500
	}
	headers := map[string]string{}
	if status == 429 {
		retry := f.RetryAfter
		if retry == 0 {
			retry = 1
		}
		headers["Retry-After"] = itoa(retry)
	}
	msg := "llmmock injected fault " + f.ID
	if api == "bedrock" {
		switch status {
		case 429:
			headers["x-amzn-ErrorType"] = "ThrottlingException"
		case 503:
			headers["x-amzn-ErrorType"] = "ServiceUnavailableException"
		default:
			headers["x-amzn-ErrorType"] = "InternalServerException"
		}
		writeJSON(w, status, obj{"message": msg}, headers)
		return status
	}
	writeJSON(w, status, obj{"error": obj{"message": msg, "type": "server_error", "code": itoa(status)}}, headers)
	return status
}

func (s *Server) openaiEmbeddings(w http.ResponseWriter, r *http.Request, body obj) {
	model := "text-embedding-3-small"
	if v, ok := body["model"]; ok {
		model = str(v)
	}
	var raw any = ""
	if v, ok := body["input"]; ok {
		raw = v
	}
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	if len(items) > 0 && slices.IndexFunc(items, func(x any) bool { return !isInt(x) }) < 0 {
		items = []any{items}
	}
	texts := make([]string, len(items))
	for i, x := range items {
		switch v := x.(type) {
		case string:
			texts[i] = v
		case []any:
			parts := make([]string, len(v))
			for j, p := range v {
				parts[j] = str(p)
			}
			texts[i] = strings.Join(parts, " ")
		default:
			texts[i] = str(v)
		}
	}
	text := strings.Join(texts, "\n")
	c := s.st.record(call{Route: "embeddings", API: "openai", Model: model, Path: r.URL.RequestURI(), Inputs: texts, Text: text})
	t0 := time.Now()
	if s.faulted(w, r, c, embeddingLabels, text, "openai") {
		return
	}
	dim := embeddingDim(model, body["dimensions"])
	data := make([]obj, len(items))
	answers := map[string]bool{}
	for i, orig := range items {
		vec, how := s.embed(texts[i], orig, dim)
		answers[how] = true
		var emb any = vec
		if body["encoding_format"] == "base64" {
			buf := make([]byte, 4*len(vec))
			for j, x := range vec {
				binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(float32(x)))
			}
			emb = base64.StdEncoding.EncodeToString(buf)
		}
		data[i] = obj{"object": "embedding", "index": i, "embedding": emb}
	}
	toks := 0
	for _, t := range texts {
		toks += approxTokens(t)
	}
	hows := make([]string, 0, len(answers))
	for k := range answers {
		hows = append(hows, k)
	}
	sort.Strings(hows)
	s.st.update(c, func(c *call) {
		c.Status, c.Answer, c.Dims, c.DurationMS = 200, strings.Join(hows, ","), dim, int(time.Since(t0).Milliseconds())
	})
	writeJSON(w, 200, obj{
		"object": "list", "data": data, "model": model,
		"usage": obj{"prompt_tokens": toks, "total_tokens": toks},
	}, nil)
}

func (s *Server) embed(text string, orig any, dim int) ([]float64, string) {
	if fx := s.st.fixtureFor(text); fx != nil {
		return fit(fx.Vector, dim), "fixture"
	}
	var tokens []string
	if l, ok := orig.([]any); ok {
		for _, t := range l {
			tokens = append(tokens, str(t))
		}
	} else {
		tokens = wordRe.FindAllString(strings.ToLower(text), -1)
	}
	return hashVector(tokens, dim), "hash"
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request, body obj) {
	model := "gpt-4o-mini"
	if v, ok := body["model"]; ok {
		model = str(v)
	}
	messages := normalizeOpenAIMessages(body["messages"])
	tools := toolNames(body, "openai")
	stream := truthy(body["stream"])
	c := s.st.record(call{Route: "chat_completions", API: "openai", Model: model, Path: r.URL.RequestURI(),
		Messages: messages, Tools: tools, Stream: stream, Text: joinTexts(messages)})
	t0 := time.Now()
	if s.faulted(w, r, c, chatLabels, c.Text, "openai") {
		return
	}
	wantsJSON := false
	if rf, ok := body["response_format"]; truthy(rf) && ok {
		t := "text"
		if v, ok := asObj(rf)["type"]; ok {
			t = str(v)
		}
		wantsJSON = t != "text"
	}
	answer, text, calls := s.chatPlan(chatLabels, messages, tools, wantsJSON)
	s.st.update(c, func(c *call) {
		c.Status, c.Answer, c.DurationMS = 200, answer, int(time.Since(t0).Milliseconds())
	})
	cid, created := "chatcmpl-"+hexID(20), time.Now().Unix()
	in, out := approxTokens(c.Text), approxTokens(text)
	usage := obj{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	toolCalls := make([]obj, len(calls))
	for i, tc := range calls {
		toolCalls[i] = obj{"id": tc.ID, "type": "function", "function": obj{"name": tc.Name, "arguments": spacedJSON(tc.Arguments)}}
	}
	if !stream {
		msg := obj{"role": "assistant", "content": text}
		if len(calls) > 0 {
			msg["content"] = nil
			msg["tool_calls"] = toolCalls
		}
		writeJSON(w, 200, obj{
			"id": cid, "object": "chat.completion", "created": created, "model": model,
			"choices": []obj{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   usage,
		}, nil)
		return
	}
	s.streamOpenAI(w, cid, created, model, text, toolCalls, finish, usage, body)
}

func (s *Server) streamOpenAI(w http.ResponseWriter, cid string, created int64, model, text string, toolCalls []obj, finish string, usage obj, body obj) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "close")
	w.WriteHeader(200)

	emit := func(delta obj, fin any, withUsage obj) {
		choices := []obj{}
		if delta != nil {
			choices = []obj{{"index": 0, "delta": delta, "finish_reason": fin}}
		}
		chunk := obj{"id": cid, "object": "chat.completion.chunk", "created": created, "model": model, "choices": choices}
		if withUsage != nil {
			chunk["usage"] = withUsage
		}
		_, _ = w.Write([]byte("data: " + compact(chunk) + "\n\n"))
		flush(w)
	}

	emit(obj{"role": "assistant", "content": ""}, nil, nil)
	if len(toolCalls) > 0 {
		for i, tc := range toolCalls {
			fn := tc["function"].(obj)
			emit(obj{"tool_calls": []obj{{"index": i, "id": tc["id"], "type": "function",
				"function": obj{"name": fn["name"], "arguments": ""}}}}, nil, nil)
			args := []rune(fn["arguments"].(string))
			for j := 0; j < len(args); j += 16 {
				emit(obj{"tool_calls": []obj{{"index": i, "function": obj{"arguments": string(args[j:min(j+16, len(args))])}}}}, nil, nil)
			}
		}
	} else {
		for _, piece := range chunks(text, 3) {
			emit(obj{"content": piece}, nil, nil)
		}
	}
	emit(obj{}, finish, nil)
	if truthy(asObj(body["stream_options"])["include_usage"]) {
		emit(nil, nil, usage)
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flush(w)
}

func (s *Server) openaiResponses(w http.ResponseWriter, r *http.Request, body obj) {
	model := "gpt-4o-mini"
	if v, ok := body["model"]; ok {
		model = str(v)
	}
	messages := normalizeResponsesInput(body)
	var tools []string
	for _, t := range asList(body["tools"]) {
		if n := str(asObj(t)["name"]); n != "" {
			tools = append(tools, n)
		}
	}
	c := s.st.record(call{Route: "responses", API: "openai", Model: model, Path: r.URL.RequestURI(),
		Messages: messages, Tools: tools, Text: joinTexts(messages)})
	t0 := time.Now()
	if s.faulted(w, r, c, responsesLabels, c.Text, "openai") {
		return
	}
	answer, text, calls := s.chatPlan(responsesLabels, messages, tools, false)
	s.st.update(c, func(c *call) {
		c.Status, c.Answer, c.DurationMS = 200, answer, int(time.Since(t0).Milliseconds())
	})
	output := []obj{}
	if len(calls) > 0 {
		for _, tc := range calls {
			output = append(output, obj{"type": "function_call", "id": "fc_" + tc.ID, "call_id": tc.ID, "name": tc.Name,
				"arguments": spacedJSON(tc.Arguments), "status": "completed"})
		}
	} else {
		output = append(output, obj{"type": "message", "id": "msg_" + hexID(16), "status": "completed", "role": "assistant",
			"content": []obj{{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	in, out := approxTokens(c.Text), approxTokens(text)
	writeJSON(w, 200, obj{
		"id": "resp_" + hexID(20), "object": "response", "created_at": time.Now().Unix(), "status": "completed",
		"model": model, "output": output, "parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"usage": obj{
			"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
			"input_tokens_details":  obj{"cached_tokens": 0},
			"output_tokens_details": obj{"reasoning_tokens": 0},
		},
	}, nil)
}
