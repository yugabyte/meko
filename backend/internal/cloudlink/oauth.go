package cloudlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/version"
)

// Signing in to Meko Cloud from the browser (docs/cloud.md, "Signing in"): the MCP authorization
// spec against the link's MCP URL, then POST <server>/api-keys with the access token, which makes an
// API key for the signed-in user. It's the flow the @yugabytedb/meko-mcp installer's "Sign in with
// browser" uses. Only the key leaves here: the access token is dropped once the key is made, since
// the link authenticates with the key alone.

// SignInRequestTimeout bounds each request of a sign-in.
const SignInRequestTimeout = 30 * time.Second

const mcpProtocolVersion = "2025-03-26"

// oauthServer is what discovery found: the authorization server's endpoints, the scope to ask for
// and the resource the token is for.
type oauthServer struct {
	authorize, token, register string
	scope, resource            string
}

// OAuthClient is a client registered with the authorization server (dynamic client registration).
type OAuthClient struct {
	ID         string `json:"client_id"`
	Secret     string `json:"client_secret"`
	AuthMethod string `json:"token_endpoint_auth_method"`
}

// Flow is one sign-in: the server discovered, a client registered for redirectURI, and this
// sign-in's state and PKCE verifier. AuthorizeURL is where the browser goes to sign in; Meko Cloud
// sends it back to RedirectURI with a code, which Finish trades for an API key.
type Flow struct {
	MCPURL       string
	RedirectURI  string
	AuthorizeURL string
	server       oauthServer
	client       OAuthClient
	state        string
	verifier     string
	hc           *http.Client
}

// Minted is the API key a sign-in made, with the name it has in the account.
type Minted struct {
	Key  string
	Name string
}

// StartFlow discovers how the MCP server at mcpURL signs people in, registers a client for
// redirectURI (or uses client, registered before for the same server and redirect URI), and makes
// the URL the browser opens. hc nil is a plain client.
func StartFlow(ctx context.Context, hc *http.Client, mcpURL, redirectURI string, client *OAuthClient) (*Flow, error) {
	if hc == nil {
		hc = &http.Client{}
	}
	if err := secureURL(mcpURL); err != nil {
		return nil, err
	}
	srv, err := discover(ctx, hc, mcpURL)
	if err != nil {
		return nil, err
	}
	f := &Flow{MCPURL: mcpURL, RedirectURI: redirectURI, server: srv, hc: hc,
		state: randomString(32), verifier: randomString(48)}
	if client != nil {
		f.client = *client
	} else if f.client, err = register(ctx, hc, srv, redirectURI); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(f.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {f.client.ID}, "redirect_uri": {redirectURI},
		"state": {f.state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}}
	if srv.scope != "" {
		q.Set("scope", srv.scope)
	}
	if srv.resource != "" {
		q.Set("resource", srv.resource)
	}
	u, _ := url.Parse(srv.authorize) // checked by discover
	u.RawQuery = q.Encode()
	f.AuthorizeURL = u.String()
	return f, nil
}

// Client is the client the flow registered, to reuse for another sign-in with the same server and
// redirect URI.
func (f *Flow) Client() OAuthClient { return f.client }

// Finish takes what Meko Cloud sent the browser back with: it checks state, trades code for an
// access token and the token for an API key named keyName.
func (f *Flow) Finish(ctx context.Context, code, state, keyName string) (Minted, error) {
	if subtle.ConstantTimeCompare([]byte(state), []byte(f.state)) != 1 {
		return Minted{}, refuse("That sign-in doesn't match the one Meko started. Sign in again.")
	}
	if strings.TrimSpace(code) == "" {
		return Minted{}, refuse("Meko Cloud sent no sign-in code back. Sign in again.")
	}
	token, err := f.exchange(ctx, code)
	if err != nil {
		return Minted{}, err
	}
	return createAPIKey(ctx, f.hc, f.MCPURL, token, keyName)
}

// Callback reads the address Meko Cloud sent the browser back to: its code and state, or why the
// sign-in was refused.
func Callback(address string) (code, state string, err error) {
	u, perr := url.Parse(strings.TrimSpace(address))
	if perr != nil || u.RawQuery == "" {
		return "", "", refuse("That isn't the address Meko Cloud sent your browser to. Paste the whole address.")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", "", Refused(q.Get("error_description"), e)
	}
	if q.Get("code") == "" || q.Get("state") == "" {
		return "", "", refuse("That address has no sign-in code. Paste the whole address your browser ended on.")
	}
	return q.Get("code"), q.Get("state"), nil
}

// Refused is the error for a sign-in Meko Cloud refused or the person called off, from the error and
// error_description it sent the browser back with.
func Refused(description, code string) error {
	why := strings.TrimSpace(description)
	if why == "" {
		why = strings.TrimSpace(code)
	}
	return refuse("Meko Cloud didn't sign you in: %s. Nothing was saved.", clip(strings.TrimSuffix(why, ".")))
}

// KeyName is the name a sign-in gives its key in the account: this computer and when, at most 100
// characters, so two sign-ins never share a name.
func KeyName(now time.Time) string {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	if host == "" {
		host = "this computer"
	}
	stamp := now.UTC().Format("2006-01-02 15:04:05Z")
	room := 100 - len("Meko on  ") - len(stamp)
	if len(host) > room {
		host = host[:room]
	}
	return "Meko on " + host + " " + stamp
}

// KeyLeft adds to err that the key a sign-in made is still in the account: Meko Cloud has no route
// to delete one, so the person revokes it.
func KeyLeft(err error, name string) error {
	msg := strings.TrimSpace(err.Error())
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return refuse("%s The key Meko made, %q, is still in your Meko Cloud account: revoke it in Meko Cloud's Settings.",
		msg, name)
}

func discover(ctx context.Context, hc *http.Client, mcpURL string) (oauthServer, error) {
	host := hostOf(mcpURL)
	unsupported := refuse("%s doesn't offer sign-in. Paste an API key instead.", host)
	metaURL, scope, err := probe(ctx, hc, mcpURL)
	if err != nil {
		return oauthServer{}, err
	}
	var resource struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	found, err := firstJSON(ctx, hc, protectedResourceURLs(mcpURL, metaURL), &resource)
	if err != nil {
		return oauthServer{}, err
	}
	authServer := originOf(mcpURL)
	if found && len(resource.AuthorizationServers) > 0 && resource.AuthorizationServers[0] != "" {
		authServer = resource.AuthorizationServers[0]
	}
	// The metadata names where the code and verifier will go: never over plain http to another computer.
	if secureURL(authServer) != nil {
		return oauthServer{}, unsupported
	}
	var meta struct {
		Authorize       string   `json:"authorization_endpoint"`
		Token           string   `json:"token_endpoint"`
		Register        string   `json:"registration_endpoint"`
		ScopesSupported []string `json:"scopes_supported"`
	}
	ok, err := firstJSON(ctx, hc, authorizationMetadataURLs(authServer), &meta)
	if err != nil {
		return oauthServer{}, err
	}
	if !ok || meta.Authorize == "" || meta.Token == "" {
		return oauthServer{}, unsupported
	}
	srv := oauthServer{authorize: meta.Authorize, token: meta.Token, register: meta.Register, scope: scope,
		resource: resource.Resource}
	if srv.register == "" {
		srv.register = strings.TrimRight(authServer, "/") + "/register"
	}
	for _, u := range []string{srv.authorize, srv.token, srv.register} {
		if err := secureURL(u); err != nil {
			return oauthServer{}, unsupported
		}
	}
	if srv.scope == "" {
		srv.scope = strings.Join(resource.ScopesSupported, " ")
	}
	if srv.scope == "" {
		srv.scope = strings.Join(meta.ScopesSupported, " ")
	}
	return srv, nil
}

// probe asks the MCP server to initialize without a credential, and reads its WWW-Authenticate:
// where its resource metadata is, and the scope it wants.
func probe(ctx context.Context, hc *http.Client, mcpURL string) (metaURL, scope string, err error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "sign-in", "method": "initialize",
		"params": map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "meko", "version": version.Version}}})
	res, err := do(ctx, hc, http.MethodPost, mcpURL, map[string]string{"Content-Type": "application/json",
		"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": mcpProtocolVersion}, body)
	if err != nil {
		return "", "", err
	}
	h := res.header.Get("WWW-Authenticate")
	return challengeField(h, "resource_metadata"), challengeField(h, "scope"), nil
}

func register(ctx context.Context, hc *http.Client, srv oauthServer, redirectURI string) (OAuthClient, error) {
	req := map[string]any{"redirect_uris": []string{redirectURI}, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"client_name": "Meko", "software_id": "meko", "software_version": version.Version}
	if srv.scope != "" {
		req["scope"] = srv.scope
	}
	body, _ := json.Marshal(req)
	res, err := do(ctx, hc, http.MethodPost, srv.register, map[string]string{"Content-Type": "application/json"}, body)
	if err != nil {
		return OAuthClient{}, err
	}
	var c OAuthClient
	if res.status/100 != 2 || json.Unmarshal(res.body, &c) != nil || c.ID == "" {
		return OAuthClient{}, refuse("Meko Cloud didn't register Meko for sign-in (HTTP %d). Paste an API key instead.",
			res.status)
	}
	return c, nil
}

func (f *Flow) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.RedirectURI},
		"client_id": {f.client.ID}, "code_verifier": {f.verifier}}
	if f.server.resource != "" {
		form.Set("resource", f.server.resource)
	}
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	switch f.client.AuthMethod {
	case "client_secret_basic":
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(url.QueryEscape(f.client.ID)+":"+url.QueryEscape(f.client.Secret)))
	case "client_secret_post":
		if f.client.Secret != "" {
			form.Set("client_secret", f.client.Secret)
		}
	}
	res, err := do(ctx, f.hc, http.MethodPost, f.server.token, h, []byte(form.Encode()))
	if err != nil {
		return "", err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if res.status/100 != 2 || json.Unmarshal(res.body, &tok) != nil || tok.AccessToken == "" {
		return "", refuse("Meko Cloud didn't finish the sign-in (HTTP %d). Sign in again.", res.status)
	}
	return tok.AccessToken, nil
}

// APIKeysURL is where a key is made for an MCP URL: https://mcp.mekodata.ai/mcp's is
// https://mcp.mekodata.ai/api-keys.
func APIKeysURL(mcpURL string) string {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return ""
	}
	base := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/mcp")
	return u.Scheme + "://" + u.Host + base + "/api-keys"
}

func createAPIKey(ctx context.Context, hc *http.Client, mcpURL, accessToken, name string) (Minted, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	res, err := do(ctx, hc, http.MethodPost, APIKeysURL(mcpURL), map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer " + accessToken}, body)
	if err != nil {
		return Minted{}, err
	}
	var reply struct {
		APIKey string `json:"api_key"`
		Name   string `json:"name"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(res.body, &reply)
	host := hostOf(mcpURL)
	switch {
	case res.status == http.StatusNotFound || res.status == http.StatusMethodNotAllowed:
		return Minted{}, refuse("%s can't make API keys from a sign-in. Paste an API key instead.", host)
	case res.status == http.StatusTooManyRequests:
		return Minted{}, refuse("Your Meko Cloud account has as many API keys as it can. Revoke one in Meko " +
			"Cloud's Settings, then sign in again.")
	case res.status == http.StatusForbidden && reply.Error != nil && reply.Error.Code == "account_not_provisioned":
		return Minted{}, refuse("You signed in with a login that has no Meko Cloud account. Sign in again with the " +
			"way you normally sign in to Meko Cloud (Google, LinkedIn, or email and password). Nothing was saved.")
	case res.status/100 != 2 || !strings.HasPrefix(reply.APIKey, "mko_tkn_"):
		detail := ""
		if reply.Error != nil && reply.Error.Message != "" {
			detail = ": " + clip(reply.Error.Message)
		}
		return Minted{}, refuse("Meko Cloud didn't make an API key (HTTP %d)%s. Nothing was saved.", res.status, detail)
	}
	if reply.Name == "" {
		reply.Name = name
	}
	return Minted{Key: reply.APIKey, Name: reply.Name}, nil
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

// do makes one request of a sign-in, within SignInRequestTimeout, reading at most 1 MiB of the reply.
// Its error says what failed without the request's credentials.
func do(ctx context.Context, hc *http.Client, method, target string, header map[string]string, body []byte) (
	reply, error) {
	ctx, cancel := context.WithTimeout(ctx, SignInRequestTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, r)
	if err != nil {
		return reply{}, refuse("Meko Cloud gave a sign-in address that isn't a URL. Paste an API key instead.")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "meko/"+version.Version)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	// A redirect may not leave https for plain http (to another computer), whatever client is used.
	c := *hc
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if secureURL(next.URL.String()) != nil {
			return errors.New("it redirected to an address that isn't https")
		}
		if hc.CheckRedirect != nil {
			return hc.CheckRedirect(next, via)
		}
		return nil
	}
	res, err := c.Do(req) //nolint:gosec // G704: the link's own server, and the endpoints it names
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return reply{}, refuse("Couldn't reach %s to sign in (it timed out).", hostOf(target))
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return reply{}, refuse("Couldn't reach %s to sign in: %s.", hostOf(target), clip(err.Error()))
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return reply{status: res.StatusCode, header: res.Header, body: b}, nil
}

// firstJSON GETs each URL in turn into v, and reports whether one answered with JSON.
func firstJSON(ctx context.Context, hc *http.Client, urls []string, v any) (bool, error) {
	for _, u := range urls {
		res, err := do(ctx, hc, http.MethodGet, u, map[string]string{"MCP-Protocol-Version": mcpProtocolVersion}, nil)
		if err != nil {
			return false, err
		}
		if res.status/100 == 2 && json.Unmarshal(res.body, v) == nil {
			return true, nil
		}
	}
	return false, nil
}

func protectedResourceURLs(mcpURL, advertised string) []string {
	u, _ := url.Parse(mcpURL)
	var out []string
	if advertised != "" && secureURL(advertised) == nil {
		out = append(out, advertised)
	}
	origin := u.Scheme + "://" + u.Host
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		out = append(out, origin+"/.well-known/oauth-protected-resource"+p)
	}
	return append(out, origin+"/.well-known/oauth-protected-resource")
}

func authorizationMetadataURLs(authServer string) []string {
	u, err := url.Parse(authServer)
	if err != nil {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return []string{origin + "/.well-known/oauth-authorization-server" + p,
			origin + "/.well-known/openid-configuration" + p, origin + p + "/.well-known/openid-configuration"}
	}
	return []string{origin + "/.well-known/oauth-authorization-server", origin + "/.well-known/openid-configuration"}
}

// secureURL refuses a sign-in address that isn't https, but for one on this computer (a test's).
func secureURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return refuse("%q isn't a URL.", u)
	}
	if p.Scheme == "https" || p.Scheme == "http" && loopback(p.Hostname()) {
		return nil
	}
	return refuse("Signing in needs https, and %s isn't. Paste an API key instead.", hostOf(u))
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return u
	}
	return p.Host
}

func challengeField(header, field string) string {
	m := regexp.MustCompile(`(?i)` + field + `=(?:"([^"]+)"|([^\s,]+))`).FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// clip shortens a message from elsewhere to one line of at most 200 characters.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
