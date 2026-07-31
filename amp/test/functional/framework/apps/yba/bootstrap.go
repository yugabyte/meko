package yba

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// First-customer bootstrap. A freshly deployed YBA has no customer, no session,
// and no API token; before the REST client (which assumes a static API token)
// can do anything, someone must register the first customer, log in, and mint a
// durable API token. This mirrors the stress framework's yugaware_app
// register/login/get_runtime_token sequence, narrowed to what the ampqa
// framework needs to stand up its OWN YBA (there is no standing dev YBA to
// attach to). None of these calls is customer-scoped except MintAPIToken, and
// only MintAPIToken authenticates — with the session token Login returns.

// DefaultBootstrapRetry is the poll cadence Bootstrap waits between attempts
// while the freshly installed YBA pod is still coming up.
const DefaultBootstrapRetry = 3 * time.Second

// ErrAlreadyRegistered is the sentinel Register returns when the YBA already has
// its first customer (a re-run against a surviving install). The caller falls
// through to Login rather than treating it as a failure.
var ErrAlreadyRegistered = errors.New("yba: first customer already registered")

// RegisterReq is the first-customer registration payload. Code is YBA's
// registration code ("dev" for the self-hosted dev install); it defaults to
// "dev" when empty.
type RegisterReq struct {
	Name     string
	Email    string
	Password string
	Code     string
}

// BootstrapCreds are the credentials Bootstrap registers and logs in with.
type BootstrapCreds struct {
	Name     string
	Email    string
	Password string
}

// BootstrapResult is what a successful Bootstrap yields: the durable API token
// (now also set on the client), the customer UUID, and the YBA version.
type BootstrapResult struct {
	APIToken     string
	CustomerUUID string
	Version      string
}

// AppVersion reads GET /api/v1/app_version — the unauthenticated readiness /
// version probe. It succeeds only once the platform process is actually serving,
// so Bootstrap uses it as the "is the pod up yet" gate.
func (c *Client) AppVersion(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.apiURL+"/app_version", nil, nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// Register creates the first customer. It POSTs the code/confirmEULA payload the
// self-hosted install expects and, on success, captures the session token and
// customer UUID it returns (so a caller could mint immediately without a
// separate Login). A response indicating the customer already exists is folded
// into ErrAlreadyRegistered so the caller can fall through to Login.
func (c *Client) Register(ctx context.Context, req RegisterReq) error {
	code := req.Code
	if code == "" {
		code = "dev"
	}
	body := map[string]any{
		"code":            code,
		"name":            req.Name,
		"email":           req.Email,
		"password":        req.Password,
		"confirmPassword": req.Password,
		"confirmEULA":     true,
	}
	var out struct {
		AuthToken    string `json:"authToken"`
		CustomerUUID string `json:"customerUUID"`
	}
	_, err := c.do(ctx, http.MethodPost, c.apiURL+"/register", nil, body, &out)
	if err != nil {
		if isAlreadyRegistered(err) {
			return ErrAlreadyRegistered
		}
		return fmt.Errorf("register first customer: %w", err)
	}
	if out.AuthToken != "" {
		c.authToken = out.AuthToken
	}
	if out.CustomerUUID != "" {
		c.customerUUID = out.CustomerUUID
	}
	return nil
}

// Login authenticates with email/password and returns the session token and
// customer UUID, also setting them on the client so a subsequent MintAPIToken
// (and any customer-scoped call) works.
func (c *Client) Login(ctx context.Context, email, password string) (customerUUID, authToken string, err error) {
	body := map[string]any{"email": email, "password": password}
	var out struct {
		AuthToken    string `json:"authToken"`
		CustomerUUID string `json:"customerUUID"`
	}
	if _, err := c.do(ctx, http.MethodPost, c.apiURL+"/login", nil, body, &out); err != nil {
		return "", "", fmt.Errorf("login %s: %w", email, err)
	}
	if out.AuthToken == "" || out.CustomerUUID == "" {
		return "", "", fmt.Errorf("login %s: missing authToken/customerUUID in response", email)
	}
	c.authToken = out.AuthToken
	c.customerUUID = out.CustomerUUID
	return out.CustomerUUID, out.AuthToken, nil
}

// MintAPIToken PUTs …/customers/{uuid}/api_token to mint a durable API token,
// authenticating with the session token from Login (X-AUTH-TOKEN). The minted
// token is set as the client's apiToken so every later customer-scoped call
// authenticates with it.
func (c *Client) MintAPIToken(ctx context.Context, customerUUID string) (string, error) {
	if c.authToken == "" {
		return "", fmt.Errorf("mint api token: no session token (call Login first)")
	}
	if customerUUID == "" {
		customerUUID = c.customerUUID
	}
	if customerUUID == "" {
		return "", fmt.Errorf("mint api token: no customer uuid")
	}
	var out struct {
		APIToken string `json:"apiToken"`
	}
	url := c.apiURL + "/customers/" + customerUUID + "/api_token"
	if _, err := c.do(ctx, http.MethodPut, url, map[string]string{sessionHeader: c.authToken}, nil, &out); err != nil {
		return "", fmt.Errorf("mint api token for customer %s: %w", customerUUID, err)
	}
	if out.APIToken == "" {
		return "", fmt.Errorf("mint api token for customer %s: empty apiToken in response", customerUUID)
	}
	c.apiToken = out.APIToken
	c.customerUUID = customerUUID
	return out.APIToken, nil
}

// Bootstrap drives register → login → mint against a freshly deployed YBA,
// retrying the whole sequence until the pod answers or timeout elapses. It is
// the one call ybastack (and any deploy-then-use path) needs to turn a bare
// install into a usable, token-authenticated client. On success the client is
// left authenticated with the minted API token and scoped to the new customer.
func (c *Client) Bootstrap(ctx context.Context, creds BootstrapCreds, timeout time.Duration) (BootstrapResult, error) {
	if creds.Email == "" || creds.Password == "" {
		return BootstrapResult{}, fmt.Errorf("bootstrap: email and password are required")
	}
	wctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	retry := c.BootstrapRetry
	if retry <= 0 {
		retry = DefaultBootstrapRetry
	}
	var last error
	for {
		res, err := c.tryBootstrap(wctx, creds)
		if err == nil {
			return res, nil
		}
		last = err
		select {
		case <-wctx.Done():
			return BootstrapResult{}, fmt.Errorf("bootstrap timed out: %w (last: %v)", wctx.Err(), last)
		case <-time.After(retry):
		}
	}
}

// tryBootstrap is one attempt of the register→login→mint sequence, gated on the
// version probe so an unreachable/not-yet-serving pod fails fast into a retry.
func (c *Client) tryBootstrap(ctx context.Context, creds BootstrapCreds) (BootstrapResult, error) {
	version, err := c.AppVersion(ctx)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("app_version probe: %w", err)
	}
	if err := c.Register(ctx, RegisterReq{Name: creds.Name, Email: creds.Email, Password: creds.Password}); err != nil && !errors.Is(err, ErrAlreadyRegistered) {
		return BootstrapResult{}, err
	}
	customerUUID, _, err := c.Login(ctx, creds.Email, creds.Password)
	if err != nil {
		return BootstrapResult{}, err
	}
	token, err := c.MintAPIToken(ctx, customerUUID)
	if err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{APIToken: token, CustomerUUID: customerUUID, Version: version}, nil
}

// isAlreadyRegistered reports whether an API error from /register means the
// first customer already exists (a re-run against a surviving install), so the
// caller falls through to Login rather than failing. Matches the phrasings YBA
// uses for a duplicate registration.
func isAlreadyRegistered(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	for _, marker := range []string{"already", "single customer", "multiple account", "cannot register"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
