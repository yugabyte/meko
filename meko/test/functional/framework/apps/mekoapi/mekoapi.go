// Package mekoapi is a small hand-written client for the api-server routes the
// tests call. The committed swagger.yaml is not its source (meko#1199: it is
// missing paths and documents removed parameters), so each route here was read
// off router.go and its handler.
//
// With no identity provider the api-server authenticates every request as one
// account, so a Client without a token works against a BYOC stack. Pass
// WithToken to call as a PAT holder.
package mekoapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

const (
	apiPrefix   = "/api/v1"
	maxBodyRead = 8 << 20
)

type Client struct {
	base    string
	token   string
	hc      *http.Client
	onError func(*Error)
	headers http.Header
}

type Option func(*Client)

func WithToken(token string) Option { return func(c *Client) { c.token = token } }

func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

func New(base string, opts ...Option) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) BaseURL() string { return c.base }

func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = token
	return &cp
}

// WithTimeout returns a copy whose requests may take up to d.
// WithHeader returns a copy that sends key: value on every request, for the
// operator routes that authenticate with a header instead of a token.
func (c *Client) WithHeader(key, value string) *Client {
	cp := *c
	cp.headers = c.headers.Clone()
	if cp.headers == nil {
		cp.headers = http.Header{}
	}
	cp.headers.Set(key, value)
	return &cp
}

func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	hc := *c.hc
	hc.Timeout = d
	cp.hc = &hc
	return &cp
}

// WithErrorHook returns a copy that hands every non-2xx answer to fn before
// returning it.
func (c *Client) WithErrorHook(fn func(*Error)) *Client {
	cp := *c
	cp.onError = fn
	return &cp
}

// Error is a non-2xx answer. Code and Message come from the {"error": ...}
// envelope when the body carries one.
type Error struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
	Body    string
}

func (e *Error) Error() string {
	detail := e.Message
	if detail == "" {
		detail = strings.TrimSpace(e.Body)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s %s: HTTP %d %s: %s", e.Method, e.Path, e.Status, e.Code, detail)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, detail)
}

func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: encode body: %w", method, path, err)
		}
		payload, contentType = b, "application/json"
	}
	return c.exchange(ctx, method, path, contentType, payload, out)
}

// exchange sends one request and decodes the {"data": ...} envelope into out,
// turning a non-2xx into an *Error.
func (c *Client) exchange(ctx context.Context, method, path, contentType string, payload []byte, out any) error {
	resp, err := c.send(ctx, method, path, contentType, payload)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}

	var env envelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &Error{Method: method, Path: path, Status: resp.StatusCode, Body: truncate(string(raw), 512)}
		if env.Error != nil {
			e.Code, e.Message = env.Error.Code, env.Error.Message
		}
		if resp.StatusCode >= 500 {
			core.NoteServerError(ctx, method, apiPrefix+path)
		}
		if c.onError != nil {
			c.onError(e)
		}
		return e
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("%s %s: decode data: %w", method, path, err)
	}
	return nil
}

// The api-server limits every client IP to 10 requests a second, and a run
// reaches it from one address. A 429 is the limiter, not the answer, so it is
// retried for up to half a minute before it is reported, since four tests and
// the guard share that one bucket. The payload is bytes so a retry can send it
// again.
const maxRateLimitWait = 30 * time.Second

func (c *Client) send(ctx context.Context, method, path, contentType string, payload []byte) (*http.Response, error) {
	deadline := time.Now().Add(maxRateLimitWait)
	for wait := 150 * time.Millisecond; ; wait = min(wait*2, 2*time.Second) {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		for k, v := range c.headers {
			req.Header[k] = v
		}
		resp, err := c.hc.Do(req)
		retry := err == nil && resp.StatusCode == http.StatusTooManyRequests ||
			err != nil && method == http.MethodGet && connectionDropped(err)
		if !retry || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// connectionDropped is a transport failure a GET can be repeated after: the
// pod behind a port-forward was replaced, or a connection was reset mid-flight.
func connectionDropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Health is GET /api/v1/health. The route answers 200 even when a database is
// unreachable, so Healthy reads the body rather than the status.
type Health struct {
	Status         string `json:"status"`
	Database       string `json:"database"`
	SystemDatabase string `json:"system_database"`
	DataDatabase   string `json:"data_database"`
}

// Healthy is true when the server and both databases report ok.
func (h Health) Healthy() bool {
	return h.Status == "ok" && h.Database == "ok" && h.SystemDatabase == "ok" && h.DataDatabase == "ok"
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/health", nil, &h)
	return h, err
}

type Datapack struct {
	ID          string    `json:"datapack_id"`
	Name        string    `json:"datapack_name"`
	AgentID     string    `json:"created_by_agent_id,omitempty"`
	Description string    `json:"description,omitempty"`
	Grant       string    `json:"grant,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateDatapackRequest is the POST /datapacks body. AgentID empty means the
// account's default agent.
type CreateDatapackRequest struct {
	Name        string `json:"name"`
	AgentID     string `json:"agent_id,omitempty"`
	Description string `json:"description,omitempty"`
}

type createDatapackResponse struct {
	DatapackID string `json:"datapack_id"`
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
}

func (c *Client) CreateDatapack(ctx context.Context, req CreateDatapackRequest) (Datapack, error) {
	var out createDatapackResponse
	if err := c.do(ctx, http.MethodPost, "/datapacks", req, &out); err != nil {
		return Datapack{}, err
	}
	return Datapack{ID: out.DatapackID, Name: out.Name, AgentID: out.AgentID, Description: req.Description}, nil
}

func (c *Client) ListDatapacks(ctx context.Context) ([]Datapack, error) {
	var out []Datapack
	err := c.do(ctx, http.MethodGet, "/datapacks", nil, &out)
	return out, err
}

func (c *Client) GetDatapack(ctx context.Context, id string) (Datapack, error) {
	var out Datapack
	err := c.do(ctx, http.MethodGet, "/datapacks/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) DeleteDatapack(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/datapacks/"+url.PathEscape(id), nil, nil)
}

type Agent struct {
	AgentID string          `json:"agent_id"`
	UserID  string          `json:"user_id,omitempty"`
	Details json.RawMessage `json:"agent_details,omitempty"`
}

func (c *Client) CreateAgent(ctx context.Context, agentID string, details json.RawMessage) (Agent, error) {
	body := struct {
		AgentID string          `json:"agent_id"`
		Details json.RawMessage `json:"agent_details,omitempty"`
	}{agentID, details}
	var out Agent
	err := c.do(ctx, http.MethodPost, "/agents", body, &out)
	return out, err
}

func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	err := c.do(ctx, http.MethodGet, "/agents", nil, &out)
	return out, err
}

func (c *Client) DeleteAgent(ctx context.Context, agentID string) error {
	return c.do(ctx, http.MethodDelete, "/agents/"+url.PathEscape(agentID), nil, nil)
}

// Token scopes the api-server accepts.
const (
	ScopeAll   = "*"
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// CreateTokenRequest is the POST /me/meko-tokens body. TTLDays 0 takes the
// server default (90), and Scopes empty means "*".
type CreateTokenRequest struct {
	Name    string   `json:"name"`
	TTLDays int      `json:"ttl_days,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

type TokenRecord struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// Token is a freshly minted PAT: the plaintext (returned once) and its record.
type Token struct {
	Value  string      `json:"token"`
	Record TokenRecord `json:"token_record"`
}

// CreateToken mints a personal access token for the caller.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (Token, error) {
	var out Token
	err := c.do(ctx, http.MethodPost, "/me/meko-tokens", req, &out)
	return out, err
}

func (c *Client) ListTokens(ctx context.Context) ([]TokenRecord, error) {
	var out []TokenRecord
	err := c.do(ctx, http.MethodGet, "/me/meko-tokens", nil, &out)
	return out, err
}

// RevokeToken revokes a token by id. Revoking twice is not an error.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/me/meko-tokens/"+url.PathEscape(id), nil, nil)
}

type KBUpload struct {
	DatapackID  string `json:"datapack_id"`
	DocumentKey string `json:"document_key"`
	Filename    string `json:"filename"`
	Status      string `json:"status"`
	StatusURL   string `json:"status_url"`
}

func (c *Client) UploadKBFile(ctx context.Context, datapackID, filename string, content []byte) (KBUpload, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return KBUpload{}, err
	}
	if _, err := part.Write(content); err != nil {
		return KBUpload{}, err
	}
	if err := mw.Close(); err != nil {
		return KBUpload{}, err
	}
	var out KBUpload
	err = c.exchange(ctx, http.MethodPost, "/datapacks/"+url.PathEscape(datapackID)+"/knowledge-bases/upload", mw.FormDataContentType(), buf.Bytes(), &out)
	return out, err
}

type KBDocument struct {
	DocumentName    string `json:"document_name"`
	DocumentStatus  string `json:"document_status"`
	ChunksProcessed *int   `json:"chunks_processed"`
}

func (c *Client) KBDocumentStatus(ctx context.Context, datapackID, documentKey string) (KBDocument, error) {
	var out KBDocument
	err := c.do(ctx, http.MethodGet, "/datapacks/"+url.PathEscape(datapackID)+"/knowledge-bases/document-status?document_key="+url.QueryEscape(documentKey), nil, &out)
	return out, err
}
