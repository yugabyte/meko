package server

import (
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"strings"
	"time"
)

var (
	bedrockEmbedLabels = []string{"*", "bedrock", "embeddings", "invoke_model"}
	bedrockInvokeChat  = []string{"*", "bedrock", "chat", "invoke_model"}
)

func eventFrame(eventType string, payload any) []byte {
	header := func(name, value string) []byte {
		out := []byte{byte(len(name))}
		out = append(out, name...)
		out = append(out, 7)
		out = binary.BigEndian.AppendUint16(out, uint16(len(value)))
		return append(out, value...)
	}
	var headers []byte
	headers = append(headers, header(":event-type", eventType)...)
	headers = append(headers, header(":content-type", "application/json")...)
	headers = append(headers, header(":message-type", "event")...)
	body := []byte(compact(payload))
	total := 12 + len(headers) + len(body) + 4
	msg := binary.BigEndian.AppendUint32(nil, uint32(total))
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(headers)))
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	msg = append(msg, headers...)
	msg = append(msg, body...)
	return binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
}

func (s *Server) bedrock(w http.ResponseWriter, r *http.Request, model, op string, body obj) {
	if op == "invoke" || op == "invoke-with-response-stream" {
		s.bedrockInvoke(w, r, model, body)
		return
	}
	messages := normalizeConverse(body)
	tools := toolNames(body, "converse")
	stream := op == "converse-stream"
	route := "converse"
	if stream {
		route = "converse_stream"
	}
	c := s.st.record(call{Route: route, API: "bedrock", Model: model, Path: r.URL.RequestURI(),
		Messages: messages, Tools: tools, Stream: stream, Text: joinTexts(messages)})
	t0 := time.Now()
	labels := []string{"*", "bedrock", "chat", route}
	if s.faulted(w, r, c, labels, c.Text, "bedrock") {
		return
	}
	answer, text, calls := s.chatPlan(labels, messages, tools, false)
	s.st.update(c, func(c *call) {
		c.Status, c.Answer, c.DurationMS = 200, answer, int(time.Since(t0).Milliseconds())
	})
	in, out := approxTokens(c.Text), approxTokens(text)
	usage := obj{"inputTokens": in, "outputTokens": out, "totalTokens": in + out}
	stop := "end_turn"
	if len(calls) > 0 {
		stop = "tool_use"
	}
	if !stream {
		blocks := []obj{{"text": text}}
		if len(calls) > 0 {
			blocks = nil
			for _, tc := range calls {
				blocks = append(blocks, obj{"toolUse": obj{"toolUseId": tc.ID, "name": tc.Name, "input": tc.Arguments}})
			}
		}
		writeJSON(w, 200, obj{
			"output":     obj{"message": obj{"role": "assistant", "content": blocks}},
			"stopReason": stop, "usage": usage, "metrics": obj{"latencyMs": 1},
		}, nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.amazon.eventstream")
	h.Set("Connection", "close")
	w.WriteHeader(200)
	type ev struct {
		name    string
		payload any
	}
	events := []ev{{"messageStart", obj{"role": "assistant"}}}
	if len(calls) > 0 {
		for i, tc := range calls {
			events = append(events,
				ev{"contentBlockStart", obj{"contentBlockIndex": i, "start": obj{"toolUse": obj{"toolUseId": tc.ID, "name": tc.Name}}}},
				ev{"contentBlockDelta", obj{"contentBlockIndex": i, "delta": obj{"toolUse": obj{"input": spacedJSON(tc.Arguments)}}}},
				ev{"contentBlockStop", obj{"contentBlockIndex": i}})
		}
	} else {
		for _, piece := range chunks(text, 3) {
			events = append(events, ev{"contentBlockDelta", obj{"contentBlockIndex": 0, "delta": obj{"text": piece}}})
		}
		events = append(events, ev{"contentBlockStop", obj{"contentBlockIndex": 0}})
	}
	events = append(events,
		ev{"messageStop", obj{"stopReason": stop}},
		ev{"metadata", obj{"usage": usage, "metrics": obj{"latencyMs": 1}}})
	for _, e := range events {
		_, _ = w.Write(eventFrame(e.name, e.payload))
		flush(w)
	}
}

func (s *Server) bedrockInvoke(w http.ResponseWriter, r *http.Request, model string, body obj) {
	isEmbed := strings.Contains(model, "embed")
	var texts []string
	for _, t := range asList(body["texts"]) {
		texts = append(texts, str(t))
	}
	text := str(body["inputText"])
	if text == "" {
		text = strings.Join(texts, "\n")
	}
	if text == "" {
		text = head(spacedJSON(body), 2000)
	}
	var messages []message
	if !isEmbed {
		if _, ok := body["messages"]; ok {
			for _, item := range asList(body["messages"]) {
				m := asObj(item)
				role := "user"
				if v, ok := m["role"]; ok {
					role = str(v)
				}
				messages = append(messages, message{Role: role, Text: messageText(m["content"])})
			}
		} else {
			t := str(body["prompt"])
			if t == "" {
				t = str(body["inputText"])
			}
			if t == "" {
				t = text
			}
			messages = []message{{Role: "user", Text: t}}
		}
		text = joinTexts(messages)
	}
	rec := call{Route: "invoke_model", API: "bedrock", Model: model, Path: r.URL.RequestURI(), Messages: messages, Text: text}
	if isEmbed {
		rec.Inputs = []string{text}
	}
	c := s.st.record(rec)
	t0 := time.Now()
	labels := bedrockInvokeChat
	if isEmbed {
		labels = bedrockEmbedLabels
	}
	if s.faulted(w, r, c, labels, text, "bedrock") {
		return
	}
	if isEmbed {
		dimArg := body["dimensions"]
		if !truthy(dimArg) {
			dimArg = body["output_dimension"]
		}
		dim := embeddingDim(model, dimArg)
		inputs := texts
		if len(inputs) == 0 {
			inputs = []string{text}
		}
		vecs := make([]vector, len(inputs))
		for i, t := range inputs {
			vecs[i], _ = s.embed(t, t, dim)
		}
		s.st.update(c, func(c *call) {
			c.Status, c.Answer, c.Dims, c.DurationMS = 200, "embedding", dim, int(time.Since(t0).Milliseconds())
		})
		if _, ok := body["texts"]; ok {
			writeJSON(w, 200, obj{"id": hexID(32), "embeddings": vecs, "texts": inputs, "response_type": "embeddings_floats"}, nil)
			return
		}
		writeJSON(w, 200, obj{"embedding": vecs[0], "inputTextTokenCount": approxTokens(text)}, nil)
		return
	}
	answer, reply, _ := s.chatPlan(labels, messages, nil, false)
	s.st.update(c, func(c *call) {
		c.Status, c.Answer, c.DurationMS = 200, answer, int(time.Since(t0).Milliseconds())
	})
	in, out := approxTokens(text), approxTokens(reply)
	switch {
	case strings.Contains(model, "anthropic."):
		writeJSON(w, 200, obj{
			"id": "msg_" + hexID(16), "type": "message", "role": "assistant", "model": model,
			"content":     []obj{{"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage":       obj{"input_tokens": in, "output_tokens": out},
		}, nil)
	case strings.Contains(model, "titan-text"):
		writeJSON(w, 200, obj{
			"inputTextTokenCount": in,
			"results":             []obj{{"tokenCount": out, "outputText": reply, "completionReason": "FINISH"}},
		}, nil)
	default:
		writeJSON(w, 200, obj{
			"output":     obj{"message": obj{"role": "assistant", "content": []obj{{"text": reply}}}},
			"stopReason": "end_turn",
			"usage":      obj{"inputTokens": in, "outputTokens": out, "totalTokens": in + out},
		}, nil)
	}
}
