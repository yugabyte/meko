package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenUseAccess is the expected value of the "token_use" claim for
// Cognito access tokens.
const TokenUseAccess = "access"

// AccessTokenClaims holds the validated claims from a Cognito access token.
type AccessTokenClaims struct {
	Sub      string `json:"sub"`
	Username string `json:"username"`
	TokenUse string `json:"token_use"`
	jwt.RegisteredClaims
}

// JWTValidator verifies Cognito access tokens using the User Pool's
// JWKS (JSON Web Key Set).
type JWTValidator struct {
	fetched  time.Time
	keys     map[string]*rsa.PublicKey
	jwksURL  string
	issuer   string
	clientID string
	mu       sync.RWMutex
}

// jwksCacheTTL controls how long JWKS keys are cached before a refresh.
const jwksCacheTTL = 1 * time.Hour

// NewJWTValidator returns a JWTValidator configured for the given
// Cognito region and pool ID.
func NewJWTValidator(region, poolID, clientID string) *JWTValidator {
	baseURL := fmt.Sprintf(
		"https://cognito-idp.%s.amazonaws.com/%s", region, poolID,
	)
	return &JWTValidator{
		jwksURL:  baseURL + "/.well-known/jwks.json",
		issuer:   baseURL,
		clientID: clientID,
		keys:     make(map[string]*rsa.PublicKey),
	}
}

// NewJWTValidatorWithJWKSURL creates a JWTValidator with a custom JWKS
// URL (useful for testing with a local HTTP server).
func NewJWTValidatorWithJWKSURL(
	jwksURL, issuer, clientID string,
) *JWTValidator {
	return &JWTValidator{
		jwksURL:  jwksURL,
		issuer:   issuer,
		clientID: clientID,
		keys:     make(map[string]*rsa.PublicKey),
	}
}

// ValidateAccessToken parses and validates a Cognito access token JWT.
// It verifies the signature against the JWKS, checks expiry, issuer,
// and that token_use is "access".
func (v *JWTValidator) ValidateAccessToken(
	tokenString string,
) (*AccessTokenClaims, error) {
	claims := &AccessTokenClaims{}

	token, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		v.keyFunc,
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		slog.Debug("jwt validation failed", "error", err)
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	if !token.Valid {
		return nil, ErrInvalidToken
	}

	if claims.TokenUse != TokenUseAccess {
		slog.Debug("jwt wrong token_use",
			"expected", TokenUseAccess,
			"got", claims.TokenUse,
		)
		return nil, ErrWrongTokenUse
	}

	slog.Debug("jwt validation succeeded",
		"sub", claims.Sub,
		"username", claims.Username,
	)
	return claims, nil
}

// ErrInvalidToken indicates a token that failed validation.
var ErrInvalidToken = errors.New("invalid token")

// ErrWrongTokenUse indicates a token whose token_use claim is not "access".
var ErrWrongTokenUse = errors.New("token is not an access token")

// keyFunc is called by jwt.Parse to resolve the signing key for a given
// token header. It uses the "kid" header to look up the correct RSA
// public key from the cached JWKS.
func (v *JWTValidator) keyFunc(
	token *jwt.Token,
) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf(
			"unexpected signing method: %v", token.Header["alg"],
		)
	}

	kid, ok := token.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, errors.New("token missing kid header")
	}

	key, err := v.getKey(kid)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// getKey returns the RSA public key for the given kid, fetching the
// JWKS if necessary.
func (v *JWTValidator) getKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, found := v.keys[kid]
	stale := time.Since(v.fetched) > jwksCacheTTL
	v.mu.RUnlock()

	if found && !stale {
		return key, nil
	}

	if err := v.refreshKeys(); err != nil {
		return nil, fmt.Errorf("failed to refresh jwks: %w", err)
	}

	v.mu.RLock()
	key, found = v.keys[kid]
	v.mu.RUnlock()

	if !found {
		return nil, fmt.Errorf("key %s not found in jwks", kid)
	}
	return key, nil
}

// jwksResponse models the JSON structure returned by the Cognito JWKS
// endpoint.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// refreshKeys fetches the JWKS from the configured URL and caches the
// parsed RSA public keys.
func (v *JWTValidator) refreshKeys() error {
	slog.Debug("fetching jwks", "url", v.jwksURL)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(v.jwksURL) //nolint:gosec // URL is from trusted config
	if err != nil {
		slog.Error("jwks fetch failed", "url", v.jwksURL, "error", err)
		return fmt.Errorf("fetching jwks: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close jwks response body",
				"error", closeErr,
			)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading jwks body: %w", err)
	}

	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parsing jwks json: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Use != "sig" {
			continue
		}
		pubKey, parseErr := parseRSAPublicKey(k.N, k.E)
		if parseErr != nil {
			slog.Warn("skipping jwk",
				"kid", k.Kid, "error", parseErr,
			)
			continue
		}
		newKeys[k.Kid] = pubKey
	}

	v.mu.Lock()
	v.keys = newKeys
	v.fetched = time.Now()
	v.mu.Unlock()

	slog.Debug("jwks refreshed", "key_count", len(newKeys))
	return nil
}

// parseRSAPublicKey decodes the base64url-encoded modulus and exponent
// from a JWK into an *rsa.PublicKey.
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("decoding modulus: %w", err)
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("decoding exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}
