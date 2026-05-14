package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/api"
	"meko/api_server/internal/auth"
)

// stamp is a tiny middleware that injects a pat_id into the context
// so PerPATRateLimiter has something to key on without booting the
// full auth dispatch.
func stampPATID(patID string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyPATID, patID)
			return next(c)
		}
	}
}

func TestPerPATRateLimiter_NoOpWithoutPATID(t *testing.T) {
	e := echo.New()
	// Burst=1, perMin=60: would 429 immediately on the second hit
	// IF a pat_id were set. Without it, the limiter is a no-op.
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		api.PerPATRateLimiter(60, 1),
	)

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d", i, rec.Code)
		}
	}
}

func TestPerPATRateLimiter_DisabledWhenPerMinZero(t *testing.T) {
	e := echo.New()
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		stampPATID("pat-1"),
		api.PerPATRateLimiter(0, 1),
	)

	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200 on disabled limiter, got %d", i, rec.Code)
		}
	}
}

func TestPerPATRateLimiter_429AfterBurst(t *testing.T) {
	e := echo.New()
	// Burst=2, very low refill rate (1/min) so we'll exhaust it.
	e.GET("/x",
		func(c echo.Context) error { return c.NoContent(http.StatusOK) },
		stampPATID("pat-1"),
		api.PerPATRateLimiter(1, 2),
	)

	// First two should pass (burst budget = 2).
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("burst call %d: expected 200, got %d", i, rec.Code)
		}
	}
	// Third should 429.
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst: expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 429, got none")
	}
}

func TestPerPATRateLimiter_PerPATIsolation(t *testing.T) {
	mw := api.PerPATRateLimiter(1, 1) // burst 1 per pat
	e := echo.New()
	mountFor := func(patID, path string) {
		e.GET(path,
			func(c echo.Context) error { return c.NoContent(http.StatusOK) },
			stampPATID(patID),
			mw,
		)
	}
	mountFor("pat-1", "/a")
	mountFor("pat-2", "/b")

	// pat-1: first ok, second 429.
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pat-1 first: expected 200, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("pat-1 second: expected 429, got %d", rec.Code)
	}
	// pat-2 should still have its full burst available -- the
	// limiter is keyed per-pat, not global.
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pat-2 first: expected 200 (separate bucket), got %d", rec.Code)
	}
}
