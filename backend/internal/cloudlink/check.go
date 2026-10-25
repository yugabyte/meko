package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/version"
)

// CheckTimeout bounds each check.
const CheckTimeout = 10 * time.Second

type bearer struct {
	key  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(r)
}

// CheckMCP connects to the MCP server at url with key: the initialize handshake is an authenticated
// round trip, which is what AgentK's MekoClient.check made before Meko owned the link. No ping
// follows, since a stateless server (Meko's own, on the MCP Go SDK) refuses a ping that carries no
// protocol version. The error says why it failed, never with the key.
func CheckMCP(ctx context.Context, url, key string) error {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	hc := &http.Client{Transport: bearer{key: key, next: http.DefaultTransport}}
	c := mcp.NewClient(&mcp.Implementation{Name: "meko", Version: version.Version}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc, MaxRetries: -1,
		DisableStandaloneSSE: true}, nil)
	if err != nil {
		return shortError(ctx, err)
	}
	_ = cs.Close() // ending the session isn't part of the check
	return nil
}

func shortError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("it timed out")
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return errors.New(msg)
}

// CheckAPI asks whether apiURL answers like Meko's REST API, without the key: its {data} or
// {error: {code, message}} envelope on a route every Meko has.
func CheckAPI(ctx context.Context, apiURL string) error {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	route := strings.TrimRight(apiURL, "/") + "/datapacks/incoming-shares"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL the owner typed, checked before it's saved
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("couldn't reach it (it timed out)")
		}
		return fmt.Errorf("couldn't reach it (%w)", err)
	}
	defer func() { _ = res.Body.Close() }()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if json.Unmarshal(body, &env) == nil && (env.Data != nil || (env.Error != nil && env.Error.Code != "")) {
		return nil
	}
	return fmt.Errorf("it answered %d, not like Meko's API", res.StatusCode)
}
