// Package mcpclient is a minimal MCP client over the streamable HTTP transport
// (JSON-RPC 2.0 on POST). It covers what the tests need: initialize, tools/list
// and tools/call with a bearer token, and it reads a reply either as plain JSON
// or as a text/event-stream, since the server picks per request.
package mcpclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultProtocolVersion is offered in initialize. The server answers with
	// the version it speaks, and that one is sent on every later request.
	DefaultProtocolVersion = "2025-06-18"

	sessionHeader  = "Mcp-Session-Id"
	protocolHeader = "MCP-Protocol-Version"
	maxReply       = 16 << 20
)

type Client struct {
	url      string
	token    string
	hc       *http.Client
	name     string
	mu       sync.Mutex
	session  string
	protocol string
	next     int64
	onResult func(tool, text string)
}

type Option func(*Client)

func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

func WithClientName(name string) Option { return func(c *Client) { c.name = name } }

// WithResultHook hands fn the text of every tool call's answer, a tool-level
// error's included, and the message of a call that failed outright.
func WithResultHook(fn func(tool, text string)) Option { return func(c *Client) { c.onResult = fn } }

func New(url, token string, opts ...Option) *Client {
	c := &Client{url: url, token: token, hc: &http.Client{Timeout: 60 * time.Second}, name: "mekoqa"}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("mcp: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

type InitResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

func (r CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" || c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Initialize opens the session: initialize, then notifications/initialized.
// When the server answers 404 or 405 on the configured URL it retries once with
// the trailing slash toggled, since a mounted app often serves only one form.
func (c *Client) Initialize(ctx context.Context) (InitResult, error) {
	params := map[string]any{
		"protocolVersion": DefaultProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": c.name, "version": "1"},
	}
	var out InitResult
	err := c.call(ctx, "initialize", params, &out)
	// A port-forward to the Service drops a connection now and then. initialize
	// changes nothing on the server, so it is safe to send again.
	for attempt := 1; attempt < initAttempts && transportError(err); attempt++ {
		select {
		case <-ctx.Done():
			return InitResult{}, fmt.Errorf("initialize: %w", err)
		case <-time.After(time.Duration(attempt) * time.Second):
		}
		err = c.call(ctx, "initialize", params, &out)
	}
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
		c.mu.Lock()
		orig := c.url
		if strings.HasSuffix(c.url, "/") {
			c.url = strings.TrimRight(c.url, "/")
		} else {
			c.url += "/"
		}
		c.mu.Unlock()
		if err2 := c.call(ctx, "initialize", params, &out); err2 != nil {
			c.mu.Lock()
			c.url = orig
			c.mu.Unlock()
			return InitResult{}, fmt.Errorf("initialize on %s: %v; on the alternate form: %w", orig, err, err2)
		}
		err = nil
	}
	if err != nil {
		return InitResult{}, fmt.Errorf("initialize: %w", err)
	}
	if out.ProtocolVersion == "" {
		return InitResult{}, errors.New("initialize: reply carries no protocolVersion")
	}
	c.mu.Lock()
	c.protocol = out.ProtocolVersion
	c.mu.Unlock()
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return InitResult{}, fmt.Errorf("notifications/initialized: %w", err)
	}
	return out, nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var out struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &out); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
	return nil, errors.New("tools/list: more than 50 pages")
}

// CallTool invokes a tool. A tool-level failure is a nil error with
// CallResult.IsError set, and a protocol or auth failure is an error.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var out CallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		if c.onResult != nil {
			c.onResult(name, err.Error())
		}
		return CallResult{}, fmt.Errorf("tools/call %s: %w", name, err)
	}
	if c.onResult != nil {
		c.onResult(name, out.Text())
	}
	return out, nil
}

func (c *Client) Close(ctx context.Context) {
	c.mu.Lock()
	sid, url := c.session, c.url
	c.mu.Unlock()
	if sid == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	c.decorate(req)
	if resp, err := c.hc.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func (c *Client) decorate(req *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set(sessionHeader, c.session)
	}
	if c.protocol != "" {
		req.Header.Set(protocolHeader, c.protocol)
	}
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	msg, err := c.post(ctx, body, id)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return fmt.Errorf("decode reply %q: %w", truncate(string(msg), 256), err)
	}
	if env.Error != nil {
		return env.Error
	}
	if out == nil {
		return nil
	}
	if len(env.Result) == 0 {
		return errors.New("reply carries neither result nor error")
	}
	return json.Unmarshal(env.Result, out)
}

func (c *Client) notify(ctx context.Context, method string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	_, err = c.post(ctx, body, 0)
	return err
}

// post sends one JSON-RPC message. With id > 0 it returns the reply message for
// that id, and with id 0 (a notification) it returns nil once the server accepts.
func (c *Client) post(ctx context.Context, body []byte, id int64) (json.RawMessage, error) {
	c.mu.Lock()
	url := c.url
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.decorate(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	if id == 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReply))
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, id)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	return pickReply(b, id)
}

// pickReply returns the message answering id from a JSON body, which is one
// object or a batch array.
func pickReply(b []byte, id int64) (json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, errors.New("empty reply")
	}
	if b[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(b, &batch); err != nil {
			return nil, fmt.Errorf("decode batch reply: %w", err)
		}
		for _, m := range batch {
			if answers(m, id) {
				return m, nil
			}
		}
		return nil, fmt.Errorf("no message answers request %d", id)
	}
	if !answers(b, id) {
		return nil, fmt.Errorf("reply %q does not answer request %d", truncate(string(b), 256), id)
	}
	return b, nil
}

// readSSE reads events until one answers id. Notifications and requests the
// server interleaves are skipped.
func readSSE(r io.Reader, id int64) (json.RawMessage, error) {
	r = io.LimitReader(r, maxReply)
	var data []string
	flush := func() json.RawMessage {
		if len(data) == 0 {
			return nil
		}
		msg := json.RawMessage(strings.Join(data, "\n"))
		data = data[:0]
		if answers(msg, id) {
			return msg
		}
		return nil
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimRight(string(buf[:i]), "\r")
			buf = buf[i+1:]
			switch {
			case line == "":
				if m := flush(); m != nil {
					return m, nil
				}
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(buf) > 0 && strings.HasPrefix(string(buf), "data:") {
					data = append(data, strings.TrimSpace(strings.TrimPrefix(string(buf), "data:")))
				}
				if m := flush(); m != nil {
					return m, nil
				}
				return nil, fmt.Errorf("event stream ended without a reply to request %d", id)
			}
			return nil, fmt.Errorf("read event stream: %w", err)
		}
	}
}

func answers(msg []byte, id int64) bool {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(msg, &probe) != nil || len(probe.ID) == 0 {
		return false
	}
	if len(probe.Result) == 0 && len(probe.Error) == 0 {
		return false
	}
	got := strings.Trim(strings.TrimSpace(string(probe.ID)), `"`)
	return got == fmt.Sprint(id)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsAuthRejection reports whether err is the server refusing the credential: a
// 401 or 403 on the transport, or a JSON-RPC error whose message says so.
func IsAuthRejection(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden
	}
	var re *RPCError
	if errors.As(err, &re) {
		return looksLikeAuth(re.Message)
	}
	return false
}

func LooksLikeAuthFailure(text string) bool { return looksLikeAuth(text) }

var authText = regexp.MustCompile(`(?i)unauthor|forbidden|invalid token|invalid or expired|revoked|authenticat|token[^.]{0,20}expired|expired[^.]{0,20}token|\b(status|code|http)[ :=]*(401|403)\b|^(401|403)\b`)

func looksLikeAuth(s string) bool { return authText.MatchString(s) }

// ArgsFromSchema builds a value for every required property of a tool's JSON
// input schema, so a contract test can call a tool it knows nothing else about.
// Optional properties are left out.
func ArgsFromSchema(schema json.RawMessage) map[string]any {
	var s struct {
		Properties map[string]struct {
			Type    any             `json:"type"`
			Format  string          `json:"format"`
			Enum    []any           `json:"enum"`
			Default json.RawMessage `json:"default"`
			Minimum *float64        `json:"minimum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	out := map[string]any{}
	if json.Unmarshal(schema, &s) != nil {
		return out
	}
	for _, name := range s.Required {
		p := s.Properties[name]
		switch {
		case len(p.Default) > 0 && string(p.Default) != "null":
			var v any
			if json.Unmarshal(p.Default, &v) == nil {
				out[name] = v
				continue
			}
		case len(p.Enum) > 0:
			out[name] = p.Enum[0]
			continue
		}
		switch schemaType(p.Type) {
		case "integer", "number":
			v := 1.0
			if p.Minimum != nil && *p.Minimum > v {
				v = *p.Minimum
			}
			out[name] = v
		case "boolean":
			out[name] = false
		case "array":
			out[name] = []any{}
		case "object":
			out[name] = map[string]any{}
		default:
			if p.Format == "uuid" || strings.HasSuffix(name, "_id") {
				out[name] = randomUUID()
			} else {
				out[name] = "mekoqa"
			}
		}
	}
	return out
}

func schemaType(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

const initAttempts = 4

// transportError is a failure to exchange the request at all, as opposed to an
// answer the server gave.
func transportError(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	var re *RPCError
	if errors.As(err, &he) || errors.As(err, &re) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "EOF")
}
