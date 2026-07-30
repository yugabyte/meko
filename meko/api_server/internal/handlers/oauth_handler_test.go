package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v4"
)

// ctxForRoute builds a context whose *registered route path* is routePath —
// that, not the request URL, is what mountPrefix reads.
func ctxForRoute(routePath string) echo.Context {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, routePath, nil), httptest.NewRecorder())
	c.SetPath(routePath)
	return c
}

func TestMountPrefix(t *testing.T) {
	cases := []struct {
		route     string
		routePath string
		want      string
	}{
		{"/api/v1/auth/google/login", "/api/v1/auth/google/login", ""},
		{"/ycp/api/v1/auth/google/login", "/api/v1/auth/google/login", "/ycp"},
		{"/ycp/api/v1/auth/google/callback", "/api/v1/auth/google/callback", "/ycp"},
		{"/a/b/api/v1/auth/linkedin/login", "/api/v1/auth/linkedin/login", "/a/b"},
		// Unexpected route shape falls back to no prefix rather than guessing.
		{"/somewhere/else", "/api/v1/auth/google/login", ""},
	}
	for _, c := range cases {
		if got := mountPrefix(ctxForRoute(c.route), c.routePath); got != c.want {
			t.Errorf("mountPrefix(route=%q, %q) = %q, want %q", c.route, c.routePath, got, c.want)
		}
	}
}

// TestStartOAuthLogin_RedirectURIFollowsMount is the regression guard for
// social sign-in under --base-path: the redirect_uri handed to Cognito has to
// name a route this server actually registered. Hardcoding /api/v1/... sent the
// browser back to a 404 on any base-path deployment.
func TestStartOAuthLogin_RedirectURIFollowsMount(t *testing.T) {
	for _, tc := range []struct {
		route string
		want  string
	}{
		{"/api/v1/auth/google/login", "http://example.com/api/v1/auth/google/callback"},
		{"/ycp/api/v1/auth/google/login", "http://example.com/ycp/api/v1/auth/google/callback"},
	} {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, tc.route, nil)
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath(tc.route)

		if err := startOAuthLogin(c, "auth.example.com", "client123", googleProvider); err != nil {
			t.Fatalf("startOAuthLogin: %v", err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parsing Location: %v", err)
		}
		if got := loc.Query().Get("redirect_uri"); got != tc.want {
			t.Errorf("route %q: redirect_uri = %q, want %q", tc.route, got, tc.want)
		}
	}
}
