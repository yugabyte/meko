package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
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
	return c.do(ctx, http.MethodGet, path, data)
}

// reloadWeb has the server read the web UI's settings again (meko ui start and stop).
func (c *client) reloadWeb(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/web/reload", nil)
}

func (c *client) do(ctx context.Context, method, path string, data any) error {
	// G704: the URL is always 127.0.0.1 on the port serve wrote to the data folder.
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil) //nolint:gosec
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	// A refused connection is tried again for up to a second: the server takes its port again when
	// remote access turns on or off (the handoff, meko ui start and stop), and nothing listens on it
	// for a moment.
	resp, err := c.http.Do(req) //nolint:gosec // G704: see above
	for deadline := time.Now().Add(time.Second); errors.Is(err, syscall.ECONNREFUSED) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(50 * time.Millisecond):
		}
		resp, err = c.http.Do(req) //nolint:gosec // G704: see above
	}
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
