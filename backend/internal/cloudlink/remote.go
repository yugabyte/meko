package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/version"
)

// ForwardTimeout bounds one tool call this Meko's /mcp sends on to the linked Meko Cloud: some of its
// tools (a cluster's create) take a while.
const ForwardTimeout = 2 * time.Minute

// toolsTTL is how long the linked Meko Cloud's tool list is used before it's read again, and
// toolsRetry how long after a failed read the next one waits.
const (
	toolsTTL   = 15 * time.Minute
	toolsRetry = time.Minute
)

// toolsWait bounds how long a request waits for the first read of the tool list; one that waits
// longer gets the tools this Meko knows Meko Cloud has.
const toolsWait = 10 * time.Second

// Tools is what the linked Meko Cloud's MCP server offers: its tools, as its tools/list gives them,
// and the instructions its initialize gives.
type Tools struct {
	Tools        []*mcp.Tool
	Instructions string
}

// remote is the kept session with the linked Meko Cloud's MCP server and the tools it offers. A
// link change drops both (reset).
type remote struct {
	mu       sync.Mutex
	cs       *mcp.ClientSession
	url, key string

	toolsMu sync.Mutex
	gen     int // counts resets, so a read that started before one is dropped
	tools   *Tools
	readAt  time.Time
	tried   time.Time
	reading chan struct{} // closed when the read in progress ends
}

// reset drops the session and the tool list, after a link change.
func (r *remote) reset() {
	r.mu.Lock()
	cs := r.cs
	r.cs, r.url, r.key = nil, "", ""
	r.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
	r.toolsMu.Lock()
	r.gen++
	r.tools, r.readAt, r.tried = nil, time.Time{}, time.Time{}
	r.toolsMu.Unlock()
}

// session is the kept session for l, connecting when there's none (or it's for another link).
func (r *remote) session(ctx context.Context, l Link) (*mcp.ClientSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cs != nil && r.url == l.MCPURL() && r.key == l.Key {
		return r.cs, nil
	}
	if r.cs != nil {
		_ = r.cs.Close()
		r.cs = nil
	}
	cs, err := connectCloud(ctx, l)
	if err != nil {
		return nil, err
	}
	r.cs, r.url, r.key = cs, l.MCPURL(), l.Key
	return cs, nil
}

// drop forgets cs when it's still the kept session, so the next call connects again.
func (r *remote) drop(cs *mcp.ClientSession) {
	r.mu.Lock()
	if r.cs == cs {
		r.cs = nil
	}
	r.mu.Unlock()
	_ = cs.Close()
}

// connectCloud opens a session with l's MCP server, sending its key. The handshake is bounded by ctx;
// the session outlives it.
func connectCloud(ctx context.Context, l Link) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: bearer{key: l.Key, next: http.DefaultTransport}}
	c := mcp.NewClient(&mcp.Implementation{Name: "meko", Version: version.Version}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: l.MCPURL(), HTTPClient: hc, MaxRetries: -1,
		DisableStandaloneSSE: true}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "401") {
			return nil, keyRefused()
		}
		return nil, &CallError{Code: CodeUnreachable, msg: "Couldn't reach Meko Cloud's MCP server: " +
			strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	return cs, nil
}

// sessionGone says a call failed because the session was gone before the server ran it, so making
// it again on a new session can't run it twice.
func sessionGone(err error) bool {
	return errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)
}

// Forward calls one of the linked Meko Cloud's MCP tools with the arguments as they came (raw JSON,
// or nil for none) on the kept session, within ForwardTimeout, and returns its result as it is. A
// session the server no longer has is replaced, and the call made once more. A failure is a
// *CallError, a CodeNotLinked one while unlinked.
func (s *Service) Forward(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	l, err := s.linked()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ForwardTimeout)
	defer cancel()
	var a any = map[string]any{}
	if len(args) > 0 {
		a = args
	}
	for again := true; ; again = false {
		cs, err := s.remote.session(ctx, l)
		if err != nil {
			return nil, err
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: a})
		if err == nil {
			return res, nil
		}
		s.remote.drop(cs)
		if !again || !sessionGone(err) || ctx.Err() != nil {
			return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud's " + name + " failed: " +
				strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
		}
	}
}

// CloudTools is what the linked Meko Cloud offers, as last read; nil when it isn't linked, or its
// list couldn't be read yet. A list older than toolsTTL is read again in the background; before the
// first read the call waits for it up to toolsWait.
func (s *Service) CloudTools(ctx context.Context) *Tools {
	l, err := s.linked()
	if err != nil {
		return nil
	}
	r := &s.remote
	r.toolsMu.Lock()
	t, readAt, tried, reading := r.tools, r.readAt, r.tried, r.reading
	stale := time.Since(readAt) > toolsTTL && time.Since(tried) > toolsRetry
	if stale && reading == nil {
		reading = make(chan struct{})
		r.reading, r.tried = reading, time.Now()
		go s.readTools(l, r.gen, reading) //nolint:gosec // G118: the read outlives the request that starts it
	}
	r.toolsMu.Unlock()
	if t != nil || reading == nil {
		return t
	}
	wait, cancel := context.WithTimeout(ctx, toolsWait)
	defer cancel()
	select {
	case <-reading:
	case <-wait.Done():
	}
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	return r.tools
}

// readTools reads the tool list and the instructions on the kept session, every page, and keeps
// them unless the link changed meanwhile.
func (s *Service) readTools(l Link, gen int, done chan struct{}) {
	r := &s.remote
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	t, err := s.listTools(ctx, l)
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	if r.reading == done {
		r.reading = nil
	}
	close(done)
	if err == nil && r.gen == gen {
		r.tools, r.readAt = t, time.Now()
	}
}

func (s *Service) listTools(ctx context.Context, l Link) (*Tools, error) {
	cs, err := s.remote.session(ctx, l)
	if err != nil {
		return nil, err
	}
	t := &Tools{}
	if ir := cs.InitializeResult(); ir != nil {
		t.Instructions = ir.Instructions
	}
	cursor := ""
	for range 100 {
		res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			s.remote.drop(cs)
			return nil, err
		}
		t.Tools = append(t.Tools, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return t, nil
}
