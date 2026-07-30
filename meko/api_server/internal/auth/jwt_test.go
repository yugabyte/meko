package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"meko/api_server/internal/auth"
)

const (
	testIssuer   = "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_TestPool"
	testClientID = "test-client-id"
	testKID      = "test-kid-1"
)

// testKeyPair generates a fresh 2048-bit RSA key pair for tests.
func testKeyPair(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	return key
}

// serveJWKS starts an httptest server that returns a JWKS containing
// the public half of the given RSA key.
func serveJWKS(
	t *testing.T, kid string, pub *rsa.PublicKey, //nolint:unparam // kid kept as param for test readability
) *httptest.Server {
	t.Helper()

	nBase64 := base64.RawURLEncoding.EncodeToString(
		pub.N.Bytes(),
	)
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	eBase64 := base64.RawURLEncoding.EncodeToString(eBytes)

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   nBase64,
				"e":   eBase64,
			},
		},
	}

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(jwks); err != nil {
				t.Logf("failed to encode jwks: %v", err)
			}
		}),
	)
	t.Cleanup(srv.Close)
	return srv
}

// signToken creates a signed JWT string using the given private key,
// kid, and claims.
func signToken(
	t *testing.T,
	key *rsa.PrivateKey,
	kid string, //nolint:unparam // kid kept as param for test readability
	claims jwt.MapClaims,
) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid

	tokenStr, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return tokenStr
}

// validClaims returns a set of claims that should pass validation.
func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"sub":       "user-sub-123",
		"username":  "user@example.com",
		"token_use": "access",
		"iss":       testIssuer,
		"exp":       jwt.NewNumericDate(now.Add(1 * time.Hour)),
		"iat":       jwt.NewNumericDate(now),
	}
}

func TestValidateAccessToken_Valid(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)

	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	tokenStr := signToken(t, key, testKID, validClaims())

	claims, err := validator.ValidateAccessToken(tokenStr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if claims.Sub != "user-sub-123" {
		t.Errorf("expected sub user-sub-123, got %s", claims.Sub)
	}
	if claims.Username != "user@example.com" {
		t.Errorf(
			"expected username user@example.com, got %s",
			claims.Username,
		)
	}
	if claims.TokenUse != "access" {
		t.Errorf("expected token_use access, got %s", claims.TokenUse)
	}
}

func TestValidateAccessToken_Expired(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)

	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	claims := validClaims()
	claims["exp"] = jwt.NewNumericDate(
		time.Now().Add(-1 * time.Hour),
	)
	tokenStr := signToken(t, key, testKID, claims)

	_, err := validator.ValidateAccessToken(tokenStr)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "token is expired") {
		t.Errorf("expected 'token is expired' in error, got: %v", err)
	}
}

func TestValidateAccessToken_WrongIssuer(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)

	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	claims := validClaims()
	claims["iss"] = "https://wrong-issuer.example.com"
	tokenStr := signToken(t, key, testKID, claims)

	_, err := validator.ValidateAccessToken(tokenStr)
	if err == nil {
		t.Fatal("expected error for wrong issuer, got nil")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("expected 'issuer' in error, got: %v", err)
	}
}

func TestValidateAccessToken_InvalidSignature(t *testing.T) {
	legitimateKey := testKeyPair(t)
	attackerKey := testKeyPair(t)

	jwksSrv := serveJWKS(t, testKID, &legitimateKey.PublicKey)

	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	tokenStr := signToken(t, attackerKey, testKID, validClaims())

	_, err := validator.ValidateAccessToken(tokenStr)
	if err == nil {
		t.Fatal("expected error for invalid signature, got nil")
	}
	if !strings.Contains(err.Error(), "verification error") {
		t.Errorf(
			"expected 'verification error' in error, got: %v", err,
		)
	}
}

func TestValidateAccessToken_WrongTokenUse(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)

	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	claims := validClaims()
	claims["token_use"] = "id"
	tokenStr := signToken(t, key, testKID, claims)

	_, err := validator.ValidateAccessToken(tokenStr)
	if err == nil {
		t.Fatal("expected error for wrong token_use, got nil")
	}
	if !strings.Contains(err.Error(), "not an access token") {
		t.Errorf(
			"expected 'not an access token' in error, got: %v", err,
		)
	}
}
