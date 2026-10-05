package cognitomock

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

type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

// UserSpec creates or updates a user. Status defaults to CONFIRMED, so the user
// can log in at once. Other values are the Cognito ones, UNCONFIRMED and
// FORCE_CHANGE_PASSWORD.
type UserSpec struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Status    string `json:"status,omitempty"`
}

type User struct {
	Email  string `json:"email"`
	Sub    string `json:"sub"`
	Status string `json:"status"`
}

// MintSpec asks the mock to sign a token for a user. Zero values mean an access
// token for the pool, valid for the pool's TTL. A negative ExpiresIn mints an
// expired one, and Issuer names another pool.
type MintSpec struct {
	Email     string `json:"email,omitempty"`
	Sub       string `json:"sub,omitempty"`
	TokenUse  string `json:"token_use,omitempty"`
	ExpiresIn *int   `json:"expires_in,omitempty"`
	Issuer    string `json:"issuer,omitempty"`
}

func (c *Client) UpsertUser(ctx context.Context, u UserSpec) (User, error) {
	var out User
	err := c.do(ctx, http.MethodPost, "/control/users", u, &out)
	return out, err
}

func (c *Client) Mint(ctx context.Context, m MintSpec) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/control/tokens", m, &out)
	return out.Token, err
}

func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out struct {
		Users []User `json:"users"`
	}
	err := c.do(ctx, http.MethodGet, "/control/users", nil, &out)
	return out.Users, err
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
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
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cognito mock %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cognito mock %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
