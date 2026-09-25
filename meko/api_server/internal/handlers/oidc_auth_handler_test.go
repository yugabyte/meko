package handlers_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// An issuer good enough to drive the handlers end to end: discovery, a JWKS the
// ID token verifies against, and a token endpoint that answers the code
// exchange. The security-relevant half of these handlers is what they do with
// the issuer's answer, which is why they are tested against a real exchange
// rather than a stubbed provider.
type oidcFakeIssuer struct {
	url      string
	key      *rsa.PrivateKey
	kid      string
	idToken  func(aud string) string // what the token endpoint returns
	tokenHit int
}

func newOIDCFakeIssuer(t *testing.T, endSession bool) *oidcFakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	f := &oidcFakeIssuer{key: key, kid: "test-key"}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                 f.url,
			"authorization_endpoint": f.url + "/authorize",
			"token_endpoint":         f.url + "/token",
			"jwks_uri":               f.url + "/jwks",
		}
		if endSession {
			doc["end_session_endpoint"] = f.url + "/logout"
		}
		writeJSON(w, doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA",
			"kid": f.kid,
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		f.tokenHit++
		body := map[string]any{"access_token": "opaque", "token_type": "Bearer"}
		if f.idToken != nil {
			if tok := f.idToken("meko"); tok != "" {
				body["id_token"] = tok
			}
		}
		writeJSON(w, body)
	})
	f.idToken = func(aud string) string { return f.sign(t, f.claims(aud, "user-1", "person@example.com")) }
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// oidcTestNonce is what the fake issuer echoes into every id_token, and what
// the callback tests put in the nonce cookie the login handler would have set.
const oidcTestNonce = "nonce-under-test"

func (f *oidcFakeIssuer) claims(aud, sub, email string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.url, "aud": aud, "sub": sub, "email": email,
		"nonce": oidcTestNonce,
		"exp":   time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
}

func (f *oidcFakeIssuer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func (f *oidcFakeIssuer) provider(t *testing.T) *auth.OIDCProvider {
	t.Helper()
	p, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
		IssuerURL:   f.url,
		ClientID:    "meko",
		RedirectURL: "https://meko.example.com/api/v1/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// oidcAuthService builds the AuthService the callback provisions through,
// against a store whose behaviour the caller decides.
func oidcAuthService(store *handlerMockAccountStore) *service.AuthService {
	return service.NewAuthService(service.NoUserStore{}, store, service.WithIssuerOwnedEmail())
}

func oidcRequest(target string, cookies ...*http.Cookie) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "meko.example.com"
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// cookieByName returns the LAST Set-Cookie with this name, which is what the
// browser ends up holding when a handler sets and then clears one.
func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	var found *http.Cookie
	for _, ck := range (&http.Response{Header: rec.Header()}).Cookies() {
		if ck.Name == name {
			c := ck
			found = c
		}
	}
	return found
}

func TestOIDCLoginHandler_RedirectsToTheIssuerAndStampsState(t *testing.T) {
	f := newOIDCFakeIssuer(t, false)
	c, rec := oidcRequest("/api/v1/auth/oidc/login")

	if err := handlers.OIDCLoginHandler(handlers.OIDCLoginConfig{Provider: f.provider(t)})(c); err != nil {
		t.Fatalf("login: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	for _, want := range []string{f.url + "/authorize", "client_id=meko", "code_challenge_method=S256", "scope=openid"} {
		if !strings.Contains(loc, want) {
			t.Errorf("authorize redirect missing %q:\n%s", want, loc)
		}
	}
	state := cookieByName(rec, "oauth_state")
	if state == nil || state.Value == "" {
		t.Fatal("no state cookie was set")
	}
	if !strings.Contains(loc, "state="+state.Value) {
		t.Error("the state in the redirect is not the one in the cookie")
	}
	if pkce := cookieByName(rec, "oauth_pkce"); pkce == nil || pkce.Value == "" {
		t.Error("no PKCE verifier cookie was set")
	}
	nonce := cookieByName(rec, "oidc_nonce")
	if nonce == nil || nonce.Value == "" {
		t.Fatal("no nonce cookie was set")
	}
	if !strings.Contains(loc, "nonce="+nonce.Value) {
		t.Error("the nonce in the redirect is not the one in the cookie")
	}
	if !state.HttpOnly {
		t.Error("state cookie is readable from JavaScript")
	}
}

// An unreachable issuer is a 503, not a redirect into a flow that cannot
// complete — and it leaves no state cookie behind to be replayed later.
func TestOIDCLoginHandler_503sWhenTheIssuerIsDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	p, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
		IssuerURL: dead.URL, ClientID: "meko",
		RedirectURL: "https://meko.example.com/api/v1/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	c, rec := oidcRequest("/api/v1/auth/oidc/login")
	if err := handlers.OIDCLoginHandler(handlers.OIDCLoginConfig{Provider: p})(c); err != nil {
		t.Fatalf("login: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if ck := cookieByName(rec, "oauth_state"); ck != nil {
		t.Error("a state cookie was stamped for a sign-in that never started")
	}
}

func TestOIDCLoginHandler_503sWithNoProvider(t *testing.T) {
	c, rec := oidcRequest("/api/v1/auth/oidc/login")
	if err := handlers.OIDCLoginHandler(handlers.OIDCLoginConfig{})(c); err != nil {
		t.Fatalf("login: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// State is the CSRF defence: a callback with none, or one that does not match
// the cookie, is rejected before the code is ever exchanged.
func TestOIDCCallbackHandler_RejectsBadState(t *testing.T) {
	cases := map[string]struct {
		query   string
		cookies []*http.Cookie
	}{
		"no cookie":      {query: "?code=c&state=s"},
		"empty cookie":   {query: "?code=c&state=s", cookies: []*http.Cookie{{Name: "oauth_state", Value: ""}}},
		"mismatch":       {query: "?code=c&state=other", cookies: []*http.Cookie{{Name: "oauth_state", Value: "s"}}},
		"no state param": {query: "?code=c", cookies: []*http.Cookie{{Name: "oauth_state", Value: "s"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOIDCFakeIssuer(t, false)
			store := noopAccountStore()
			c, rec := oidcRequest("/api/v1/auth/oidc/callback"+tc.query, tc.cookies...)

			err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
				Provider: f.provider(t), AuthService: oidcAuthService(store),
			})(c)
			if err != nil {
				t.Fatalf("callback: %v", err)
			}
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/signin?error=oidc_state") {
				t.Errorf("Location = %q, want the sign-in page with oidc_state", loc)
			}
			if f.tokenHit != 0 {
				t.Error("the code was exchanged despite a bad state")
			}
			if ck := cookieByName(rec, "access_token"); ck != nil && ck.Value != "" {
				t.Error("an auth cookie was set for a rejected callback")
			}
		})
	}
}

// The happy path: exchange, provision, then — and only then — the session
// cookies, plus the single-use state and PKCE cookies cleared.
func TestOIDCCallbackHandler_SignsIn(t *testing.T) {
	f := newOIDCFakeIssuer(t, false)
	var created bool
	store := noopAccountStore()
	store.getByCognitoSubFn = func(context.Context, string) (*models.Account, error) { return nil, nil }
	store.createFn = func(_ context.Context, p repository.CreateAccountParams) (*models.Account, error) {
		created = true
		if p.CognitoSub != "user-1" {
			t.Errorf("account created for sub %q, want user-1", p.CognitoSub)
		}
		// The issuer owns its users and sent no email_verified: an account that
		// lands here unverified is stuck at the SPA's onboarding modal, which
		// posts to a route this provider does not register.
		if !p.EmailVerified {
			t.Error("account created with email_verified=false under an OIDC issuer")
		}
		return &models.Account{ID: "acct-1"}, nil
	}

	c, rec := oidcRequest("/api/v1/auth/oidc/callback?code=the-code&state=s",
		&http.Cookie{Name: "oauth_state", Value: "s"},
		&http.Cookie{Name: "oauth_pkce", Value: "verifier"},
		&http.Cookie{Name: "oidc_nonce", Value: oidcTestNonce},
	)
	if err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
		Provider: f.provider(t), AuthService: oidcAuthService(store),
	})(c); err != nil {
		t.Fatalf("callback: %v", err)
	}

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "http://meko.example.com/" {
		t.Errorf("Location = %q, want the app root", loc)
	}
	if !created {
		t.Error("no account was provisioned")
	}
	for _, name := range []string{"access_token", "id_token"} {
		ck := cookieByName(rec, name)
		if ck == nil || ck.Value == "" {
			t.Errorf("%s cookie was not set", name)
			continue
		}
		if strings.Count(ck.Value, ".") != 2 {
			t.Errorf("%s is not the verified JWT: %q", name, ck.Value)
		}
	}
	// Single use: both are cleared on the way out, so a replayed callback has
	// nothing to match against.
	for _, name := range []string{"oauth_state", "oauth_pkce", "oidc_nonce"} {
		if ck := cookieByName(rec, name); ck == nil || ck.Value != "" || ck.MaxAge >= 0 {
			t.Errorf("%s was not cleared: %+v", name, ck)
		}
	}
}

// An id_token the issuer's own JWKS cannot verify is a forgery; the exchange
// fails and no session is created.
func TestOIDCCallbackHandler_RejectsAForgedIDToken(t *testing.T) {
	f := newOIDCFakeIssuer(t, false)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	f.idToken = func(aud string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims(aud, "user-1", "person@example.com"))
		tok.Header["kid"] = f.kid
		s, signErr := tok.SignedString(other)
		if signErr != nil {
			t.Fatalf("signing: %v", signErr)
		}
		return s
	}

	c, rec := oidcRequest("/api/v1/auth/oidc/callback?code=c&state=s",
		&http.Cookie{Name: "oauth_state", Value: "s"},
		&http.Cookie{Name: "oidc_nonce", Value: oidcTestNonce})
	if err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
		Provider: f.provider(t), AuthService: oidcAuthService(noopAccountStore()),
	})(c); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_exchange") {
		t.Errorf("Location = %q, want the sign-in page with oidc_exchange", loc)
	}
	if ck := cookieByName(rec, "access_token"); ck != nil && ck.Value != "" {
		t.Error("a session was created from an unverifiable id_token")
	}
}

// Provisioning runs before any cookie is set, so its failure leaves the caller
// signed out rather than holding a session that 401s on every call.
func TestOIDCCallbackHandler_SetsNoSessionWhenProvisioningFails(t *testing.T) {
	f := newOIDCFakeIssuer(t, false)
	store := noopAccountStore()
	store.getByCognitoSubFn = func(context.Context, string) (*models.Account, error) { return nil, nil }
	store.createFn = func(context.Context, repository.CreateAccountParams) (*models.Account, error) {
		return nil, errors.New("database is down")
	}

	c, rec := oidcRequest("/api/v1/auth/oidc/callback?code=c&state=s",
		&http.Cookie{Name: "oauth_state", Value: "s"},
		&http.Cookie{Name: "oidc_nonce", Value: oidcTestNonce})
	if err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
		Provider: f.provider(t), AuthService: oidcAuthService(store),
	})(c); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_provision") {
		t.Errorf("Location = %q, want the sign-in page with oidc_provision", loc)
	}
	if ck := cookieByName(rec, "access_token"); ck != nil && ck.Value != "" {
		t.Error("a session was created despite provisioning failing")
	}
}

// The issuer returning ?error= is its rejection, passed through rather than
// reported as our own failure.
func TestOIDCCallbackHandler_PassesTheIssuersErrorThrough(t *testing.T) {
	f := newOIDCFakeIssuer(t, false)
	c, rec := oidcRequest("/api/v1/auth/oidc/callback?state=s&error=access_denied",
		&http.Cookie{Name: "oauth_state", Value: "s"})
	if err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
		Provider: f.provider(t), AuthService: oidcAuthService(noopAccountStore()),
	})(c); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=access_denied") {
		t.Errorf("Location = %q, want the issuer's own error", loc)
	}
	if f.tokenHit != 0 {
		t.Error("a code exchange was attempted after the issuer refused")
	}
}

// Sign-out clears the local session either way; where the browser goes next
// depends on whether the issuer publishes RP-initiated logout.
func TestOIDCLogoutHandler(t *testing.T) {
	for name, endSession := range map[string]bool{"issuer logout": true, "local only": false} {
		t.Run(name, func(t *testing.T) {
			f := newOIDCFakeIssuer(t, endSession)
			c, rec := oidcRequest("/api/v1/auth/logout",
				&http.Cookie{Name: "id_token", Value: "the-id-token"})

			if err := handlers.OIDCLogoutHandler(handlers.OIDCLogoutConfig{Provider: f.provider(t)})(c); err != nil {
				t.Fatalf("logout: %v", err)
			}
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			loc := rec.Header().Get("Location")
			if endSession {
				for _, want := range []string{f.url + "/logout", "id_token_hint=the-id-token", "post_logout_redirect_uri="} {
					if !strings.Contains(loc, want) {
						t.Errorf("logout redirect missing %q:\n%s", want, loc)
					}
				}
			} else if loc != "http://meko.example.com/" {
				t.Errorf("Location = %q, want the app root", loc)
			}
			for _, name := range []string{"access_token", "id_token", "refresh_token"} {
				if ck := cookieByName(rec, name); ck == nil || ck.Value != "" || ck.MaxAge >= 0 {
					t.Errorf("%s was not cleared: %+v", name, ck)
				}
			}
		})
	}
}

// A callback whose nonce cookie is gone — or never matched the token — must not
// mint a session, even though state and the code exchange both check out.
func TestOIDCCallbackHandler_RejectsAMismatchedNonce(t *testing.T) {
	for name, cookies := range map[string][]*http.Cookie{
		"no nonce cookie": {{Name: "oauth_state", Value: "s"}},
		"another sign-in's nonce": {
			{Name: "oauth_state", Value: "s"},
			{Name: "oidc_nonce", Value: "not-the-one"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOIDCFakeIssuer(t, false)
			c, rec := oidcRequest("/api/v1/auth/oidc/callback?code=c&state=s", cookies...)

			if err := handlers.OIDCCallbackHandler(handlers.OIDCCallbackConfig{
				Provider: f.provider(t), AuthService: oidcAuthService(noopAccountStore()),
			})(c); err != nil {
				t.Fatalf("callback: %v", err)
			}
			if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=oidc_exchange") {
				t.Errorf("Location = %q, want the sign-in page with oidc_exchange", loc)
			}
			if ck := cookieByName(rec, "access_token"); ck != nil && ck.Value != "" {
				t.Error("a session was created from an id_token bound to another sign-in")
			}
		})
	}
}
