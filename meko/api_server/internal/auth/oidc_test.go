package auth

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIssuer is a minimal OIDC provider: a discovery document, a JWKS, and a
// token endpoint. Enough to exercise everything NewOIDCProvider discovers and
// everything OIDCValidator verifies, without reaching the network.
type fakeIssuer struct {
	srv        *httptest.Server
	key        *rsa.PrivateKey
	kid        string
	url        string
	endSession bool
	// idToken is what the token endpoint hands back; set per test.
	idToken string
}

func newFakeIssuer(t *testing.T, endSession bool) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	f := &fakeIssuer{key: key, kid: "test-key", endSession: endSession}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                f.url,
			"authorization_endpoint":                f.url + "/authorize",
			"token_endpoint":                        f.url + "/token",
			"jwks_uri":                              f.url + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if f.endSession {
			doc["end_session_endpoint"] = f.url + "/logout"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": f.kid, "n": n, "e": e},
		}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "opaque-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     f.idToken,
		})
	})

	f.srv = httptest.NewServer(mux)
	f.url = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// sign mints an ID token for this issuer.
func (f *fakeIssuer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

// testNonce is what standardClaims echoes, so an Exchange test passes the nonce
// the login handler would have sent.
const testNonce = "nonce-abc"

func (f *fakeIssuer) standardClaims(aud string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":                f.url,
		"aud":                aud,
		"sub":                "user-123",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"email":              "someone@example.com",
		"preferred_username": "someone",
		"nonce":              testNonce,
	}
}

func newTestProvider(t *testing.T, f *fakeIssuer, clientID string) *OIDCProvider {
	t.Helper()
	p, err := NewOIDCProvider(context.Background(), OIDCConfig{
		IssuerURL:   f.url,
		ClientID:    clientID,
		RedirectURL: "https://meko.example.com/api/v1/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// Everything but the issuer and client is discovered, so a provider is usable
// with nothing else configured.
func TestOIDCProvider_DiscoversEndpoints(t *testing.T) {
	f := newFakeIssuer(t, true)
	p := newTestProvider(t, f, "meko")

	url, err := p.AuthCodeURL(context.Background(), "state-abc", "verifier-xyz", testNonce)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	for _, want := range []string{
		f.url + "/authorize",
		"client_id=meko",
		"state=state-abc",
		// PKCE goes on every request: mandatory for a public client, ignored by
		// an issuer that does not support it.
		"code_challenge=",
		"code_challenge_method=S256",
		"scope=openid+profile+email",
		"nonce=" + testNonce,
	} {
		if !strings.Contains(url, want) {
			t.Errorf("authorize URL missing %q:\n%s", want, url)
		}
	}
}

// A valid ID token resolves to the subject the middleware looks accounts up by.
func TestOIDCValidator_AcceptsAValidIDToken(t *testing.T) {
	f := newFakeIssuer(t, false)
	p := newTestProvider(t, f, "meko")
	raw := f.sign(t, f.standardClaims("meko"))

	claims, err := NewOIDCValidator(p).ValidateAccessToken(raw)
	if err != nil {
		t.Fatalf("ValidateAccessToken: %v", err)
	}
	if claims.Sub != "user-123" {
		t.Errorf("sub = %q, want user-123", claims.Sub)
	}
	if claims.Username != "someone" {
		t.Errorf("username = %q, want someone", claims.Username)
	}
	// Cognito's token_use has no OIDC counterpart, so it is synthesized rather
	// than left empty — anything downstream comparing it works on either provider.
	if claims.TokenUse != TokenUseAccess {
		t.Errorf("token_use = %q, want %q", claims.TokenUse, TokenUseAccess)
	}
}

// The three rejections that matter: another issuer's token, another client's
// token, and an expired one.
func TestOIDCValidator_Rejects(t *testing.T) {
	f := newFakeIssuer(t, false)
	other := newFakeIssuer(t, false)
	p := newTestProvider(t, f, "meko")
	v := NewOIDCValidator(p)

	wrongAudience := f.sign(t, f.standardClaims("some-other-client"))

	expired := f.standardClaims("meko")
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	foreign := other.standardClaims("meko")

	for name, token := range map[string]string{
		"wrong audience": wrongAudience,
		"expired":        f.sign(t, expired),
		"foreign issuer": other.sign(t, foreign),
		"not a jwt":      "not-a-token",
	} {
		if _, err := v.ValidateAccessToken(token); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

// Exchange verifies the ID token before returning it, and puts it in BOTH
// fields so the existing cookie contract needs no provider-specific branch.
func TestOIDCProvider_ExchangeVerifiesAndFillsBothTokens(t *testing.T) {
	f := newFakeIssuer(t, false)
	p := newTestProvider(t, f, "meko")
	f.idToken = f.sign(t, f.standardClaims("meko"))

	res, err := p.Exchange(context.Background(), "the-code", "the-verifier", testNonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.IDToken != f.idToken {
		t.Errorf("IDToken not the issuer's token")
	}
	if res.AccessToken != res.IDToken {
		t.Errorf("AccessToken must carry the ID token: an OIDC access token may be opaque")
	}
}

// An ID token the issuer did not sign must not survive the exchange, or a
// forged code response would mint a session.
func TestOIDCProvider_ExchangeRejectsAnUnverifiableIDToken(t *testing.T) {
	f := newFakeIssuer(t, false)
	other := newFakeIssuer(t, false)
	p := newTestProvider(t, f, "meko")
	f.idToken = other.sign(t, other.standardClaims("meko"))

	if _, err := p.Exchange(context.Background(), "the-code", "the-verifier", testNonce); err == nil {
		t.Fatal("expected verification failure for a foreign-signed id_token")
	}
}

// end_session_endpoint is optional. Present, sign-out goes through the issuer;
// absent, the caller falls back to clearing its own cookies.
func TestOIDCProvider_EndSessionURL(t *testing.T) {
	with := newTestProvider(t, newFakeIssuer(t, true), "meko")
	u := with.EndSessionURL("the-id-token", "https://meko.example.com")
	for _, want := range []string{"/logout", "client_id=meko", "id_token_hint=the-id-token", "post_logout_redirect_uri="} {
		if !strings.Contains(u, want) {
			t.Errorf("end session URL missing %q:\n%s", want, u)
		}
	}

	without := newTestProvider(t, newFakeIssuer(t, false), "meko")
	if got := without.EndSessionURL("t", "https://meko.example.com"); got != "" {
		t.Errorf("issuer publishes no end_session_endpoint, want \"\", got %q", got)
	}
}

// A misconfiguration fails at boot, not on the first sign-in attempt.
func TestNewOIDCProvider_RequiresIssuerAndClient(t *testing.T) {
	for name, cfg := range map[string]OIDCConfig{
		"no issuer": {ClientID: "meko"},
		"no client": {IssuerURL: "https://example.com"},
	} {
		if _, err := NewOIDCProvider(context.Background(), cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// openid is what makes the request an OIDC one. An operator who lists the
// scopes they want beyond it must not lose it, or the issuer returns no
// id_token and every login fails at Exchange.
func TestNewOIDCProvider_AlwaysRequestsOpenID(t *testing.T) {
	f := newFakeIssuer(t, false)
	for name, scopes := range map[string][]string{
		"operator omitted it":     {"profile", "email", "groups"},
		"operator spelled it out": {"openid", "groups"},
		"unset":                   nil,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := NewOIDCProvider(context.Background(), OIDCConfig{
				IssuerURL: f.url, ClientID: "meko", Scopes: scopes,
			})
			if err != nil {
				t.Fatalf("NewOIDCProvider: %v", err)
			}
			url, err := p.AuthCodeURL(context.Background(), "s", "v", testNonce)
			if err != nil {
				t.Fatalf("AuthCodeURL: %v", err)
			}
			if !strings.Contains(url, "scope=openid") {
				t.Errorf("authorize URL requests no openid scope:\n%s", url)
			}
			if strings.Count(url, "openid") != 1 {
				t.Errorf("openid requested more than once:\n%s", url)
			}
		})
	}
}

// An issuer that is down must not take the api-server with it: construction
// succeeds, sign-in reports unavailable, and PAT traffic (which never reaches
// this validator) keeps serving.
func TestNewOIDCProvider_SurvivesAnUnreachableIssuer(t *testing.T) {
	// A listener that is closed immediately, so the address refuses connections
	// rather than hanging.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	p, err := NewOIDCProvider(context.Background(), OIDCConfig{
		IssuerURL: dead.URL, ClientID: "meko",
		RedirectURL: "https://meko.example.com/api/v1/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("construction must not fail on an unreachable issuer: %v", err)
	}
	if _, err := p.AuthCodeURL(context.Background(), "s", "v", testNonce); !errors.Is(err, ErrOIDCUnavailable) {
		t.Errorf("AuthCodeURL: want ErrOIDCUnavailable, got %v", err)
	}
	if _, err := p.Exchange(context.Background(), "code", "v", testNonce); !errors.Is(err, ErrOIDCUnavailable) {
		t.Errorf("Exchange: want ErrOIDCUnavailable, got %v", err)
	}
	if _, err := NewOIDCValidator(p).ValidateAccessToken("whatever"); !errors.Is(err, ErrOIDCUnavailable) {
		t.Errorf("ValidateAccessToken: want ErrOIDCUnavailable, got %v", err)
	}
	if u := p.EndSessionURL("hint", "https://meko.example.com"); u != "" {
		t.Errorf("EndSessionURL before discovery: want empty, got %q", u)
	}
}

// ...and it recovers on its own once the issuer answers, without a restart.
func TestOIDCProvider_RecoversWhenTheIssuerComesBack(t *testing.T) {
	f := newFakeIssuer(t, false)
	p := newTestProvider(t, f, "meko")

	// The state a provider is in after a failed discovery: configured, with
	// nothing discovered yet and the retry window elapsed.
	p.mu.Lock()
	p.provider, p.verifier, p.oauth = nil, nil, nil
	p.nextRetry = time.Time{}
	p.mu.Unlock()

	url, err := p.AuthCodeURL(context.Background(), "s", "v", testNonce)
	if err != nil {
		t.Fatalf("AuthCodeURL after the issuer came back: %v", err)
	}
	if !strings.Contains(url, f.url+"/authorize") {
		t.Errorf("rediscovery produced the wrong authorize endpoint:\n%s", url)
	}
}

// A failed discovery is remembered for discoveryRetryInterval, so an issuer
// that is down gets one attempt per window rather than one per request.
func TestOIDCProvider_RateLimitsRediscovery(t *testing.T) {
	var attempts int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)

	p, err := NewOIDCProvider(context.Background(), OIDCConfig{IssuerURL: down.URL, ClientID: "meko"})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	for range 5 {
		_, _ = p.AuthCodeURL(context.Background(), "s", "v", testNonce)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("discovery attempted %d times in one retry window, want 1", got)
	}
}

// The nonce binds the id_token to the sign-in that started it: one the issuer
// did not echo back, or one this server never sent, is a rejection.
func TestOIDCProvider_ExchangeRequiresTheNonce(t *testing.T) {
	for name, tc := range map[string]struct{ tokenNonce, ours string }{
		"issuer echoed nothing": {"", testNonce},
		"issuer echoed another": {"someone-elses", testNonce},
		"we have no nonce":      {testNonce, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeIssuer(t, false)
			p := newTestProvider(t, f, "meko")
			claims := f.standardClaims("meko")
			if tc.tokenNonce == "" {
				delete(claims, "nonce")
			} else {
				claims["nonce"] = tc.tokenNonce
			}
			f.idToken = f.sign(t, claims)

			if _, err := p.Exchange(context.Background(), "the-code", "the-verifier", tc.ours); err == nil {
				t.Fatal("expected the exchange to reject the id_token")
			}
		})
	}
}
