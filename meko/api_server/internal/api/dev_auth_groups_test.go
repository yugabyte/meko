package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func tag(name string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Add("X-Auth", name)
			return next(c)
		}
	}
}

func authOf(t *testing.T, e *echo.Echo, path string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Header().Values("X-Auth")
}

// With PATs accepted, only dpDirect may resolve a PAT; the JWT-only group
// (PAT management, deployments) must keep the dev account.
func TestDevAuthGroups_PATsReachOnlyTheDirectGroup(t *testing.T) {
	e := echo.New()
	v1 := e.Group("/api/v1")
	jwtOnly, direct := devAuthGroups(v1, tag("dev"), tag("pat"), tag("rate"))
	ok := func(c echo.Context) error { return c.NoContent(http.StatusOK) }
	jwtOnly.GET("/me/meko-tokens", ok)
	direct.GET("/datapacks", ok)

	if got := authOf(t, e, "/api/v1/me/meko-tokens"); len(got) != 1 || got[0] != "dev" {
		t.Errorf("JWT-only route ran %v, want [dev]", got)
	}
	if got := authOf(t, e, "/api/v1/datapacks"); len(got) != 2 || got[0] != "pat" || got[1] != "rate" {
		t.Errorf("direct route ran %v, want [pat rate]", got)
	}
}

func TestDevAuthGroups_WithoutPATsBothGroupsAreDevAuth(t *testing.T) {
	e := echo.New()
	jwtOnly, direct := devAuthGroups(e.Group("/api/v1"), tag("dev"), nil, tag("rate"))
	if jwtOnly != direct {
		t.Fatal("without PAT auth the two groups should be the same group")
	}
}
