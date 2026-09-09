// Package mekoclient is the reconcile agent's typed client for meko's
// api_server tenant REST API — the surface a datapack owner drives, reached
// with an MCP personal access token rather than a browser session.
//
// It is hand-written where internal/ampclient is generated, because the two
// specs are not the same kind of artifact. amp publishes an OpenAPI 3 document
// that oapi-codegen consumes directly; meko's docs/swagger.json is Swagger 2.0
// emitted by swaggo, which oapi-codegen cannot read, and it documents a fraction
// of the routes internal/api/router.go registers. Converting a partial 2.0 spec
// to generate a client for four calls costs more than writing the four.
//
// Every response is the api-server's {"data":…,"error":…} envelope. Decoding
// that wrapper straight into a payload struct yields zero values instead of an
// error, which is how the agent once enrolled with an empty environment id
// (meko#399) — so the envelope is decoded explicitly here, once, and a body
// carrying an error is returned as *Error no matter what the status line said.
package mekoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one meko api_server as one user. The token is an MCP PAT, and
// every route this client uses is on the PAT-accepting group (dpDirect /
// dpNamedDirect in internal/api/router.go), gated on the read and write scopes —
// including POST /agents, which was moved there for this client. The agent list and
// delete routes stay JWT-only and are not used here.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New builds a Client against the api-server at baseURL, which must include the
// API base path (the api-server mounts the tenant API at /api/v1, under an
// optional --base-path prefix). hc is optional.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// Datapack is one row of GET /datapacks. The api-server redacts the connection
// details (universe id, YSQL connection string, MCP username) out of every
// datapack it returns, so those fields are absent here rather than declared and
// always empty.
type Datapack struct {
	ID             string    `json:"datapack_id"`
	Name           string    `json:"datapack_name"`
	Description    string    `json:"description,omitempty"`
	UserID         string    `json:"user_id"`
	CreatedByAgent string    `json:"created_by_agent_id,omitempty"`
	Grant          string    `json:"grant,omitempty"`
	MemoryCount    int       `json:"memory_count"`
	KnowledgeCount int       `json:"knowledge_count"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateDatapackRequest is the body of POST /datapacks. user_id is not sent:
// the api-server overwrites it from the authenticated account, so a value here
// would be silently ignored rather than honoured.
//
// AgentID is stored as the datapack's created_by_agent_id and is what marks it
// as the reconciler's. The agent it names must already exist (CreateAgent).
type CreateDatapackRequest struct {
	Name        string `json:"name"`
	AgentID     string `json:"agent_id,omitempty"`
	Description string `json:"description,omitempty"`
}

// CreateAgentRequest is the body of POST /agents. The id is chosen by the
// caller, not assigned, which is what lets a reconciler declare one by name.
type CreateAgentRequest struct {
	AgentID string `json:"agent_id"`
}

// Agent is one row of GET /agents: a user-scoped name that datapacks are
// attributed to. It carries no credential — an agent is an attribution, not an
// access grant.
type Agent struct {
	AgentID   string    `json:"agent_id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateDatapackResponse is what POST /datapacks returns, minus the credential
// fields the handler redacts before it replies.
type CreateDatapackResponse struct {
	DatapackID string `json:"datapack_id"`
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
}

// UpdateDatapackRequest is the body of PATCH /datapacks/{id}. Every field is
// omitempty and the api-server treats an absent one as unchanged.
type UpdateDatapackRequest struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// Error is a non-2xx response, or a 2xx whose envelope carried an error. Code is
// the api-server's machine-readable code (VALIDATION_ERROR, CONFLICT,
// FREE_TIER_DATAPACK_LIMIT, …), which is what callers should branch on: the
// status alone does not distinguish a spec that can never be accepted from a
// limit that may lift.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("meko api: %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("meko api: %d: %s", e.StatusCode, e.Message)
}

// ListDatapacks returns every datapack the caller's account owns or has been
// granted, each carrying the agent id it was created by.
func (c *Client) ListDatapacks(ctx context.Context) ([]Datapack, error) {
	var out []Datapack
	if err := c.do(ctx, http.MethodGet, "/datapacks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateAgent registers a user-scoped agent under the given id. The api-server
// answers 409 when one already exists, which callers that only need it to be
// present treat as success.
func (c *Client) CreateAgent(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/agents", CreateAgentRequest{AgentID: id}, nil)
}

// ListAgents returns every agent the caller's account holds.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	if err := c.do(ctx, http.MethodGet, "/agents", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAgent removes one agent. Its datapacks are detached, not deleted — the
// row is an attribution, so withdrawing it costs the attribution and nothing else.
func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/agents/"+id, nil, nil)
}

// CreateDatapack creates one datapack. The api-server assigns its UUID.
func (c *Client) CreateDatapack(ctx context.Context, req CreateDatapackRequest) (*CreateDatapackResponse, error) {
	var out CreateDatapackResponse
	if err := c.do(ctx, http.MethodPost, "/datapacks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDatapack patches one datapack, addressed by its UUID — the ownership
// middleware rejects anything that does not parse as one, so a name will not do.
func (c *Client) UpdateDatapack(ctx context.Context, id string, req UpdateDatapackRequest) error {
	return c.do(ctx, http.MethodPatch, "/datapacks/"+id, req, nil)
}

// DeleteDatapack removes one datapack by UUID. A 404 is returned as *Error;
// callers that treat "already gone" as success check for it.
func (c *Client) DeleteDatapack(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/datapacks/"+id, nil, nil)
}

// envelope is the api-server's uniform response shape (handlers.APIResponse).
type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do issues one request and unwraps the envelope into out (nil to discard).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}

	var env envelope
	// A body that is not the envelope at all (a proxy's HTML error page, an
	// empty 204) must not read as success on a bad status, and must not read as
	// failure on a good one.
	decoded := json.Unmarshal(raw, &env) == nil
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		if decoded && env.Error != nil {
			e.Code, e.Message = env.Error.Code, env.Error.Message
		}
		return e
	}
	if decoded && env.Error != nil {
		return &Error{StatusCode: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out == nil {
		return nil
	}
	if !decoded {
		return fmt.Errorf("%s %s: response is not a JSON envelope: %s", method, path, truncate(string(raw)))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("%s %s: response carried no data", method, path)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// truncate keeps an unexpected body out of the logs at full length; enough to
// recognise an HTML error page, not enough to bury the tick that logged it.
func truncate(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
