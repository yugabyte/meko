package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// setupAuthTestServerWithAccount mirrors setupAuthTestServer, but also wires
// the four routes whose extractAccessToken call previously no-opped on a
// missing token (ChangePassword, StartEmailChange, ResendEmailChangeCode,
// VerifyEmailChange), and injects an authenticated account into context the
// same way requireAuth does in production -- StartEmailChange/VerifyEmailChange
// check auth.GetAccount(c) before extractAccessToken, so these tests need an
// account present to actually reach the code path under test.
func setupAuthTestServerWithAccount(
	t *testing.T, cog service.CognitoProvider, store service.AccountStore,
) *httptest.Server {
	t.Helper()

	authSvc := service.NewAuthService(cog, store)
	authH := handlers.NewAuthHandler(authSvc, nil)

	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = handlers.CustomHTTPErrorHandler
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyAccount, &models.Account{ID: "acct-1"})
			return next(c)
		}
	})

	g := e.Group("/api/v1/auth")
	g.POST("/change-password", authH.ChangePassword)
	g.POST("/me/email/start", authH.StartEmailChange)
	g.POST("/me/email/resend", authH.ResendEmailChangeCode)
	g.POST("/me/email/verify", authH.VerifyEmailChange)

	return httptest.NewServer(e)
}

// TestAuth_MissingAccessToken_WritesExactlyOneResponse is the regression
// test for the extractAccessToken fix: before it, a missing access token
// wrote a 401 body and returned a nil error (apiError's return is c.JSON's
// write error, always nil on success), so ChangePassword/StartEmailChange/
// ResendEmailChangeCode/VerifyEmailChange fell through to the underlying
// AuthService call. That call's own accessToken=="" guard rejected it too,
// so Cognito itself was never reached -- but classifyAuthErr wrote a SECOND
// response on top of the first, the same concatenated-body bug class as
// MEKO-656/MEKO-690's memories endpoints. Logout already had its own
// `accessToken == ""` workaround and is covered by TestAuth_Logout_NoToken;
// these four did not.
func TestAuth_MissingAccessToken_WritesExactlyOneResponse(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"ChangePassword", "/change-password"},
		{"StartEmailChange", "/me/email/start"},
		{"ResendEmailChangeCode", "/me/email/resend"},
		{"VerifyEmailChange", "/me/email/verify"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cognitoCalled := false
			cog := &handlerMockCognito{
				changePasswordFn: func(context.Context, string, string, string) error {
					cognitoCalled = true
					return nil
				},
				resendFn: func(context.Context, string) error {
					cognitoCalled = true
					return nil
				},
			}
			srv := setupAuthTestServerWithAccount(t, cog, noopAccountStore())
			defer srv.Close()

			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately no access_token cookie and no Authorization header.

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST %s failed: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			if cognitoCalled {
				t.Fatal("expected Cognito never to be called without an access token")
			}

			dec := json.NewDecoder(resp.Body)
			var body handlers.APIResponse
			if err := dec.Decode(&body); err != nil {
				t.Fatalf("failed to decode response body: %v", err)
			}
			if body.Error == nil {
				t.Fatalf("expected an error envelope, got %+v", body)
			}
			if dec.More() {
				var extra json.RawMessage
				_ = dec.Decode(&extra)
				t.Fatalf("response body contains a second JSON document: %s", extra)
			}
		})
	}
}
