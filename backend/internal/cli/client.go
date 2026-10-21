package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/server"
)

// client talks to a running server, finding it through the data folder's port and token files.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(l paths.Layout) (*client, error) {
	port, err := readInt(l.Port)
	if err != nil {
		return nil, fmt.Errorf("can't find the server's port: %w", err)
	}
	token, err := auth.ReadToken(l.Token)
	if err != nil {
		return nil, err
	}
	return &client{
		base:  fmt.Sprintf("http://127.0.0.1:%d", port),
		token: token,
		http:  &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (c *client) status(ctx context.Context) (server.StatusReply, error) {
	var out server.StatusReply
	err := c.get(ctx, "/api/v1/status", &out)
	return out, err
}

func (c *client) get(ctx context.Context, path string, data any) error {
	// G704: the URL is always 127.0.0.1 on the port serve wrote to the data folder.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil) //nolint:gosec
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	env := envelope.Response{Data: data}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("unexpected reply from %s (%s): %w", path, resp.Status, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	return nil
}
