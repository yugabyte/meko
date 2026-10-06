package llmmock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	RouteEmbeddings      = "embeddings"
	RouteChat            = "chat"
	RouteChatCompletions = "chat_completions"
	RouteResponses       = "responses"
	RouteConverse        = "converse"
	RouteConverseStream  = "converse_stream"
	RouteInvokeModel     = "invoke_model"
	RouteOpenAI          = "openai"
	RouteBedrock         = "bedrock"
)

const (
	KindLatency   = "latency"
	KindStatus    = "status"
	KindTimeout   = "timeout"
	KindMalformed = "malformed"
	KindCutStream = "cut_stream"
)

const defaultTimeout = 30 * time.Second

// The vector is zero padded or cut to the width the caller asked for, so a two
// number vector fixes the angle between two texts whatever the model.
type Fixture struct {
	Text     string    `json:"text"`
	Vector   []float64 `json:"vector"`
	Contains bool      `json:"contains,omitempty"`
}

type ToolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// Match is a substring of the whole prompt and System of its system messages, which
// tells callers apart when both carry the same user text. A script with tool calls
// only answers a request that offers tools and does not end in a tool result.
type Script struct {
	ID        string     `json:"id,omitempty"`
	Route     string     `json:"route,omitempty"`
	Match     string     `json:"match,omitempty"`
	System    string     `json:"system,omitempty"`
	Reply     string     `json:"reply,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Times     int        `json:"times,omitempty"`
}

// Times 0 means every matching request. Latency delays and lets the request
// through, the other kinds end it.
type Fault struct {
	ID         string `json:"id,omitempty"`
	Route      string `json:"route,omitempty"`
	Match      string `json:"match,omitempty"`
	Kind       string `json:"kind"`
	Status     int    `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	DelayMS    int    `json:"delay_ms,omitempty"`
	HangMS     int    `json:"hang_ms,omitempty"`
	Times      int    `json:"times,omitempty"`
}

func Latency(d time.Duration) Fault {
	return Fault{Kind: KindLatency, DelayMS: int(d / time.Millisecond)}
}

func HTTPStatus(code int) Fault { return Fault{Kind: KindStatus, Status: code} }

func RateLimit(retryAfter time.Duration) Fault {
	return Fault{Kind: KindStatus, Status: http.StatusTooManyRequests, RetryAfter: int(retryAfter / time.Second)}
}

func Timeout(hang time.Duration) Fault {
	return Fault{Kind: KindTimeout, HangMS: int(hang / time.Millisecond)}
}

func Malformed() Fault { return Fault{Kind: KindMalformed} }

func CutStream() Fault { return Fault{Kind: KindCutStream} }

type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type Call struct {
	Seq        int       `json:"seq"`
	Time       float64   `json:"time"`
	Route      string    `json:"route"`
	Labels     []string  `json:"labels"`
	API        string    `json:"api"`
	Model      string    `json:"model"`
	Path       string    `json:"path"`
	Stream     bool      `json:"stream"`
	Status     int       `json:"status"`
	Fault      string    `json:"fault"`
	Answer     string    `json:"answer"`
	Dims       int       `json:"dims"`
	DurationMS int       `json:"duration_ms"`
	Text       string    `json:"text"`
	Inputs     []string  `json:"inputs"`
	Messages   []Message `json:"messages"`
	Tools      []string  `json:"tools"`
}

func (c Call) At() time.Time {
	return time.Unix(0, int64(c.Time*float64(time.Second)))
}

type Filter struct {
	Route    string
	Contains string
	After    int
}

type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: defaultTimeout}}
}

func (c *Client) URL() string { return c.base }

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_mock/health", nil, nil)
}

func (c *Client) SetEmbedding(ctx context.Context, fx ...Fixture) error {
	return c.do(ctx, http.MethodPost, "/_mock/embeddings", fx, nil)
}

func (c *Client) ClearEmbedding(ctx context.Context, text string) error {
	return c.do(ctx, http.MethodDelete, "/_mock/embeddings?text="+url.QueryEscape(text), nil, nil)
}

func (c *Client) AddScript(ctx context.Context, s Script) (string, error) {
	return c.add(ctx, "/_mock/scripts", s)
}

func (c *Client) RemoveScript(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/_mock/scripts?id="+url.QueryEscape(id), nil, nil)
}

func (c *Client) AddFault(ctx context.Context, f Fault) (string, error) {
	return c.add(ctx, "/_mock/faults", f)
}

func (c *Client) RemoveFault(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/_mock/faults?id="+url.QueryEscape(id), nil, nil)
}

func (c *Client) Calls(ctx context.Context, f Filter) ([]Call, error) {
	q := url.Values{}
	if f.Route != "" {
		q.Set("route", f.Route)
	}
	if f.Contains != "" {
		q.Set("contains", f.Contains)
	}
	if f.After > 0 {
		q.Set("after", fmt.Sprint(f.After))
	}
	var out struct {
		Calls []Call `json:"calls"`
	}
	if err := c.do(ctx, http.MethodGet, "/_mock/calls?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Calls, nil
}

// Stats is how many calls the mock has answered since its last reset, by route.
type Stats struct {
	Seq    int            `json:"seq"`
	Routes map[string]int `json:"routes"`
}

func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	err := c.do(ctx, http.MethodGet, "/_mock/stats", nil, &out)
	return out, err
}

// Reset drops every fixture, script, fault and recorded call. On a stack that
// tests share it also wipes what the other tests set, so a test removes its own
// rules instead.
func (c *Client) Reset(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/_mock/reset", struct{}{}, nil)
}

func (c *Client) add(ctx context.Context, path string, item any) (string, error) {
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := c.do(ctx, http.MethodPost, path, item, &out); err != nil {
		return "", err
	}
	if len(out.IDs) != 1 {
		return "", fmt.Errorf("llmmock %s: %d ids back, want 1", path, len(out.IDs))
	}
	return out.IDs[0], nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("llmmock %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("llmmock %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("llmmock %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}
