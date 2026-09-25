package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// OIDCProvider is the generic OpenID Connect identity provider: any issuer that
// serves a discovery document. It is the half of an IdP this server actually
// needs — who is this request, and how does a browser sign in — and deliberately
// not the other half, the user database that CognitoProvider wraps. An OIDC
// issuer owns its own users, so signup, password reset and email verification
// happen on the issuer's pages and have no endpoint here.
//
// Everything is discovered rather than configured: the authorization, token and
// JWKS endpoints all come from /.well-known/openid-configuration, so pointing
// this at Keycloak, Dex, Okta, Auth0 or Google is one issuer URL.
//
// Discovery is attempted at construction and retried on demand, because the
// issuer is a separate deployment with its own outages: failing construction
// would take the whole api-server down with it, including the PAT and MCP
// traffic that never touches an IdP, and turn an issuer restart into a
// crashloop.
type OIDCProvider struct {
	cfg OIDCConfig

	// mu guards the discovered half. Everything above it is fixed at
	// construction; everything below is nil until discovery succeeds.
	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	// endSession is the issuer's RP-initiated logout endpoint. Optional in the
	// spec and absent from some issuers, which is why sign-out degrades to
	// clearing our own cookies rather than failing.
	endSession string
	// nextRetry rate-limits rediscovery, so an issuer that is down does not get
	// one discovery request per sign-in attempt.
	nextRetry time.Time
}

// ErrOIDCUnavailable is what every entry point returns while discovery has not
// succeeded. Callers turn it into a 503: the deployment is configured for OIDC,
// the issuer is not answering, and that is a temporary state rather than a
// rejection of the caller.
var ErrOIDCUnavailable = errors.New("oidc: issuer not reachable yet")

// discoveryRetryInterval is how long a failed discovery is remembered. Short
// enough that a recovered issuer starts serving sign-ins on its own, long
// enough that a down one is not hammered once per request.
const discoveryRetryInterval = 30 * time.Second

// OIDCConfig is everything an operator must supply; the rest is discovered.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback this server serves. It must match the issuer's
	// registered redirect URI byte-for-byte, so it is configured rather than
	// derived — behind a proxy the server cannot know its own public origin.
	RedirectURL string
	// Scopes beyond the mandatory openid. Defaults to profile+email, which is
	// what account provisioning reads.
	Scopes []string
}

// defaultOIDCScopes are what EnsureAccountFromOIDC needs to fill an account:
// a subject (always present), a name and an email.
var defaultOIDCScopes = []string{oidc.ScopeOpenID, "profile", "email"}

// NewOIDCProvider validates the configuration and tries discovery once.
//
// Only a configuration error fails: an unreachable issuer is reported and
// retried, because the alternative is an api-server that will not boot while
// somebody else's service is down. Until discovery lands, every OIDC entry
// point returns ErrOIDCUnavailable and the handlers serve 503.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: issuer URL and client ID are required")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = defaultOIDCScopes
	} else {
		// openid is what makes this an OIDC request rather than a bare OAuth2
		// one: without it the issuer returns no id_token and Exchange fails on
		// every login. The operator's list is "scopes beyond openid", so it is
		// prepended rather than demanded of them.
		cfg.Scopes = withOpenIDScope(cfg.Scopes)
	}
	cfg.IssuerURL = strings.TrimRight(cfg.IssuerURL, "/")

	p := &OIDCProvider{cfg: cfg}
	if err := p.discover(ctx); err != nil {
		slog.Error("oidc: discovery failed, sign-in unavailable until the issuer answers",
			"issuer", cfg.IssuerURL, "error", err, "retry_after", discoveryRetryInterval)
	}
	return p, nil
}

// withOpenIDScope returns scopes with oidc.ScopeOpenID present exactly once.
func withOpenIDScope(scopes []string) []string {
	for _, s := range scopes {
		if s == oidc.ScopeOpenID {
			return scopes
		}
	}
	return append([]string{oidc.ScopeOpenID}, scopes...)
}

// discover performs the one network call this provider needs and publishes the
// result. Safe to call concurrently; a caller that arrives during another's
// attempt waits for it rather than issuing a second one.
func (p *OIDCProvider) discover(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return nil
	}
	if time.Now().Before(p.nextRetry) {
		return ErrOIDCUnavailable
	}
	p.nextRetry = time.Now().Add(discoveryRetryInterval)

	provider, err := oidc.NewProvider(ctx, p.cfg.IssuerURL)
	if err != nil {
		return fmt.Errorf("oidc discovery against %s: %w", p.cfg.IssuerURL, err)
	}

	// end_session_endpoint is not in oidc.Provider's struct, so it comes off the
	// raw discovery document.
	var discovery struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovery); err != nil {
		// Not fatal: only sign-out loses a step, and the claim is optional.
		slog.Warn("oidc: could not read discovery claims", "error", err)
	}

	p.provider = provider
	p.verifier = provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID})
	p.endSession = discovery.EndSession
	p.oauth = &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       p.cfg.Scopes,
	}
	slog.Info("oidc: discovery complete", "issuer", p.cfg.IssuerURL, "scopes", p.cfg.Scopes)
	return nil
}

// ready returns the discovered half, or ErrOIDCUnavailable after trying once
// more. Callers hold nothing: the returned values are immutable once set.
func (p *OIDCProvider) ready(ctx context.Context) (*oidc.IDTokenVerifier, *oauth2.Config, error) {
	p.mu.Lock()
	v, o := p.verifier, p.oauth
	p.mu.Unlock()
	if v != nil {
		return v, o, nil
	}
	if err := p.discover(ctx); err != nil {
		return nil, nil, ErrOIDCUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifier, p.oauth, nil
}

// AuthCodeURL is where the browser is sent to sign in, carrying the CSRF state,
// the PKCE challenge and a nonce. PKCE is sent unconditionally: it is mandatory
// for a public client, harmless for a confidential one, and an issuer that does
// not support it ignores the two parameters. The nonce binds the ID token to
// this particular request — state and PKCE already cover the code flow, so it is
// the belt to their braces, and it is what the spec asks of an RP.
func (p *OIDCProvider) AuthCodeURL(ctx context.Context, state, verifier, nonce string) (string, error) {
	_, oauth, err := p.ready(ctx)
	if err != nil {
		return "", err
	}
	return oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
	), nil
}

// Exchange trades the authorization code for tokens and verifies the ID token.
//
// The ID token is what this server treats as the session credential, and the
// Result carries it in BOTH AccessToken and IDToken. That is deliberate: an
// OIDC access token may legitimately be opaque, and only the ID token is
// guaranteed to be a JWT the RP can verify. Putting it in AccessToken is what
// lets the existing cookie contract and middleware stay exactly as they are —
// see OIDCValidator, which verifies the same token on the way back in.
func (p *OIDCProvider) Exchange(
	ctx context.Context, code, verifier, nonce string,
) (*Result, error) {
	verify, oauth, err := p.ready(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oidc: code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("oidc: token response carried no id_token")
	}
	idToken, err := verify.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("oidc: id token verification: %w", err)
	}
	// The nonce the login handler generated has to come back in the token. An
	// empty one on our side means the cookie was lost, which is a rejection
	// rather than a reason to skip the check.
	if nonce == "" || idToken.Nonce != nonce {
		return nil, errors.New("oidc: id token nonce does not match this sign-in")
	}

	var expiresIn int32
	if !tok.Expiry.IsZero() {
		if s := time.Until(tok.Expiry).Seconds(); s > 0 {
			expiresIn = int32(s)
		}
	}
	return &Result{
		AccessToken:  raw,
		IDToken:      raw,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    expiresIn,
	}, nil
}

// EndSessionURL is where a browser is sent to sign out of the issuer itself.
// Empty when the issuer publishes no end_session_endpoint, in which case the
// caller clears its own cookies and stops there — the local session ends, the
// issuer's does not, and the next sign-in is silent.
func (p *OIDCProvider) EndSessionURL(idTokenHint, postLogoutRedirect string) string {
	p.mu.Lock()
	endSession := p.endSession
	p.mu.Unlock()
	if endSession == "" {
		return ""
	}
	u := endSession + "?client_id=" + p.cfg.ClientID
	if idTokenHint != "" {
		u += "&id_token_hint=" + idTokenHint
	}
	if postLogoutRedirect != "" {
		u += "&post_logout_redirect_uri=" + postLogoutRedirect
	}
	return u
}

// OIDCValidator adapts an OIDCProvider to TokenValidator, so the auth
// middleware does not know which IdP issued the bearer it is checking.
//
// It verifies the token as an ID token — signature against the issuer's JWKS,
// issuer, audience and expiry — because that is what Exchange put in the
// cookie. The Cognito validator's token_use check has no counterpart here: it
// is a Cognito-specific claim, and requiring it would reject every conforming
// OIDC issuer.
type OIDCValidator struct {
	provider *OIDCProvider
}

// NewOIDCValidator returns the TokenValidator for an already-discovered provider.
func NewOIDCValidator(p *OIDCProvider) *OIDCValidator {
	return &OIDCValidator{provider: p}
}

// ValidateAccessToken implements TokenValidator.
func (v *OIDCValidator) ValidateAccessToken(tokenString string) (*AccessTokenClaims, error) {
	ctx := context.Background()
	verifier, _, err := v.provider.ready(ctx)
	if err != nil {
		// Not ErrInvalidToken: the token was never examined. The caller cannot
		// fix this by signing in again, and the log line is the operator's.
		slog.Warn("oidc token validation skipped: issuer unreachable", "error", err)
		return nil, err
	}
	idToken, err := verifier.Verify(ctx, tokenString)
	if err != nil {
		slog.Debug("oidc token validation failed", "error", err)
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	var c struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&c); err != nil {
		slog.Debug("oidc token claims unreadable", "error", err)
		return nil, fmt.Errorf("%w: claims: %w", ErrInvalidToken, err)
	}

	// token_use is filled with the Cognito-equivalent value rather than left
	// empty, so anything downstream that compares it keeps working against
	// either provider.
	return &AccessTokenClaims{
		Sub:      idToken.Subject,
		Username: firstNonEmpty(c.PreferredUsername, c.Email, idToken.Subject),
		TokenUse: TokenUseAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   idToken.Subject,
			Issuer:    idToken.Issuer,
			ExpiresAt: jwt.NewNumericDate(idToken.Expiry),
		},
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
