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

// errLinkChanged is a call or read that started with a link a change has since replaced.
var errLinkChanged = &CallError{Code: CodeCallFailed,
	msg: "The Meko Cloud link changed while the call was being made. Make it again."}

// remote is the kept session with the linked Meko Cloud's MCP server and the tools it offers, for
// one generation of the link (Service.gen): a change resets both, and a session or a tool list for
// an older generation is never kept.
type remote struct {
	mu  sync.Mutex
	gen int
	cs  *mcp.ClientSession

	toolsMu sync.Mutex
	tools   *Tools
	readAt  time.Time
	tried   time.Time
	reading chan struct{} // closed when the read in progress ends
}

// reset drops the session and the tool list, for the link of generation gen, after a change, and
// returns the old session for the caller to close. A read still running for the old link no longer
// holds up the new link's. r.mu is never held over the network (session), so this never waits on one.
func (r *remote) reset(gen int) *mcp.ClientSession {
	r.mu.Lock()
	cs := r.cs
	r.cs, r.gen = nil, gen
	r.mu.Unlock()
	r.toolsMu.Lock()
	r.tools, r.readAt, r.tried, r.reading = nil, time.Time{}, time.Time{}, nil
	r.toolsMu.Unlock()
	return cs
}

// generation is the link generation the kept session is for.
func (r *remote) generation() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// session is the kept session for l, of generation gen, connecting when there's none; errLinkChanged
// when a change has replaced l since it was read. The handshake runs outside r.mu, so a slow Meko
// Cloud holds up neither a link change nor calls that already have the session; two calls that
// connect at once keep the first session made and close the other.
func (r *remote) session(ctx context.Context, l Link, gen int) (*mcp.ClientSession, error) {
	r.mu.Lock()
	switch {
	case gen != r.gen:
		r.mu.Unlock()
		return nil, errLinkChanged
	case r.cs != nil:
		cs := r.cs
		r.mu.Unlock()
		return cs, nil
	}
	r.mu.Unlock()
	cs, err := connectCloud(ctx, l)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	kept, stale := r.cs, gen != r.gen
	if !stale && kept == nil {
		r.cs = cs
	}
	r.mu.Unlock()
	switch {
	case stale:
		_ = cs.Close()
		return nil, errLinkChanged
	case kept != nil:
		_ = cs.Close()
		return kept, nil
	}
	return cs, nil
}

// drop forgets cs when it's still the kept session, so the next call connects again. Only for a
// session that's gone (sessionGone): closing it fails every call still on it.
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
		if refused(err) {
			return nil, keyRefused()
		}
		return nil, &CallError{Code: CodeUnreachable, msg: "Couldn't reach Meko Cloud's MCP server: " +
			strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	return cs, nil
}

// sessionGone says a call failed because its session is gone: Meko Cloud doesn't have it, or its
// connection is closed. Only then is the session dropped.
func sessionGone(err error) bool {
	return errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)
}

// refused says a call failed because Meko Cloud refused the key (an HTTP 401).
func refused(err error) bool {
	return strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "401")
}

// current is the saved link and its generation, read together; a CodeNotLinked CallError when
// there's none.
func (s *Service) current() (Link, int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return Link{}, 0, err
	}
	if !st.Linked {
		return Link{}, 0, notLinked()
	}
	return st.Link, s.gen, nil
}

// Forward calls one of the linked Meko Cloud's MCP tools with the arguments as they came (raw JSON,
// or nil for none) on the kept session, within ForwardTimeout, and returns its result as it is. A
// call Meko Cloud answered with a missing session never ran there, so it's made once more on a new
// session; any other failure is reported and never retried, since the call may have run. A failure
// is a *CallError, a CodeNotLinked one while unlinked.
func (s *Service) Forward(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	l, gen, err := s.current()
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
		cs, err := s.remote.session(ctx, l, gen)
		if err != nil {
			return nil, err
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: a})
		if err == nil {
			return res, nil
		}
		if refused(err) {
			// The key was revoked under a session already open: the next call connects again.
			s.remote.drop(cs)
			return nil, keyRefused()
		}
		if sessionGone(err) {
			s.remote.drop(cs)
		}
		if !again || !errors.Is(err, mcp.ErrSessionMissing) || ctx.Err() != nil {
			return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud's " + name + " failed: " +
				strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
		}
	}
}

// CloudTools is what the linked Meko Cloud offers, as last read; nil when it isn't linked, or its
// list couldn't be read yet. A list older than toolsTTL is read again in the background; before the
// first read the call waits for it up to toolsWait.
func (s *Service) CloudTools(ctx context.Context) *Tools {
	l, gen, err := s.current()
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
		go s.readTools(l, gen, reading) //nolint:gosec // G118: the read outlives the request that starts it
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

// readTools reads the tool list and the instructions for the link of generation gen, and keeps them
// unless the link changed meanwhile.
func (s *Service) readTools(l Link, gen int, done chan struct{}) {
	r := &s.remote
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	t, err := s.listTools(ctx, l, gen)
	// The generation is checked under toolsMu: a reset after it clears the tool list after this.
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	if r.reading == done {
		r.reading = nil
	}
	close(done)
	if err == nil && r.generation() == gen {
		r.tools, r.readAt = t, time.Now()
	}
}

func (s *Service) listTools(ctx context.Context, l Link, gen int) (*Tools, error) {
	cs, err := s.remote.session(ctx, l, gen)
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
			if sessionGone(err) {
				s.remote.drop(cs)
			}
			return nil, err
		}
		t.Tools = append(t.Tools, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return t, nil
}
