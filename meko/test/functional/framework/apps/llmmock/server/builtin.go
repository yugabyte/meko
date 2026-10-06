package server

import (
	"crypto/sha1"
	"encoding/binary"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	wordRe     = regexp.MustCompile(`[a-z0-9]+`)
	sentenceRe = regexp.MustCompile(`[^.!?\n]+[.!?]?`)
	fenceRe    = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
	chunkRe    = regexp.MustCompile(`\S+\s*`)
)

var models = []string{
	"gpt-4o-mini",
	"gpt-4.1-nano-2025-04-14",
	"text-embedding-3-small",
	"text-embedding-3-large",
	"text-embedding-ada-002",
}

func embeddingDim(model string, requested any) int {
	if truthy(requested) {
		if n := intOf(requested); n > 0 {
			return n
		}
	}
	switch {
	case strings.Contains(model, "titan-embed-text-v2"), strings.HasPrefix(model, "cohere.embed"):
		return 1024
	case strings.Contains(model, "text-embedding-3-large"):
		return 3072
	}
	return 1536
}

func hashVector(tokens []string, dim int) []float64 {
	vec := make([]float64, dim)
	for _, tok := range tokens {
		h := sha1.Sum([]byte(tok))
		vec[int(binary.BigEndian.Uint32(h[:4])%uint32(dim))]++
	}
	var sum float64
	for _, x := range vec {
		sum += x * x
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func fit(vector []float64, dim int) []float64 {
	out := make([]float64, dim)
	copy(out, vector)
	return out
}

func messageText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, part := range asList(content) {
		switch p := part.(type) {
		case string:
			parts = append(parts, p)
		case obj:
			if v, ok := p["text"]; ok {
				parts = append(parts, str(v))
			} else if tr, ok := p["toolResult"]; ok {
				parts = append(parts, messageText(asObj(tr)["content"]))
			} else if v, ok := p["json"]; ok {
				parts = append(parts, spacedJSON(v))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func sentences(text string) []string {
	out := []string{}
	for _, s := range sentenceRe.FindAllString(text, -1) {
		if s = strings.TrimSpace(s); utf8.RuneCountInString(s) > 2 {
			out = append(out, s)
		}
	}
	return out
}

func userText(body string) string {
	var lines []string
	for _, ln := range splitLines(body) {
		if strings.HasPrefix(strings.ToLower(ln), "user:") {
			_, after, _ := strings.Cut(ln, ":")
			lines = append(lines, strings.TrimSpace(after))
		}
	}
	if len(lines) == 0 {
		return body
	}
	return strings.Join(lines, " ")
}

func memories(body string) []obj {
	out := []obj{}
	for i, t := range sentences(userText(body)) {
		out = append(out, obj{"id": strconv.Itoa(i), "text": t})
	}
	return out
}

func jsonInFences(prompt string) obj {
	m := fenceRe.FindStringSubmatch(prompt)
	if m == nil {
		return nil
	}
	v, err := decode([]byte(m[1]))
	if err != nil {
		return nil
	}
	o, _ := v.(obj)
	return o
}

func builtinAnswer(messages []message, wantsJSON bool) (name, text string) {
	texts := make([]string, len(messages))
	var systems []string
	for i, m := range messages {
		texts[i] = m.Text
		if m.Role == "system" {
			systems = append(systems, m.Text)
		}
	}
	prompt := strings.Join(texts, "\n")
	system := strings.Join(systems, "\n")
	last := ""
	if len(messages) > 0 {
		last = messages[len(messages)-1].Text
		if messages[len(messages)-1].Role == "tool" {
			return "tool_result", "Done. The tool returned: " + head(last, 200)
		}
	}

	// The last heading: mem0 2.x documents its inputs under the same heading in the system prompt.
	if i := strings.LastIndex(prompt, "## New Messages\n"); i >= 0 {
		body, _, _ := strings.Cut(prompt[i+len("## New Messages\n"):], "\n\n## ")
		return "mem0_extract", spacedJSON(obj{"memory": memories(body)})
	}

	if strings.Contains(prompt, "smart memory manager") ||
		(strings.Contains(prompt, "Current memory") && strings.Contains(prompt, "retrieved facts")) {
		events := []obj{}
		for i, f := range asList(jsonInFences(prompt)["facts"]) {
			events = append(events, obj{"id": strconv.Itoa(i), "text": f, "event": "ADD"})
		}
		return "mem0_update", spacedJSON(obj{"memory": events})
	}

	if strings.Contains(system, "Personal Information Organizer") ||
		(strings.Contains(last, "Input:\n") && strings.Contains(strings.ToLower(prompt), "facts")) {
		body := last
		if _, after, ok := strings.Cut(last, "Input:\n"); ok {
			body = after
		}
		return "mem0_facts", spacedJSON(obj{"facts": sentences(userText(body))})
	}

	switch {
	case strings.Contains(system, "Classify their next message as EXACTLY one word"):
		return "gateway_followup", "other"
	case strings.Contains(system, "You route ONE user message"):
		return "gateway_route", "cps"
	case strings.Contains(system, "using ONLY the prior conversation"):
		return "gateway_summary", "Summary of prior conversation: " + tail(last, 300)
	case strings.HasPrefix(prompt, "Generate a summary of the following text:"):
		_, after, _ := strings.Cut(prompt, ":")
		return "api_summary", "Summary: " + head(strings.TrimSpace(after), 200)
	}

	if wantsJSON {
		return "json_default", "{}"
	}
	reply := head(last, 200)
	if reply == "" {
		reply = "ok"
	}
	return "default", "llmmock reply: " + reply
}

func normalizeOpenAIMessages(raw any) []message {
	out := []message{}
	for _, item := range asList(raw) {
		m := asObj(item)
		role := "user"
		if r, ok := m["role"]; ok {
			role = str(r)
		}
		text := messageText(m["content"])
		if truthy(m["tool_calls"]) && text == "" {
			text = spacedJSON(m["tool_calls"])
		}
		out = append(out, message{Role: role, Text: text})
	}
	return out
}

func normalizeResponsesInput(body obj) []message {
	out := []message{}
	if truthy(body["instructions"]) {
		out = append(out, message{Role: "system", Text: str(body["instructions"])})
	}
	if s, ok := body["input"].(string); ok {
		return append(out, message{Role: "user", Text: s})
	}
	for _, item := range asList(body["input"]) {
		m, ok := item.(obj)
		if !ok {
			continue
		}
		if _, has := m["role"]; has {
			out = append(out, message{Role: str(m["role"]), Text: messageText(m["content"])})
		} else if m["type"] == "function_call_output" {
			out = append(out, message{Role: "tool", Text: str(m["output"])})
		}
	}
	return out
}

func normalizeConverse(body obj) []message {
	out := []message{}
	if truthy(body["system"]) {
		out = append(out, message{Role: "system", Text: messageText(body["system"])})
	}
	for _, item := range asList(body["messages"]) {
		m := asObj(item)
		blocks := asList(m["content"])
		role := "user"
		if r, ok := m["role"]; ok {
			role = str(r)
		}
		for _, b := range blocks {
			if bo, ok := b.(obj); ok {
				if _, has := bo["toolResult"]; has {
					role = "tool"
					break
				}
			}
		}
		out = append(out, message{Role: role, Text: messageText(blocks)})
	}
	return out
}

func toolNames(body obj, style string) []string {
	var names []string
	switch style {
	case "openai":
		for _, t := range asList(body["tools"]) {
			to := asObj(t)
			if fn, ok := to["function"]; truthy(fn) && ok {
				to = asObj(fn)
			}
			names = append(names, str(to["name"]))
		}
	case "converse":
		for _, t := range asList(asObj(body["toolConfig"])["tools"]) {
			names = append(names, str(asObj(asObj(t)["toolSpec"])["name"]))
		}
	}
	out := names[:0]
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

type plannedCall struct {
	ID        string
	Name      string
	Arguments any
}

func (s *Server) chatPlan(labels []string, messages []message, tools []string, wantsJSON bool) (answer, text string, calls []plannedCall) {
	texts := make([]string, len(messages))
	var systems []string
	for i, m := range messages {
		texts[i] = m.Text
		if m.Role == "system" {
			systems = append(systems, m.Text)
		}
	}
	lastTool := len(messages) > 0 && messages[len(messages)-1].Role == "tool"
	sc := s.st.takeScript(labels, strings.Join(texts, "\n"), strings.Join(systems, "\n"), len(tools) > 0, lastTool)
	if sc != nil {
		for _, c := range sc.ToolCalls {
			args := c.Arguments
			if !truthy(args) {
				args = obj{}
			}
			calls = append(calls, plannedCall{ID: "call_" + hexID(12), Name: c.Name, Arguments: args})
		}
		return "script:" + sc.ID, sc.Reply, calls
	}
	name, text := builtinAnswer(messages, wantsJSON)
	return "builtin:" + name, text, nil
}

func chunks(text string, n int) []string {
	words := chunkRe.FindAllString(text, -1)
	var out []string
	for i := 0; i < len(words); i += n {
		out = append(out, strings.Join(words[i:min(i+n, len(words))], ""))
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
