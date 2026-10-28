package cloudlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/version"
)

// CallTimeout bounds one call to Meko Cloud made for a route: a search of a large datapack takes
// seconds.
const CallTimeout = 30 * time.Second

// maxCallBytes bounds a reply from Meko Cloud to a call.
const maxCallBytes = 8 << 20

// IsLinked says whether Meko Cloud is linked.
func (s *Service) IsLinked() (bool, error) {
	st, err := s.Store.Load()
	return st.Linked, err
}

// linked is the saved link a call goes through; a CodeNotLinked CallError when there's none.
func (s *Service) linked() (Link, error) {
	st, err := s.Store.Load()
	if err != nil {
		return Link{}, err
	}
	if !st.Linked {
		return Link{}, &CallError{Status: http.StatusConflict, Code: CodeNotLinked,
			msg: "This Meko isn't linked to Meko Cloud. Link it in Settings › Meko Cloud, or with meko cloud_link."}
	}
	return st.Link, nil
}

// API calls the linked Meko Cloud's REST API with the key: method on path, which follows the API URL
// (such as /datapacks/<id>), with body as JSON when it isn't nil. It returns the reply's data. A
// failure is a *CallError: Meko Cloud's own 4xx keeps its status, code and message (a 401 is
// CodeKeyRefused, a 502); anything else is a 502.
func (s *Service) API(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	l, err := s.linked()
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(l.EffectiveAPIURL(), "/")
	if base == "" {
		return nil, &CallError{Code: CodeNoAPIURL, msg: "The Meko Cloud link has no API URL. " +
			"Set the web app or API URL in Settings › Meko Cloud, or meko cloud_link --api-url."}
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return nil, merr
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, r)
	if err != nil {
		return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud's API URL isn't one Meko can call."}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	unreachable := func(err error) error {
		return &CallError{Code: CodeUnreachable,
			msg: "Couldn't reach Meko Cloud: " + strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the link's API URL, checked when it was saved
	if err != nil {
		return nil, unreachable(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized {
		return nil, keyRefused()
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCallBytes))
	if err != nil {
		return nil, unreachable(err)
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	jsonErr := json.Unmarshal(b, &env)
	switch {
	case jsonErr == nil && env.Error != nil && res.StatusCode/100 == 4:
		return nil, &CallError{Status: res.StatusCode, Code: env.Error.Code, msg: shorten(env.Error.Message)}
	case jsonErr == nil && env.Error != nil:
		return nil, &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud: " + strings.TrimSuffix(shorten(env.Error.Message), ".") + "."}
	case res.StatusCode/100 != 2 || jsonErr != nil:
		return nil, &CallError{Code: CodeCallFailed, msg: "Meko Cloud answered " + res.Status + ", not like Meko's API."}
	}
	return env.Data, nil
}

func keyRefused() error {
	return &CallError{Code: CodeKeyRefused,
		msg: "Meko Cloud refused the key. Link it again in Settings › Meko Cloud, or with meko cloud_link."}
}

// Tool calls one of the linked Meko Cloud's MCP tools with the key, and returns its reply's text:
// an error a caller can act on ({"error", "detail"}) is a reply too, as on this Meko's /mcp. args
// must hold only arguments the tool takes, since Meko Cloud refuses others.
func (s *Service) Tool(ctx context.Context, name string, args map[string]any) (string, error) {
	l, err := s.linked()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	hc := &http.Client{Transport: bearer{key: l.Key, next: http.DefaultTransport}}
	c := mcp.NewClient(&mcp.Implementation{Name: "meko", Version: version.Version}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: l.MCPURL(), HTTPClient: hc, MaxRetries: -1,
		DisableStandaloneSSE: true}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "401") {
			return "", keyRefused()
		}
		return "", &CallError{Code: CodeUnreachable, msg: "Couldn't reach Meko Cloud's MCP server: " +
			strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud's " + name + " failed: " + strings.TrimSuffix(shortError(ctx, err).Error(), ".") + "."}
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if res.IsError {
		return "", &CallError{Code: CodeCallFailed, msg: "Meko Cloud's " + name + " failed: " +
			strings.TrimSuffix(shorten(text), ".") + "."}
	}
	return text, nil
}
