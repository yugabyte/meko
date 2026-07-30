package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
)

// mockAccountLookup implements auth.AccountLookup for middleware tests.
//
// Both EnsureAccount (JWT path) and GetByID (PAT path) return the
// same canned (account, err) tuple so a single fixture covers both
// methods. Tests that need divergent behaviour can set byIDAccount /
// byIDErr to override the GetByID-only response.
type mockAccountLookup struct {
	account     *models.Account
	err         error
	byIDAccount *models.Account
	byIDErr     error
	byIDSet     bool
}

func (m *mockAccountLookup) EnsureAccount(
	_ context.Context, _, _ string,
) (*models.Account, error) {
	return m.account, m.err
}

func (m *mockAccountLookup) GetByID(
	_ context.Context, _ string,
) (*models.Account, error) {
	if m.byIDSet {
		return m.byIDAccount, m.byIDErr
	}
	return m.account, m.err
}

// setupMiddlewareTest creates an Echo instance with the RequireAuth
// middleware protecting a test handler that returns 200.
func setupMiddlewareTest(
	t *testing.T,
	validator *auth.JWTValidator,
	lookup auth.AccountLookup,
) *echo.Echo {
	t.Helper()

	e := echo.New()
	mw := auth.RequireAuth(validator, lookup)
	e.GET("/protected", func(c echo.Context) error {
		acct, ok := c.Get(auth.ContextKeyAccount).(*models.Account)
		if !ok || acct == nil {
			return c.JSON(http.StatusInternalServerError,
				map[string]string{"error": "no account in context"},
			)
		}
		return c.JSON(http.StatusOK,
			map[string]string{"account_id": acct.ID},
		)
	}, mw)

	return e
}

func TestRequireAuth_MissingHeader(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)
	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)
	lookup := &mockAccountLookup{}

	e := setupMiddlewareTest(t, validator, lookup)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/protected")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	var body map[string]any
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		t.Fatalf("failed to decode body: %v", decErr)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["code"] != "UNAUTHORIZED" {
		t.Errorf("expected code UNAUTHORIZED, got %v", errObj["code"])
	}
}

func TestRequireAuth_MalformedHeader(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)
	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)
	lookup := &mockAccountLookup{}

	e := setupMiddlewareTest(t, validator, lookup)
	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(
		http.MethodGet, srv.URL+"/protected", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "NotBearer some-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRequireAuth_InvalidToken(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)
	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)
	lookup := &mockAccountLookup{}

	e := setupMiddlewareTest(t, validator, lookup)
	srv := httptest.NewServer(e)
	defer srv.Close()

	req, err := http.NewRequest(
		http.MethodGet, srv.URL+"/protected", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer garbage-token-value")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRequireAuth_ValidToken(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)
	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	lookup := &mockAccountLookup{
		account: &models.Account{
			ID:         "acct-uuid-1",
			Email:      "user@example.com",
			CognitoSub: "user-sub-123",
		},
	}

	e := setupMiddlewareTest(t, validator, lookup)
	srv := httptest.NewServer(e)
	defer srv.Close()

	tokenStr := signToken(t, key, testKID, validClaims())

	req, err := http.NewRequest(
		http.MethodGet, srv.URL+"/protected", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenStr)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		t.Fatalf("failed to decode body: %v", decErr)
	}
	if body["account_id"] != "acct-uuid-1" {
		t.Errorf(
			"expected account_id acct-uuid-1, got %s",
			body["account_id"],
		)
	}
}

func TestRequireAuth_AccountLookupError(t *testing.T) {
	key := testKeyPair(t)
	jwksSrv := serveJWKS(t, testKID, &key.PublicKey)
	validator := auth.NewJWTValidatorWithJWKSURL(
		jwksSrv.URL, testIssuer, testClientID,
	)

	lookup := &mockAccountLookup{
		account: nil,
		err:     errors.New("db connection lost"),
	}

	e := setupMiddlewareTest(t, validator, lookup)
	srv := httptest.NewServer(e)
	defer srv.Close()

	claims := validClaims()
	claims["sub"] = "unknown-sub"
	claims["username"] = "ghost@example.com"
	tokenStr := signToken(t, key, testKID, claims)

	req, err := http.NewRequest(
		http.MethodGet, srv.URL+"/protected", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenStr)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}
