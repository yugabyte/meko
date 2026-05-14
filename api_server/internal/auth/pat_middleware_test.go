package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
)

// fakePATValidator implements auth.PATValidator with a canned verdict
// (or canned error) per call. Lets the WithPAT tests exercise every
// classification without booting the real service / DB stack.
type fakePATValidator struct {
	verdict *auth.PATValidateResult
	err     error
	calls   int
}

func (f *fakePATValidator) ValidatePAT(
	_ context.Context, _ auth.PATValidateInput,
) (*auth.PATValidateResult, error) {
	f.calls++
	return f.verdict, f.err
}

// fakePATAccountLookup implements auth.AccountLookup. EnsureAccount
// is unused on the PAT path; GetByID returns the canned account.
type fakePATAccountLookup struct {
	account *models.Account
	err     error
}

func (m *fakePATAccountLookup) EnsureAccount(
	_ context.Context, _, _ string,
) (*models.Account, error) {
	return m.account, m.err
}

func (m *fakePATAccountLookup) GetByID(
	_ context.Context, _ string,
) (*models.Account, error) {
	return m.account, m.err
}

// patEcho builds an Echo instance whose /protected handler is gated
// by RequireAuth(WithPAT). The real JWT validator is unused on the
// PAT path (bearer prefix dispatches first), so we pass a nil
// validator -- the test always exercises the PAT branch.
func patEcho(
	t *testing.T,
	pats *fakePATValidator,
	accounts auth.AccountLookup,
) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.GET("/protected", func(c echo.Context) error {
		acct, _ := c.Get(auth.ContextKeyAccount).(*models.Account)
		method, _ := c.Get(auth.ContextKeyAuthMethod).(string)
		patID, _ := c.Get(auth.ContextKeyPATID).(string)
		scopes, _ := c.Get(auth.ContextKeyPATScopes).([]string)
		return c.JSON(http.StatusOK, map[string]any{
			"account_id":  acct.ID,
			"auth_method": method,
			"pat_id":      patID,
			"scopes":      scopes,
		})
	}, auth.RequireAuth(nil, accounts, auth.WithPAT(pats)))
	return e
}

func TestRequireAuth_PAT_ValidPopulatesContext(t *testing.T) {
	pats := &fakePATValidator{
		verdict: &auth.PATValidateResult{
			Valid:   true,
			TokenID: "pat-1",
			UserID:  "user-1",
			Scopes:  []string{"read", "write"},
		},
	}
	accounts := &fakePATAccountLookup{
		account: &models.Account{ID: "user-1", CognitoSub: "sub-1"},
	}
	e := patEcho(t, pats, accounts)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer mko_tkn_aaaaaaaaaaaaaaaa")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["account_id"] != "user-1" {
		t.Errorf("expected account_id=user-1, got %v", body["account_id"])
	}
	if body["auth_method"] != "pat" {
		t.Errorf("expected auth_method=pat, got %v", body["auth_method"])
	}
	if body["pat_id"] != "pat-1" {
		t.Errorf("expected pat_id=pat-1, got %v", body["pat_id"])
	}
	scopes, _ := body["scopes"].([]any)
	if len(scopes) != 2 || scopes[0] != "read" || scopes[1] != "write" {
		t.Errorf("expected scopes=[read write], got %v", body["scopes"])
	}
	if pats.calls != 1 {
		t.Errorf("expected 1 validator call, got %d", pats.calls)
	}
}

func TestRequireAuth_PAT_InvalidVerdictReturns401(t *testing.T) {
	pats := &fakePATValidator{
		verdict: &auth.PATValidateResult{Valid: false, Reason: "expired"},
	}
	accounts := &fakePATAccountLookup{}
	e := patEcho(t, pats, accounts)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer mko_tkn_bbbbbbbbbbbbbbbb")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestRequireAuth_PAT_StorageErrorReturns500(t *testing.T) {
	pats := &fakePATValidator{err: errors.New("db down")}
	accounts := &fakePATAccountLookup{}
	e := patEcho(t, pats, accounts)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer mko_tkn_cccccccccccccccc")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestRequireAuth_PAT_AccountMissingReturns401(t *testing.T) {
	pats := &fakePATValidator{
		verdict: &auth.PATValidateResult{
			Valid: true, TokenID: "pat-1", UserID: "ghost", Scopes: []string{"*"},
		},
	}
	accounts := &fakePATAccountLookup{account: nil} // PAT outlived account
	e := patEcho(t, pats, accounts)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer mko_tkn_dddddddddddddddd")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequireScope_AllowsWildcard(t *testing.T) {
	e := echo.New()
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set(auth.ContextKeyPATScopes, []string{"*"})
				return next(c)
			}
		},
		auth.RequireScope(auth.ScopeWrite),
	)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("wildcard should bypass; got %d", rec.Code)
	}
}

func TestRequireScope_AllowsExactMatch(t *testing.T) {
	e := echo.New()
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set(auth.ContextKeyPATScopes, []string{"read"})
				return next(c)
			}
		},
		auth.RequireScope(auth.ScopeRead),
	)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("matching scope should pass; got %d", rec.Code)
	}
}

func TestRequireScope_403OnMismatch(t *testing.T) {
	e := echo.New()
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set(auth.ContextKeyPATScopes, []string{"read"})
				return next(c)
			}
		},
		auth.RequireScope(auth.ScopeWrite),
	)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("write required, only read held; want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INSUFFICIENT_SCOPE") {
		t.Errorf("expected INSUFFICIENT_SCOPE in body, got %s", rec.Body.String())
	}
}

func TestRequireScope_403WhenScopesMissing(t *testing.T) {
	e := echo.New()
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		auth.RequireScope(auth.ScopeRead),
	)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 when no scopes set, got %d", rec.Code)
	}
}
